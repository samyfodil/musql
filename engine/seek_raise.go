package engine

import (
	"slices"
	"strings"
)

// A seek never changes an answer, since every conjunct is still tested, but it
// can change whether a statement fails. C tests a row's terms in one order
// (sqlite3WhereCodeOneLoopStart: its index's terms, then the rest in WHERE
// order, correlated sub-selects last) and stops at the first false one.
// Seeking key k skips the rows where k is false, which is exact only when C
// never reaches a raising term on them: C consumes k with its own index, or no
// raising term comes before k.

// raiseSafeFuncs have no run-time error past the argument count the compiler
// checks.
var raiseSafeFuncs = map[string]bool{
	"coalesce": true, "ifnull": true, "nullif": true, "iif": true, "typeof": true,
	"length": true, "octet_length": true, "lower": true, "upper": true,
	"likely": true, "unlikely": true, "min": true, "max": true, "instr": true,
	"substr": true, "substring": true, "trim": true, "ltrim": true, "rtrim": true,
	"unicode": true, "sign": true,
}

// likeSafePatternMax is below SQLITE_MAX_LIKE_PATTERN_LENGTH.
const likeSafePatternMax = 50000

// exprMayRaise reports whether e can raise at run time; anything it does not
// know can.
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

// termMayRaise also counts a read of a VIRTUAL generated column, which C
// computes where it is read (sqlite3ExprCodeGeneratedColumn).
func termMayRaise(e Expr, scopes []tableScope) bool {
	if exprMayRaise(e) {
		return true
	}
	virtual := false
	forEachColumnRef(e, func(ce ColumnExpr) {
		for _, ts := range scopes {
			if ce.Qualifier != "" && !equalFoldName(ts.name, ce.Qualifier) {
				continue
			}
			if k, ok := ts.colIndex[r33sFoldIdent(ce.Name)]; ok && k < len(ts.cols) && ts.cols[k].IsGenerated() && !ts.cols[k].GeneratedStored {
				virtual = true
			}
		}
	})
	return virtual
}

// seekGuard decides which seek keys keep one source's rows exact. raiseAt is
// the WHERE position of the first per-row conjunct that can raise,
// tableRaiseAt the first that reads no other table, which an automatic index
// tests on every row as it builds (constructAutomaticIndex).
type seekGuard struct {
	raiseAt, tableRaiseAt int
	access                wherePlanAccess
}

// holdsCorrelatedSelect reports a sub-select naming something outside itself
// (C's EP_VarSelect): C tests such a term after every other (TERM_VARSELECT).
// A name the walk cannot resolve counts as tested in place, which declines the
// seek.
func (c *compiler) holdsCorrelatedSelect(e Expr) bool {
	if c == nil || c.pager == nil || !containsSubquery(e) {
		return false
	}
	inTrigger := enclosingTriggerCtx(c) != nil
	correlated := false
	wherePlanSubqueryNames(c.pager, e, func(ce ColumnExpr) bool {
		// A bare TRUE, FALSE or double-quoted string naming no column is a
		// literal, and a trigger's NEW and OLD resolve before lookupName climbs.
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

// newSeekGuard reads where's top-level conjuncts for source i, leaving out
// correlated sub-selects, which C tests after the key.
func newSeekGuard(c *compiler, srcs []joinSource, i int, cjs []Expr) seekGuard {
	g := seekGuard{raiseAt: -1, tableRaiseAt: -1}
	scopes := tableScopesOf(srcs)
	for pos, cj := range cjs {
		if !termMayRaise(cj, scopes) || c.holdsCorrelatedSelect(cj) {
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

// keep reports whether a seek on col (xnRowid for the rowid) from the
// conjunct at pos keeps the rows exact; real says a declared index leads on
// col.
func (g seekGuard) keep(col, pos int, real bool) bool {
	if g.raiseAt < 0 {
		return true
	}
	a := g.access
	switch {
	case !a.known:
		// C's loop is unknown: a declared index is taken to be walked, a column
		// without one scanned.
		return real || pos < g.raiseAt
	case a.auto:
		// The automatic index tests this table's terms on every row as it
		// builds, in WHERE order.
		return containsInt(a.eq, col) && (g.tableRaiseAt < 0 || pos < g.tableRaiseAt)
	case containsInt(a.eq, col):
		return true
	case a.index:
		// An index on other columns: C tests the terms it covers first.
		return false
	}
	return pos < g.raiseAt
}

// keepRange is keep for a rowid range bound, which C consumes when its loop is
// that range, and which an index on other columns tests with its own terms.
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

// consumedFirst moves the equalities C's index consumes to the front of a
// one-table level's terms when a raising term is among them: C never tests a
// term on a row its index skips, so a row read here that C skips must fail one
// of those equalities first. Not for an automatic index, which tests its
// table's terms on every row as it builds.
func consumedFirst(c *compiler, srcs []joinSource, terms []Expr) []Expr {
	if len(srcs) != 1 || len(terms) < 2 {
		return terms
	}
	scopes := tableScopesOf(srcs)
	if !slices.ContainsFunc(terms, func(e Expr) bool { return termMayRaise(e, scopes) }) {
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

// equalityOnColumn reports whether cj is "col = key", either way round, with a
// key a seek can probe.
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

// seekAccessOf is C's loop for source i, as the ported planner chose it.
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

// accessOfKey is the access path C walks for a level's key.
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
