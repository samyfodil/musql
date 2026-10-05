// MATCH placement: which "X MATCH q" nodes C's planner turns into a
// virtual-table constraint, and what happens to the rest.
//
// This engine evaluates MATCH as a per-row predicate, which is right wherever C
// consumes the constraint and wrong everywhere else, because there C fails:
//
//	CREATE VIRTUAL TABLE t USING fts5(a, b);
//	INSERT INTO t(rowid,a) VALUES(1,'alpha'),(2,'beta');
//	SELECT rowid FROM t WHERE NOT (t MATCH 'alpha');
//	  this engine: rowid 2   cgo: unable to use function MATCH ...
//
// "match" is an SQL function only so names resolve; its body is
// sqlite3InvalidFunction (main.c:2210), "unable to use function %s in the
// requested context" (main.c:2220). The planner lifts it into a constraint in
// one place, exprAnalyze (whereexpr.c:1533):
//
//	else if( pWC->op==TK_AND ){                       /* a WHERE/ON conjunct */
//	  int res = isAuxiliaryVtabOperator(db, pExpr, &eOp2, &pLeft, &pRight);
//	  ...  if( (prereqExpr & prereqColumn)==0 ){      /* whereexpr.c:1543  */
//	         ... pNewTerm->eOperator = WO_AUX;
//
// and isAuxiliaryVtabOperator (whereexpr.c:402, 449) needs the left operand to
// be a vtab column (so "+b MATCH ..." is not it). An unlifted MATCH is coded
// into the row test and raises at run time, per row -- so over an empty table
// or behind a short-circuited AND, C answers normally. compileFts5Match
// reproduces that by compiling an unusable MATCH to an OpMatch whose target
// cannot resolve (fts5UnusableMatch).
//
// Three more rules decide whether a lifted MATCH survives:
//
//   - fts5BestIndexMethod (fts5_main.c:657) rejects a plan with an unusable
//     MATCH constraint; with no usable order, "no query solution"
//     (where.c:6169). OUTER and CROSS joins fix the order (where.c:5010), so a
//     MATCH whose query reads a later table can never be satisfied.
//   - disableTerm (wherecode.c:419) keeps a consumed WHERE term at a LEFT JOIN
//     level unless it is EP_OuterON, so a WHERE MATCH on an outer join's right
//     table raises while the same MATCH in its ON clause does not.
//   - two fts5 tables whose queries read each other's columns have no legal
//     order: "no query solution".
//
// fts5MatchBindings is called from vdbe_scan.go beside fts3MatchBindings, and
// compileFts5Match is compileMatchExpr's fts5 tail. The gate is
// compat-harness/fts5_r39d_placement_test.go.
package engine

import "fmt"

// fts5MatchBindings computes which of scopes (this compile's FROM-item scopes,
// keyed by index, as in fts3MatchBindings, since each caller builds its own
// []tableScope copy) may serve an fts5 MATCH as a per-row test -- those C's
// planner hands a usable constraint.
//
// A scope is bound when some top-level AND conjunct of the WHERE or of an ON
// clause is built purely from MATCH leaves against that scope (AND/OR only --
// fts5BestIndexMethod's nSeenMatch loop consumes them all), and no other MATCH
// against it appears elsewhere. Unlike fts3, several such conjuncts are fine:
// fts5 intersects them.
//
// The "nowhere else" half makes a per-scope verdict safe for a per-node
// question: a scope with both a good and a bad placement is refused outright.
// That over-refuses "t MATCH 'nohit' AND NOT (t MATCH 'beta')", which C
// answers with no rows -- a missing answer, never a wrong one. An unknown
// expression node voids the whole result.
func fts5MatchBindings(p *ReadOnlyPager, stmt *SelectStmt, scopes []tableScope) *fts5MatchPlacement {
	ctx := &evalCtx{tables: scopes, pager: p}
	hasSlot := map[int]bool{}
	stray := map[int]bool{}
	hard := map[int]bool{}
	deps := map[int]map[int]bool{}
	known := true

	// An OUTER or CROSS join fixes the FROM order (where.c:5010) and a LEFT
	// JOIN level re-evaluates even a consumed WHERE term (wherecode.c:419).
	outerRight, barrier := fts5OuterJoinScopes(stmt, scopes)

	poison := func(e Expr) {
		if !fts5WalkMatchExpr(e, func(m MatchExpr) {
			if idx, ok := fts5MatchScopeIndex(ctx, m.X); ok {
				stray[idx] = true
			}
		}, nil) {
			known = false
		}
	}

	// classify walks one top-level conjunct against the scope its first MATCH
	// resolves to:
	//
	//	ok       - every MatchExpr in the tree is a constraint the planner
	//	           lifts out for THIS scope (whereexpr.c:1533/1543);
	//	consumed - the tree yields at least one constraint fts5's xBestIndex
	//	           actually takes for it.
	//
	// consumed makes an OR usable: whereLoopAddOr (where.c:4814) keeps the
	// OR-scan only if every branch has an index probe, so "t MATCH 'a' OR rowid=2"
	// is two probes while "t MATCH 'a' OR b='two'" falls back to a full scan that
	// evaluates (and raises on) the MATCH. Only a MATCH (cost 50000) and a rowid
	// equality (cost 25, bSeenEq) are recognized; a rowid range (2250000, against
	// a 3000000 scan) is a cost question not modelled, so "t MATCH 'a' OR rowid>1"
	// is refused though C answers it.
	var classify func(e Expr, s int) (consumed, ok bool, refs map[int]bool)
	classify = func(e Expr, s int) (bool, bool, map[int]bool) {
		switch x := e.(type) {
		case MatchExpr:
			if x.Not {
				// "X NOT MATCH Y" parses in C to NOT(X MATCH Y): the likeop rule
				// flags the token (parse.y:1362) and then wraps the call --
				//
				//	if( bNot ) A = sqlite3PExpr(pParse, TK_NOT, A, 0);
				//	                                       parse.y:1370
				//
				// -- which exprAnalyze never lifts. This engine keeps one node with a
				// flag, so it must be refused here: "WHERE ft NOT MATCH 'beta'" is
				// "unable to use function MATCH in the requested context" in C (fts5
				// and fts4 alike).
				return false, false, nil
			}
			idx, iok := fts5MatchScopeIndex(ctx, x.X)
			if !iok || idx != s {
				return false, false, nil
			}
			refs, allResolved, kk := fts5ExprScopes(ctx, x.Pattern)
			if !kk {
				known = false
			}
			if !kk || !allResolved {
				return false, false, nil
			}
			// whereexpr.c:1543's "(prereqExpr & prereqColumn)==0": a query
			// string that reads the MATCHed table's own columns is never
			// lifted out at all ("t MATCH t.a", "a MATCH b" -> raises).
			if refs[idx] {
				return false, false, nil
			}
			return true, true, refs
		case BinaryExpr:
			if x.Op == "AND" || x.Op == "OR" {
				lc, lok, lrefs := classify(x.L, s)
				rc, rok, rrefs := classify(x.R, s)
				if lrefs == nil {
					lrefs = map[int]bool{}
				}
				for k := range rrefs {
					lrefs[k] = true
				}
				if x.Op == "OR" {
					return lc && rc, lok && rok, lrefs
				}
				return lc || rc, lok && rok, lrefs
			}
		}
		// Any other node: usable only if it hides no MatchExpr at all, and
		// consumable only in the one non-MATCH shape fts5 takes for free.
		clean := true
		if !fts5WalkMatchExpr(e, func(MatchExpr) { clean = false }, nil) {
			known = false
			return false, false, nil
		}
		return fts5RowidEq(ctx, e, s), clean, nil
	}

	slotScope := func(e Expr) (int, map[int]bool, bool) {
		s, found := fts5FirstMatchScope(ctx, e, &known)
		if !found {
			return -1, nil, false
		}
		consumed, ok, refs := classify(e, s)
		if !consumed || !ok {
			return -1, nil, false
		}
		return s, refs, true
	}

	// walkConjunct records one top-level conjunct. owner is -1 for a conjunct
	// of stmt.Where or of an INNER/CROSS join's ON clause (both become plain
	// WhereTerms, so they may constrain any table), or the scope index of the
	// item an OUTER join's ON clause belongs to (its terms carry EP_OuterON
	// and are usable at THAT level only).
	walkConjunct := func(cj Expr, owner int) {
		idx, refs, ok := slotScope(cj)
		if ok {
			switch {
			case owner >= 0 && idx != owner:
				// fts5leftjoin.test 2.2/3.1: an outer join's ON clause
				// MATCHing an EARLIER table is a constraint the planner DID
				// form and can never make usable -- and, unlike a placement
				// refusal, that is decided while PREPARING: C SQLite says
				// "no query solution" whether or not any row is ever tested
				// (verified with the joined table both empty and non-empty).
				hard[idx] = true
			case barrier && len(refs) > 0:
				// The order is fixed, so a query string read from another
				// table may or may not be ready when this table's loop opens,
				// and this pass does not model the order. Same prepare-time
				// outcome as above.
				hard[idx] = true
			case owner < 0 && outerRight[idx]:
				// disableTerm (wherecode.c:419): the module consumes the
				// constraint and the LEFT JOIN level evaluates the term
				// ANYWAY, so this one raises per row like a bad placement.
			default:
				hasSlot[idx] = true
				if deps[idx] == nil {
					deps[idx] = map[int]bool{}
				}
				for k := range refs {
					deps[idx][k] = true
				}
				return
			}
		}
		poison(cj)
	}

	for _, cj := range splitTopLevelAnd(stmt.Where) {
		walkConjunct(cj, -1)
	}
	for i := range stmt.From {
		it := &stmt.From[i]
		if it.On == nil {
			continue
		}
		owner := -1
		if it.Join == JoinLeft || it.Join == JoinRight || it.Join == JoinFull {
			owner = fts5ScopeOfItem(it, scopes)
			if owner < 0 {
				// A derived table, or a name this pass cannot line up with a
				// scope: no slot of this ON clause is classifiable.
				for _, cj := range splitTopLevelAnd(it.On) {
					poison(cj)
				}
				continue
			}
		}
		for _, cj := range splitTopLevelAnd(it.On) {
			walkConjunct(cj, owner)
		}
	}
	for _, sc := range stmt.Columns {
		poison(sc.Expr)
	}
	for _, g := range stmt.GroupBy {
		poison(g)
	}
	poison(stmt.Having)
	for _, o := range stmt.OrderBy {
		poison(o.Expr)
	}
	for _, w := range stmt.Windows {
		if w.Spec == nil {
			continue
		}
		for _, e := range w.Spec.PartitionBy {
			poison(e)
		}
		for _, o := range w.Spec.OrderBy {
			poison(o.Expr)
		}
	}
	// A compound arm and a subquery each compile as their own statement and
	// run this pass over their own scopes.

	if !known {
		return &fts5MatchPlacement{}
	}
	good := map[int]bool{}
	for idx := range hasSlot {
		if !stray[idx] && !hard[idx] {
			good[idx] = true
		}
	}
	// Two bound fts5 tables whose query strings read each other leave the
	// planner no order to open them in ("no query solution", verified over
	// "WHERE t MATCH u.x AND u MATCH t.a" -- while either one alone, and
	// "t MATCH u.x AND u MATCH 'beta'", answer normally). Drop every scope on
	// a cycle; a self-edge cannot occur here (slotScope refuses it above).
	for idx := range good {
		if fts5DepCycle(idx, deps, good) {
			delete(good, idx)
			hard[idx] = true
		}
	}
	return &fts5MatchPlacement{good: good, hard: hard}
}

// fts5MatchPlacement is fts5MatchBindings' verdict per table scope (by index
// into the compile's scopes): served, refused while PREPARING, or -- the
// default for everything else -- refused when the row test RUNS. The zero
// value refuses everything at run time, which is what a compile with no
// statement in hand (nil placement) and an unclassifiable statement both get.
type fts5MatchPlacement struct {
	// good is the scopes whose MATCH the planner consumes as a constraint.
	good map[int]bool
	// hard is the scopes whose MATCH the planner DID lift into a constraint
	// and can never make usable -- C SQLite fails at prepare time with "no
	// query solution", so no row is needed to observe it.
	hard map[int]bool
}

func (pl *fts5MatchPlacement) serves(idx int) bool {
	return pl != nil && pl.good[idx]
}

func (pl *fts5MatchPlacement) noSolution(idx int) bool {
	return pl != nil && pl.hard[idx]
}

// fts5FirstMatchScope reports the scope the first MatchExpr in e resolves to.
// found is false when e holds no MatchExpr at all, or when the first one's
// target is not an fts5 table (there is then no scope to classify against).
func fts5FirstMatchScope(ctx *evalCtx, e Expr, known *bool) (int, bool) {
	idx, found := -1, false
	if !fts5WalkMatchExpr(e, func(m MatchExpr) {
		if found {
			return
		}
		if i, ok := fts5MatchScopeIndex(ctx, m.X); ok {
			idx, found = i, true
		}
	}, nil) {
		*known = false
	}
	return idx, found
}

// fts5RowidEq reports whether e is "<scope s>.rowid = <expr>" (either way
// round), the one non-MATCH constraint fts5's xBestIndex takes for free --
// fts5BestIndexMethod's bSeenEq arm, "p->op==EQ && iCol<0", estimated cost 25.
// The other side must not read s itself, exactly as for a MATCH's own query
// string (whereexpr.c:1543). An UNQUALIFIED rowid is accepted only when the
// statement has a single scope, where it cannot be ambiguous.
func fts5RowidEq(ctx *evalCtx, e Expr, s int) bool {
	b, ok := e.(BinaryExpr)
	if !ok || b.Op != "=" {
		return false
	}
	isRowid := func(x Expr) bool {
		ce, ok := x.(ColumnExpr)
		if !ok {
			return false
		}
		switch r33sFoldIdent(ce.Name) {
		case "rowid", "oid", "_rowid_":
		default:
			return false
		}
		if ce.Qualifier != "" {
			return equalFoldName(ctx.tables[s].name, ce.Qualifier)
		}
		return len(ctx.tables) == 1
	}
	other := func(x Expr) bool {
		refs, allResolved, known := fts5ExprScopes(ctx, x)
		return known && allResolved && !refs[s]
	}
	return (isRowid(b.L) && other(b.R)) || (isRowid(b.R) && other(b.L))
}

// fts5DepCycle reports whether start can reach itself through deps, following
// only edges into scopes that are still bound (an unbound scope's MATCH raises
// before any ordering question arises).
func fts5DepCycle(start int, deps map[int]map[int]bool, good map[int]bool) bool {
	seen := map[int]bool{}
	var walk func(int) bool
	walk = func(at int) bool {
		for next := range deps[at] {
			if !good[next] {
				continue
			}
			if next == start {
				return true
			}
			if seen[next] {
				continue
			}
			seen[next] = true
			if walk(next) {
				return true
			}
		}
		return false
	}
	return walk(start)
}

// fts5OuterJoinScopes reports which scopes are the right-hand (NULL-extended)
// operand of an outer join -- where a consumed WHERE term is still evaluated
// (disableTerm, wherecode.c:419) -- and whether the statement contains any
// join that FIXES the FROM order (where.c:5010's JT_OUTER|JT_CROSS). A RIGHT
// or FULL join marks every scope: which side is NULL-extended there depends on
// the row, and over-marking only costs a refusal.
func fts5OuterJoinScopes(stmt *SelectStmt, scopes []tableScope) (outerRight map[int]bool, barrier bool) {
	outerRight = map[int]bool{}
	markAll := func() {
		for j := range scopes {
			outerRight[j] = true
		}
	}
	for i := range stmt.From {
		it := &stmt.From[i]
		switch {
		case it.Join == JoinRight || it.Join == JoinFull:
			barrier = true
			markAll()
		case it.Join == JoinLeft:
			barrier = true
			if idx := fts5ScopeOfItem(it, scopes); idx >= 0 {
				outerRight[idx] = true
			} else {
				markAll()
			}
		case it.CrossKeyword:
			// Not an outer join -- nothing is NULL-extended -- but still a
			// reorder barrier in the C.
			barrier = true
		}
	}
	return outerRight, barrier
}

// fts5ScopeOfItem finds the scope a FROM item contributed, by the name a
// column reference would use for it (its alias, else its table name). -1 when
// no scope matches -- an unaliased derived table, or any shape whose scopes
// this pass cannot line up with the FROM list.
func fts5ScopeOfItem(it *FromItem, scopes []tableScope) int {
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	if name == "" {
		return -1
	}
	for i := range scopes {
		if equalFoldName(scopes[i].name, name) {
			return i
		}
	}
	return -1
}

// fts5MatchScopeIndex is fts5MatchTarget additionally reporting WHICH element
// of ctx.tables matched. fts5MatchTarget returns a pointer INTO that slice, so
// the index is recovered by identity rather than by resolving again -- which
// matters: its column form STOPS at the first scope holding the name, so
// resolving one scope at a time would not answer the same question.
func fts5MatchScopeIndex(ctx *evalCtx, e Expr) (int, bool) {
	ts, _, ok := ctx.fts5MatchTarget(e)
	if !ok {
		return -1, false
	}
	for i := range ctx.tables {
		if &ctx.tables[i] == ts {
			return i, true
		}
	}
	return -1, false
}

// fts5ExprScopes reports which scopes e reads columns of. allResolved is false
// when some column reference in e names no scope column at all -- C SQLite
// rejects such a statement while PREPARING ("no such column: x2"), which is a
// different outcome from the run-time MATCH error and must stay a compile-time
// refusal. known is false for an expression node this walker does not know.
func fts5ExprScopes(ctx *evalCtx, e Expr) (refs map[int]bool, allResolved bool, known bool) {
	refs = map[int]bool{}
	allResolved = true
	known = fts5WalkMatchExpr(e, nil, func(ce ColumnExpr) {
		lname := r33sFoldIdent(ce.Name)
		if ce.Qualifier != "" {
			for i := range ctx.tables {
				if !equalFoldName(ctx.tables[i].name, ce.Qualifier) {
					continue
				}
				if _, ok := ctx.tables[i].colIndex[lname]; ok {
					refs[i] = true
					return
				}
			}
			allResolved = false
			return
		}
		for i := range ctx.tables {
			if _, ok := ctx.tables[i].colIndex[lname]; ok {
				refs[i] = true
				return
			}
		}
		// Not a declared column: it may still be the table-name hidden column
		// fts5 declares last (fts5MatchTarget) -- or a pseudo-rowid, which
		// this pass does not resolve and reports as unresolved, refusing.
		if idx, ok := fts5MatchScopeIndex(ctx, ce); ok {
			refs[idx] = true
			return
		}
		allResolved = false
	})
	return refs, allResolved, known
}

// fts5WalkMatchExpr walks e, reporting every MatchExpr to onMatch and every
// ColumnExpr to onCol (either may be nil), and returns false for an expression
// node it does not know -- a DEFAULT-DENY, so a caller declines rather than
// assume nothing hides inside. It stops at subquery boundaries: a subquery
// compiles as its own statement and runs its own placement pass.
func fts5WalkMatchExpr(e Expr, onMatch func(MatchExpr), onCol func(ColumnExpr)) bool {
	sum := func(es ...Expr) bool {
		ok := true
		for _, sub := range es {
			if !fts5WalkMatchExpr(sub, onMatch, onCol) {
				ok = false
			}
		}
		return ok
	}
	switch x := e.(type) {
	case nil:
		return true
	case LiteralExpr, ParamExpr, RaiseExpr:
		return true
	case ColumnExpr:
		if onCol != nil {
			onCol(x)
		}
		return true
	case MatchExpr:
		if onMatch != nil {
			onMatch(x)
		}
		return sum(x.X, x.Pattern)
	case UnaryExpr:
		return sum(x.X)
	case BinaryExpr:
		return sum(x.L, x.R)
	case IsNullExpr:
		return sum(x.X)
	case CollateExpr:
		return sum(x.X)
	case CastExpr:
		return sum(x.X)
	case BetweenExpr:
		return sum(x.X, x.Lo, x.Hi)
	case LikeExpr:
		return sum(x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return sum(x.X, x.Pattern)
	case InExpr:
		if x.Sub != nil {
			return sum(x.X)
		}
		return sum(append([]Expr{x.X}, x.List...)...)
	case FuncExpr:
		es := append([]Expr(nil), x.Args...)
		es = append(es, x.Filter)
		es = append(es, x.orderByExprs()...)
		if w := x.Over; w != nil {
			es = append(es, w.PartitionBy...)
			for _, o := range w.OrderBy {
				es = append(es, o.Expr)
			}
			if f := w.Frame; f != nil {
				es = append(es, f.Start.Offset, f.End.Offset)
			}
		}
		return sum(es...)
	case CaseExpr:
		es := []Expr{x.Base, x.Else}
		for _, w := range x.Whens {
			es = append(es, w.When, w.Then)
		}
		return sum(es...)
	case RowExpr:
		return sum(x.Elems...)
	case SubqueryExpr, ExistsExpr:
		return true
	}
	return false
}

// fts5UnusableMatch is what an fts5 MATCH C cannot lift compiles to: a
// MatchExpr with no left operand, whose target resolves to nothing -- evalMatch's
// first error,
//
//	engine: unable to use function MATCH in the requested context
//
// sqlite3InvalidFunction's message (main.c:2220), raised when the row test
// runs. fts5leftjoin.test 2.3 ("... LEFT JOIN t1 ON +b MATCH '1'") answers
// because t1 is empty, and "WHERE rowid=99 AND NOT (t MATCH 'beta')" answers
// no rows through AND's short-circuit (compileAndOr), as in C.
func fts5UnusableMatch(x MatchExpr) MatchExpr {
	return MatchExpr{Pattern: x.Pattern, Not: x.Not}
}

// compileFts5Match compiles "X MATCH q" against an fts5 table. good is this
// compile's fts5MatchBindings result (nil outside compileSelectScan, where
// nothing may be served). It is checkFts3Match's twin, except a refusal is
// compiled (fts5UnusableMatch) wherever C fails at run time. A left operand
// resolving to no column stays a compile-time error, as C rejects it while
// preparing ("no such column: x2").
func (c *compiler) compileFts5Match(pl *fts5MatchPlacement, scopes []tableScope, cursors []int, colBases []int, x MatchExpr) (int, error) {
	ctx := &evalCtx{tables: scopes, pager: c.pager}
	idx, ok := fts5MatchScopeIndex(ctx, x.X)
	switch {
	case !ok:
		if _, allResolved, known := fts5ExprScopes(ctx, x.X); !known || !allResolved {
			return 0, fmt.Errorf("engine: unable to use function MATCH in the requested context")
		}
		// A legal expression that simply is not a virtual-table column
		// ("+b MATCH ...", "o.q MATCH ...", "'lit' MATCH ..."):
		// isAuxiliaryVtabOperator (whereexpr.c:449) requires ExprIsVtab, so
		// the planner leaves the function call in place and it raises per row.
		x = fts5UnusableMatch(x)
	case pl.noSolution(idx):
		return 0, fmt.Errorf("engine: no query solution")
	case !pl.serves(idx):
		x = fts5UnusableMatch(x)
	}
	if ok && matchGroupAfterVtab(colBases, idx) {
		return 0, fmt.Errorf("%w: MATCH over a vtab followed by a parenthesized join group", errVDBEUnsupported)
	}
	// The query is coded into a register right before the OpMatch that
	// reads it, as in the fts3 arm: C codes a vtab constraint value in the
	// loop body that runs OP_VFilter (codeExprOrVector, wherecode.c:1584),
	// so it is recomputed per row of the enclosing loop. It comes after the
	// placement verdicts, so a refused compile discards it.
	patReg, perr := c.compileExpr(x.Pattern)
	if perr != nil {
		return 0, perr
	}
	d := c.alloc()
	c.emit(Instruction{Op: OpMatch, P2: d, P4: &matchCompileInfo{expr: x, scopes: scopes, cursors: cursors, colBases: colBases, patReg: patReg, patCompiled: true}})
	return d, nil
}
