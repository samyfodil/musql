// This file implements the run-time anchor certificate: a mechanism that lets
// aggregate queries with bare columns answer when scan order is unknown. A
// bare column reads the group's anchor (first) row. The certificate validates
// at run time that all rows of a group agree on the values the items read; if
// they do, any row serves as the anchor. If they disagree, the query declines.
// This trades prepare-time certainty for run-time validation, like orderRiskErr.
package engine

import "regexp"

// anchorCheck names the anchor-row slots an aggregate query's items read:
// column indices and per-scope rowid slots, and whether the GROUP BY key is read.
type anchorCheck struct {
	cols   []int
	rowids []int

	// key: true if groupKeyExpr is read. Rows of one group can hold different
	// spellings of an equal key (1 vs 1.0, 'a' vs 'A' under NOCASE), so key
	// values are certified via OpGroupSame.
	key bool
}

// anchorCheckFor collects slots from the rewritten item trees. It fails closed:
// returns false for any expression kind it doesn't recognize, subqueries,
// window functions, aggregates, or FILTERed calls. Failing keeps the prior decline.
func anchorCheckFor(items []*itemPlan) (*anchorCheck, bool) {
	chk := &anchorCheck{}
	seenCol := map[int]bool{}
	seenRowid := map[int]bool{}
	var walk func(e Expr) bool
	walkAll := func(es []Expr) bool {
		for _, e := range es {
			if !walk(e) {
				return false
			}
		}
		return true
	}
	walk = func(e Expr) bool {
		switch x := e.(type) {
		case nil, LiteralExpr, ParamExpr, groupAggExpr:
			return true
		case groupKeyExpr:
			chk.key = true
			return true
		case groupBareColExpr:
			if x.isRowid {
				if x.rowidTableIdx < 0 {
					return false
				}
				if !seenRowid[x.rowidTableIdx] {
					seenRowid[x.rowidTableIdx] = true
					chk.rowids = append(chk.rowids, x.rowidTableIdx)
				}
				return true
			}
			if x.idx < 0 {
				return false
			}
			if !seenCol[x.idx] {
				seenCol[x.idx] = true
				chk.cols = append(chk.cols, x.idx)
			}
			return true
		case UnaryExpr:
			return walk(x.X)
		case IsNullExpr:
			return walk(x.X)
		case CollateExpr:
			return walk(x.X)
		case CastExpr:
			return walk(x.X)
		case BinaryExpr:
			return walk(x.L) && walk(x.R)
		case BetweenExpr:
			return walk(x.X) && walk(x.Lo) && walk(x.Hi)
		case LikeExpr:
			return walk(x.X) && walk(x.Pattern) && walk(x.Escape)
		case GlobExpr:
			return walk(x.X) && walk(x.Pattern)
		case RowExpr:
			return walkAll(x.Elems)
		case InExpr:
			return x.Sub == nil && walk(x.X) && walkAll(x.List)
		case CaseExpr:
			if !walk(x.Base) || !walk(x.Else) {
				return false
			}
			for _, w := range x.Whens {
				if !walk(w.When) || !walk(w.Then) {
					return false
				}
			}
			return true
		case FuncExpr:
			return x.Over == nil && !x.Star && x.Filter == nil && len(x.OrderBy) == 0 && !isAggregateCall(x) && walkAll(x.Args)
		}
		return false
	}
	for _, it := range items {
		if it == nil {
			continue
		}
		if it.bareWide || !walk(it.rewritten) {
			return nil, false
		}
	}
	return chk, true
}

// certifyAnchor runs once per stepped row (magnetWalk): the group's first row
// records the named slots, and every later row that differs on one latches
// anchorRisk, which aggResult turns into the decline.
func (a *aggAccumulators) certifyAnchor(ctx *evalCtx) {
	k := a.check
	if k == nil || a.anchorRisk {
		return
	}
	at := func(vs []Value, i int) Value {
		if i >= 0 && i < len(vs) {
			return vs[i]
		}
		return Value{}
	}
	if !a.checkSeen {
		a.checkVals = a.checkVals[:0]
		for _, i := range k.cols {
			a.checkVals = append(a.checkVals, copyValue(at(ctx.vals, i)))
		}
		for _, i := range k.rowids {
			a.checkVals = append(a.checkVals, copyValue(at(ctx.rowids, i)))
		}
		a.checkSeen = true
		return
	}
	n := 0
	for _, i := range k.cols {
		if !anchorValuesIdentical(at(ctx.vals, i), a.checkVals[n]) {
			a.anchorRisk = true
			return
		}
		n++
	}
	for _, i := range k.rowids {
		if !anchorValuesIdentical(at(ctx.rowids, i), a.checkVals[n]) {
			a.anchorRisk = true
			return
		}
		n++
	}
}

// anchorValuesIdentical is valuesIdentical plus the function subtype, which a
// whole-table aggregate's anchor row keeps (clearJSONSubtype strips it only for
// grouped rows).
func anchorValuesIdentical(a, b Value) bool {
	return a.Subtype == b.Subtype && valuesIdentical(a, b)
}

// anchorKeysIdentical reports whether two GROUP BY key records spell the key
// identically, not merely compare equal.
func anchorKeysIdentical(a, b []Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !anchorValuesIdentical(a[i], b[i]) {
			return false
		}
	}
	return true
}

// groupEmissionAscendingEverywhere reports whether a single GROUP BY term's
// groups come out in ascending key order under EVERY plan sqlite3WhereBegin
// can choose. wherePathSatisfiesOrderBy (where.c:5147) delivers a GROUP BY
// either through a sorter, which emits keys ascending, or through a loop whose
// index walks the key; for WHERE_GROUPBY the walk keeps its own direction
// (where.c:5356-5363), so only a DESC key column reverses it. The other two
// ways to inherit an order are a virtual table consuming it
// (where.c:5225-5233) and a subquery's own ORDER BY being matched
// (wherePathMatchSubqueryOB, where.c:5275-5281).
//
// ponytail: a whole-schema word search for DESC, VIRTUAL and ORDER. It only
// over-declines (a column named "desc", an ORDER BY in an unrelated view);
// reading each index's key directions through wherePlanIndexList would narrow
// it if a real workload hits that.
func groupEmissionAscendingEverywhere(p *ReadOnlyPager, srcs []joinSource, nGroup int) bool {
	if p == nil || nGroup != 1 || p.ReverseUnorderedSelects() {
		return false
	}
	for _, s := range srcs {
		if s.vtabItem != nil || s.cteItem != nil || s.catalogScope != scopeAny {
			return false
		}
	}
	for dbIdx := 0; dbIdx <= len(p.attachedReaders); dbIdx++ {
		owner := p.forDB(dbIdx)
		if owner == nil {
			return false
		}
		rows, err := owner.Schema()
		if err != nil {
			return false
		}
		for i := range rows {
			if groupEmissionOrderWordRe.MatchString(rows[i].SQL) {
				return false
			}
		}
	}
	return true
}

var groupEmissionOrderWordRe = regexp.MustCompile(`(?i)\b(desc|virtual|order)\b`)
