package engine

import (
	"slices"
	"strings"
)

// A seek that skips a row C reads, or reads a row C skips, changes no answer:
// every conjunct stays in the WHERE and is tested on every row the seek hands
// over. It can change whether the statement FAILS. C tests a row's terms in
// one order (sqlite3WhereCodeOneLoopStart: the terms its index consumed, then
// the rest in WHERE order, correlated sub-selects last) and stops at the first
// false one, so a term that can raise is raised on exactly the rows that get
// that far. Seeking key k skips the rows where k is false; that is exact when
// C never reaches a term that can raise on them -- because C consumes k with
// its own index, or because no such term comes before k in C's order.
//
//	SELECT 1 FROM i WHERE json_extract(i.j, '$') < o.l AND i.value = o.wanted
//
// C scans i (no index on value) and fails on a row of malformed JSON whose
// value is not o.wanted; a seek on value never read that row.

// raiseSafeFuncs are the scalar functions whose implementation has no error
// past the argument count, which the compiler checks.
var raiseSafeFuncs = map[string]bool{
	"coalesce": true, "ifnull": true, "nullif": true, "iif": true, "typeof": true,
	"length": true, "octet_length": true, "lower": true, "upper": true,
	"likely": true, "unlikely": true, "min": true, "max": true, "instr": true,
	"substr": true, "substring": true, "trim": true, "ltrim": true, "rtrim": true,
	"unicode": true, "sign": true,
}

// likeSafePatternMax is below the length at which a LIKE or GLOB pattern is
// "too complex" (SQLITE_MAX_LIKE_PATTERN_LENGTH).
const likeSafePatternMax = 50000

// exprMayRaise reports whether evaluating e can raise an error at run time.
// Anything it does not know can.
func exprMayRaise(e Expr) bool {
	switch x := e.(type) {
	case nil, ParamExpr, ColumnExpr:
		return false
	case LiteralExpr:
		return x.deferredErr != ""
	case UnaryExpr:
		switch x.Op {
		case "-", "+", "NOT", "~":
			return exprMayRaise(x.X)
		}
		return true
	case BinaryExpr:
		switch x.Op {
		case "=", "==", "!=", "<>", "<", "<=", ">", ">=", "IS", "IS NOT",
			"AND", "OR", "+", "-", "*", "/", "%", "&", "|", "<<", ">>", "||":
			return exprMayRaise(x.L) || exprMayRaise(x.R)
		}
		return true
	case IsNullExpr:
		return exprMayRaise(x.X)
	case CastExpr:
		return exprMayRaise(x.X)
	case CollateExpr:
		return exprMayRaise(x.X)
	case BetweenExpr:
		return exprMayRaise(x.X) || exprMayRaise(x.Lo) || exprMayRaise(x.Hi)
	case InExpr:
		if x.Sub != nil || exprMayRaise(x.X) {
			return true
		}
		for _, v := range x.List {
			if exprMayRaise(v) {
				return true
			}
		}
		return false
	case RowExpr:
		for _, v := range x.Elems {
			if exprMayRaise(v) {
				return true
			}
		}
		return false
	case CaseExpr:
		if exprMayRaise(x.Base) || exprMayRaise(x.Else) {
			return true
		}
		for _, w := range x.Whens {
			if exprMayRaise(w.When) || exprMayRaise(w.Then) {
				return true
			}
		}
		return false
	case LikeExpr:
		return x.Escape != nil || !likeSafePattern(x.Pattern) || exprMayRaise(x.X)
	case GlobExpr:
		return !likeSafePattern(x.Pattern) || exprMayRaise(x.X)
	case FuncExpr:
		if !raiseSafeFuncs[strings.ToLower(x.Name)] || x.Over != nil || x.Filter != nil || x.Star || x.Distinct {
			return true
		}
		for _, a := range x.Args {
			if exprMayRaise(a) {
				return true
			}
		}
		return false
	}
	return true
}

// likeSafePattern reports a pattern too short to be "too complex".
func likeSafePattern(e Expr) bool {
	lit, ok := e.(LiteralExpr)
	return ok && lit.deferredErr == "" && (lit.Val.Typ == Text || lit.Val.Typ == Blob) && len(lit.Val.S) < likeSafePatternMax
}

// seekGuard decides which seek keys of one source keep its rows exact. raiseAt
// is the WHERE position of the first conjunct that the source's loop tests
// per row and that can raise, tableRaiseAt the first such that reads no other
// table -- what an automatic index tests on every row as it builds
// (constructAutomaticIndex's partial condition) -- and access is C's loop.
type seekGuard struct {
	raiseAt, tableRaiseAt int
	access                wherePlanAccess
}

// holdsCorrelatedSelect reports whether e holds a sub-select a name of which
// resolves outside it: C's EP_VarSelect, which marks the term TERM_VARSELECT
// and has wherecode.c test it after every other term. A sub-select that names
// nothing outside itself is coded in place, like any other term.
//
// A name or FROM item the walk cannot resolve exactly leaves the term counted
// as tested in place, as does a correlated sub-select nested in one that is
// not (C marks the term for it too, bVarSelect): the seek is then declined.
func (c *compiler) holdsCorrelatedSelect(e Expr) bool {
	if c == nil || c.pager == nil || !containsSubquery(e) {
		return false
	}
	inTrigger := enclosingTriggerCtx(c) != nil
	correlated := false
	wherePlanSubqueryNames(c.pager, e, func(ce ColumnExpr) bool {
		// Not an enclosing query's: a bare TRUE, FALSE or double-quoted string
		// that names no column is a literal, and a trigger's NEW and OLD are
		// found before lookupName climbs.
		switch {
		case ce.FallbackLiteral != nil:
		case inTrigger && ce.Schema == "" && (equalFoldName(ce.Qualifier, "new") || equalFoldName(ce.Qualifier, "old")):
		default:
			correlated = true
		}
		return true
	})
	return correlated
}

// newSeekGuard reads where, split into its top-level conjuncts, for source i.
// A conjunct holding a correlated sub-select is left out: C tests it after
// every other term (TERM_VARSELECT), so after the key too.
func newSeekGuard(c *compiler, srcs []joinSource, i int, cjs []Expr) seekGuard {
	g := seekGuard{raiseAt: -1, tableRaiseAt: -1}
	scopes := tableScopesOf(srcs)
	for pos, cj := range cjs {
		if !exprMayRaise(cj) || c.holdsCorrelatedSelect(cj) {
			continue
		}
		if hoist, _ := constWhereTerm(cj, scopes, false); hoist {
			continue // coded once, before the loops
		}
		refs := map[int]bool{}
		collectTableRefs(cj, scopes, refs)
		if !refs[i] && (len(refs) > 0 || len(srcs) > 1) {
			continue // tested at another table's level
		}
		if g.raiseAt < 0 {
			g.raiseAt = pos
		}
		if g.tableRaiseAt < 0 && len(refs) == 1 && !columnOutsideFrom(cj, scopes) {
			g.tableRaiseAt = pos
		}
	}
	if g.raiseAt >= 0 {
		g.access = seekAccessOf(c, srcs, i)
	}
	return g
}

// columnOutsideFrom reports whether e reads a column of an enclosing query.
func columnOutsideFrom(e Expr, scopes []tableScope) bool {
	found := false
	forEachColumnRef(e, func(ce ColumnExpr) {
		if columnIsOuterOnly(ce, scopes) {
			found = true
		}
	})
	return found
}

// keep reports whether a seek on table column col (xnRowid for the rowid),
// from the conjunct at WHERE position pos, keeps the rows exact. real says a
// declared index leads on col.
func (g seekGuard) keep(col, pos int, real bool) bool {
	if g.raiseAt < 0 {
		return true
	}
	a := g.access
	switch {
	case !a.known:
		// C's loop is not known here. A declared index C is taken to walk;
		// a column with none it scans.
		return real || pos < g.raiseAt
	case a.auto:
		// The automatic index tests every term reading only this table on
		// every row while it builds, in WHERE order, stopping at a false one.
		return containsInt(a.eq, col) && (g.tableRaiseAt < 0 || pos < g.tableRaiseAt)
	case containsInt(a.eq, col):
		return true
	case a.index:
		// An index on other columns: the terms it covers come first in C's
		// order, whatever their place in the WHERE.
		return false
	}
	return pos < g.raiseAt
}

// keepRange is keep for a range bound on the rowid, which C consumes when its
// loop is that range, and which every index holds: under an index on other
// columns C tests the bound with the terms that index covers, ahead of a term
// it does not.
func (g seekGuard) keepRange(col, pos int) bool {
	a := g.access
	switch {
	case g.raiseAt < 0, !a.known, a.hasRange && a.rangeCol == col, a.index:
		return true
	case a.auto:
		return g.tableRaiseAt < 0 || pos < g.tableRaiseAt
	}
	return pos < g.raiseAt
}

// consumedFirst moves to the front of a one-table level's terms the
// equalities C's loop consumes -- its index's equality prefix -- when a term
// that can raise is among them. C never tests a term on a row its index does
// not hold, and a row read here that C does not read -- the seek declined to
// a scan, or answered from a key the index does not lead with -- must fail one
// of those equalities before it reaches the raising term. Not for an automatic
// index, which tests the terms reading only its table on every row as it
// builds, ahead of its key.
func consumedFirst(c *compiler, srcs []joinSource, terms []Expr) []Expr {
	if len(srcs) != 1 || len(terms) < 2 {
		return terms
	}
	if !slices.ContainsFunc(terms, exprMayRaise) {
		return terms
	}
	a := seekAccessOf(c, srcs, 0)
	if !a.known || a.auto || len(a.eq) == 0 {
		return terms
	}
	s := srcs[0]
	first := make([]Expr, 0, len(a.eq))
	taken := make([]bool, len(terms))
	for _, col := range a.eq {
		for k, cj := range terms {
			if !taken[k] && equalityOnColumn(c, s, cj, col) {
				taken[k] = true
				first = append(first, cj)
				break
			}
		}
	}
	if len(first) == 0 {
		return terms
	}
	out := first
	for k, cj := range terms {
		if !taken[k] {
			out = append(out, cj)
		}
	}
	return out
}

// equalityOnColumn reports whether cj is "col = key", either way round, with
// a key a seek can probe for: what C's index consumes for col.
func equalityOnColumn(c *compiler, s joinSource, cj Expr, col int) bool {
	be, ok := cj.(BinaryExpr)
	if !ok || be.Op != "=" && be.Op != "==" && be.Op != "IS" {
		return false
	}
	for _, side := range [2][2]Expr{{be.L, be.R}, {be.R, be.L}} {
		if !isSeekKeyCandidate(side[1]) && !isOuterSeekKey(c, side[1]) {
			continue
		}
		if col == xnRowid && isScopeRowidRef(c, s, side[0]) {
			return true
		}
		if got, ok := scopeColumnIndex(c, s, side[0]); ok && got == col {
			return true
		}
	}
	return false
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// seekAccessOf is C's loop for source i, as the ported planner decided it: an
// EXISTS existsToJoin turned into a join takes its level of that join, one
// table its single-table plan, and a join its level's key.
func seekAccessOf(c *compiler, srcs []joinSource, i int) wherePlanAccess {
	if o := c.outer; len(srcs) == 1 && o != nil && o.existsJoin && o.existsJoinPlan != nil && o.existsAt != nil {
		if lvl := o.existsAt.level; lvl >= 0 && lvl < len(o.existsJoinPlan.keys) {
			return accessOfKey(o.existsJoinPlan.keys[lvl])
		}
		return wherePlanAccess{}
	}
	if len(srcs) == 1 {
		return srcs[0].planAccess
	}
	if srcs[i].wherePlanOK && srcs[i].idxOrderKey != nil {
		return accessOfKey(srcs[i].idxOrderKey)
	}
	return wherePlanAccess{}
}

// accessOfKey is the access path a level's key says C walks: nil or a key of
// no columns the rowid scan, an automatic index its constrained columns, a
// declared index its equality prefix when known.
func accessOfKey(k *autoIndexKey) wherePlanAccess {
	switch {
	case k == nil || len(k.cols) == 0:
		return wherePlanAccess{known: true}
	case k.multiOr != nil:
		return wherePlanAccess{}
	case k.autoEq > 0:
		return wherePlanAccess{known: true, auto: true, eq: k.cols[:k.autoEq]}
	case k.root != 0 || k.index != "":
		if k.nEq > 0 {
			return wherePlanAccess{known: true, index: true, eq: k.cols[:k.nEq]}
		}
		return wherePlanAccess{}
	case len(k.cols) == 1 && k.cols[0] == xnRowid:
		return wherePlanAccess{known: true} // the rowid scan, reversed
	}
	return wherePlanAccess{}
}
