// SQLite's COUNT-OF-VIEW optimization, ported because where this engine is
// concerned it is not an optimization at all: it decides whether a compound
// derived table's result expressions are CODED, and among the expressions it
// deletes are ones C SQLite would otherwise REJECT.
//
// countOfViewOptimization (select.c:7137, applied from sqlite3Select at
// select.c:7938) rewrites
//
//	SELECT count(*) FROM (A UNION ALL B)
//	  -->  (SELECT count(*) FROM <A's FROM>) + (SELECT count(*) FROM <B's FROM>)
//
// and the load-bearing pair of lines is
//
//	sqlite3ParserAddCleanup(pParse, sqlite3ExprListDeleteGeneric, pSub->pEList);
//	pSub->pEList = sqlite3ExprListAppend(pParse, 0, pTerm);
//
// -- each arm's OWN select list is DISCARDED and replaced by count(*), so the
// arm's expressions are never turned into code at all.
//
// That is why aggnested.test's dbsqlfuzz statement
//
//	SELECT * FROM t0 WHERE EXISTS (SELECT 1 FROM t1 GROUP BY c3
//	  HAVING (SELECT count(*) FROM (SELECT 1 UNION ALL
//	                                SELECT sum(DISTINCT c1)))) BETWEEN 1 AND 1
//
// ANSWERS in C SQLite (every row of t0) instead of failing. "sum(DISTINCT
// c1)" there is an aggregate whose argument names t0, two query levels out, so
// resolve.c:1355-1361's NameContext climb hands it to t0's query -- and t0's
// query is NOT an aggregate query, because resolve.c:1978-1985 fixes
// SF_Aggregate from the RESULT SET alone, BEFORE the WHERE clause is resolved
// at :2005, so a discovery made while resolving WHERE arrives too late.
// select.c:8432-8442 then analyzes only pEList/ORDER BY/HAVING into the
// AggInfo, never the WHERE, and a TK_AGG_FUNCTION left with pAggInfo==0 is a
// hard error at code time -- "misuse of aggregate: %#T()" (expr.c:5333-5344).
//
// The count(*) spelling deletes the offending expression before coding.
// Real SQLite raises an error for any other spelling that reads the column.
//
// # Why this port must resolve what it is about to delete
//
// C resolves the whole statement before optimizing, so unresolvable names
// inside an arm being deleted still error. This engine has no separate resolve
// pass, so the rewrite compiles a probe over what it deletes and refuses on
// error, leaving the ordinary path to raise its message. That probe motivates
// the FROM-less/subquery-free restriction below.
package engine

// countOfViewRewrite returns stmt rewritten as countOfViewOptimization
// (select.c:7137) would rewrite it, or the SAME pointer when it does not apply
// -- so a caller can apply it unconditionally. The result is always a FROM-LESS
// statement, which is how a caller tells the two apart without a second return
// value: stmt itself always has exactly one FROM item when this fires.
//
// p/outer/rowOuter are the enclosing compile, needed only for the resolution
// probe (see this file's header).
//
// Nothing is mutated: the same *SelectStmt is re-entered once per enclosing row
// for a correlated subquery and must never accumulate a rewrite -- the rule
// r35cFlattenSubqueryLimit states, for the same reason.
//
// Every guard select.c applies is applied here, plus REFUSALS this engine's AST
// and compile order need that C does not (each marked "narrower"). Refusing is
// always safe: it leaves the statement compiled exactly the way it already was.
func countOfViewRewrite(p *ReadOnlyPager, stmt *SelectStmt, outer *compiler, rowOuter *evalCtx) *SelectStmt {
	if stmt == nil {
		return stmt
	}
	// The outer query: a single count(*) result column and nothing else
	// (select.c:7143-7156). "This is an aggregate" (:7143) is implied --
	// count(*) references no table, so sqlite3ReferencesSrcList answers -1 and
	// the call stays with this query (resolve.c:1357), setting NC_HasAgg on it.
	if len(stmt.Columns) != 1 || stmt.Columns[0].Star {
		return stmt
	}
	fc, ok := stmt.Columns[0].Expr.(FuncExpr)
	if !ok || !equalFoldName(fc.Name, "count") || !fc.Star || len(fc.Args) != 0 ||
		fc.Distinct || fc.Over != nil || fc.Filter != nil || len(fc.OrderBy) > 0 {
		// "Is count()" and "Must be count(*)" (pExpr->x.pList!=0 rejects
		// count(x)), and "Not a window function" -- which EP_WinFunc makes
		// cover a bare FILTER clause too (select.c:7150-7156).
		return stmt
	}
	if stmt.Where != nil || stmt.Having != nil || len(stmt.GroupBy) != 0 || len(stmt.OrderBy) != 0 {
		return stmt // select.c:7145-7148
	}
	// Narrower than C: this query yields one row either way, so DISTINCT and
	// LIMIT/OFFSET are no-ops on it and C does not check them -- but a
	// FROM-less compile of either is a second thing to get right for no gain.
	if stmt.Distinct || stmt.Limit != nil || stmt.Offset != nil ||
		stmt.LimitParam != nil || stmt.OffsetParam != nil || len(stmt.Windows) != 0 {
		return stmt
	}
	if len(stmt.From) != 1 {
		return stmt // "One table in FROM" (select.c:7157)
	}
	it := stmt.From[0]
	sub := it.Subquery
	// "FROM is a subquery" (select.c:7158) and "Not a CTE" (SF_CopyCte,
	// select.c:7162). A CTE reference is a NAMED FROM item in this AST -- it is
	// expanded into a row source only at resolve time -- so requiring an
	// unnamed derived table is that guard, and it excludes a table-valued
	// function at the same time.
	if sub == nil || it.Table != "" || it.TableFunc || it.On != nil {
		return stmt
	}
	if len(sub.Compound) == 0 {
		return stmt // "Must be a compound" (pSub->pPrior==0, select.c:7160)
	}
	// Narrower than C: a multi-row VALUES clause is ONE arm in C SQLite
	// (sqlite3MultiValues -- see SelectStmt.ValuesFold) and several here, so
	// this port cannot count its arms the way C does; and a WITH, a WINDOW, an
	// ORDER BY or a LIMIT belonging to the compound as a whole has nowhere to
	// go once the arms become independent subqueries.
	if sub.ValuesArms != 0 || len(sub.CTEs) != 0 || len(sub.Windows) != 0 ||
		len(sub.OrderBy) != 0 || sub.Limit != nil || sub.Offset != nil ||
		sub.LimitParam != nil || sub.OffsetParam != nil {
		return stmt
	}
	arms := make([]*SelectStmt, 0, len(sub.Compound)+1)
	arms = append(arms, sub)
	for _, a := range sub.Compound {
		if a.Op != "UNION ALL" { // "Must be UNION ALL" (select.c:7164)
			return stmt
		}
		arms = append(arms, a.Stmt)
	}
	for _, arm := range arms {
		if arm == nil || !countOfViewArmOK(arm) {
			return stmt
		}
	}
	if !countOfViewProbeResolves(p, arms, outer, rowOuter) {
		return stmt
	}
	// The transformation itself (select.c:7176-7204), right-associated exactly
	// as C associates it -- A + (B + C), since C walks pPrior (right to left)
	// building "pExpr = sqlite3PExpr(TK_PLUS, pTerm, pExpr)".
	var sum Expr
	for i := len(arms) - 1; i >= 0; i-- {
		armCopy := *arms[i]
		armCopy.Columns = []SelectColumn{{Expr: FuncExpr{Name: "count", Star: true}, Text: "count(*)"}}
		armCopy.Compound = nil
		armCopy.r35dCompoundOf = nil
		term := Expr(SubqueryExpr{Stmt: &armCopy})
		if sum == nil {
			sum = term
		} else {
			sum = BinaryExpr{Op: "+", L: term, R: sum}
		}
	}
	out := *stmt
	out.From = nil
	// Only the EXPRESSION is replaced, never the result-column naming fields:
	// C assigns p->pEList->a[0].pExpr and leaves the ExprList item's zEName
	// alone, so "SELECT count(*) FROM (...)" still reports the column name
	// "count(*)".
	col := stmt.Columns[0]
	col.Expr = sum
	out.Columns = []SelectColumn{col}
	return &out
}

// countOfViewArmOK is every restriction select.c:7163-7176 puts on ONE arm of
// the compound, with countOfViewArmIsAggregate standing in for SF_Aggregate.
//
// The FROM-less/subquery-free pair at the end is this port's own, and it is
// what keeps the resolution probe honest AND cheap: an arm with no FROM and no
// subquery has a probe that is a plain FROM-less expression compile, which can
// resolve names against the enclosing chain but cannot record an aggregate
// hoist on it (vdbe_agg_hoist.go) -- the one side effect a throwaway compile
// must not have, since a recorded hoist makes the ENCLOSING statement recompile
// as an aggregate query and changes its row count. It also costs nothing worth
// keeping: an arm WITH a FROM is exactly the case C wrote this optimization for
// (skip the rows), never a case this engine needs it for (skip the ERROR).
func countOfViewArmOK(arm *SelectStmt) bool {
	if arm.Where != nil || arm.Having != nil { // pSub->pWhere (:7165)
		return false
	}
	if arm.Limit != nil || arm.Offset != nil || arm.LimitParam != nil || arm.OffsetParam != nil {
		return false // pSub->pLimit (:7166)
	}
	if arm.Distinct { // SF_Distinct (:7167)
		return false
	}
	if arm.ValuesArms != 0 || len(arm.CTEs) != 0 || len(arm.Windows) != 0 || len(arm.OrderBy) != 0 {
		return false // narrower than C, as above
	}
	if len(arm.From) != 0 || len(arm.GroupBy) != 0 {
		return false
	}
	for _, c := range arm.Columns {
		if c.Star || exprHasWindow(c.Expr) || exprContainsSubquery(c.Expr) {
			// pSub->pWin (:7174); a bare "*" over an empty FROM and a subquery
			// are this port's own refusals.
			return false
		}
	}
	return !countOfViewArmIsAggregate(arm)
}

// countOfViewArmIsAggregate is C SQLite's SF_Aggregate for one compound arm
// -- resolve.c:1978-1985's "pGroupBy || (sNC.ncFlags & NC_HasAgg)!=0", where
// NC_HasAgg reaches THIS arm's NameContext only for an aggregate call the
// outward climb at resolve.c:1355-1361 stops at, i.e. one whose
// sqlite3ReferencesSrcList (expr.c:7200) answers something other than 0.
//
// An arm whose aggregate argument names a table in an outer query is NOT an
// aggregate query at this level (sqlite3ReferencesSrcList returns 0). Only
// FROM-less arms reach here (countOfViewArmOK), the only case decidable exactly.
func countOfViewArmIsAggregate(arm *SelectStmt) bool {
	stays := false
	for _, c := range arm.Columns {
		if c.Star {
			continue
		}
		forEachAggCall(c.Expr, func(fc FuncExpr) {
			if !aggCallNamesAColumn(fc) {
				// sqlite3ReferencesSrcList == -1, "references no tables at
				// all": the climb stops HERE and this arm owns the call.
				stays = true
			}
		})
	}
	return stays
}

// countOfViewProbeResolves compiles, and throws away, the names this rewrite is
// about to delete -- see this file's header for why C gets this for free and
// this engine does not. Every expression is compiled at the arm's own level
// (FROM-less, against the enclosing chain), with each aggregate call replaced
// by its own arguments: the CALL is what C SQLite associates outward, but
// its ARGUMENTS are ordinary names resolved right where they are written.
//
// A failure of any kind refuses the rewrite rather than being reported, so the
// ordinary compile raises whatever message it always raised.
func countOfViewProbeResolves(p *ReadOnlyPager, arms []*SelectStmt, outer *compiler, rowOuter *evalCtx) bool {
	var cols []SelectColumn
	for _, arm := range arms {
		for _, c := range arm.Columns {
			for _, e := range countOfViewProbeExprs(c.Expr) {
				cols = append(cols, SelectColumn{Expr: e, Text: "p"})
			}
		}
	}
	if len(cols) == 0 {
		return true
	}
	_, err := compileSelectNoFromTrig(p, &SelectStmt{Columns: cols}, outer, enclosingTriggerCtx(outer), rowOuter, nil)
	return err == nil
}

// countOfViewProbeExprs splits one deleted result expression into the pieces
// whose names must still resolve: an aggregate CALL becomes its arguments (and
// FILTER), anything else stands for itself.
func countOfViewProbeExprs(e Expr) []Expr {
	var out []Expr
	var walk func(Expr)
	walk = func(x Expr) {
		if x == nil {
			return
		}
		if fc, ok := x.(FuncExpr); ok && isAggregateCall(fc) {
			for _, a := range fc.Args {
				walk(a)
			}
			walk(fc.Filter)
			for _, ob := range fc.orderByExprs() {
				walk(ob)
			}
			return
		}
		if !containsAggregate(x) {
			out = append(out, x)
			return
		}
		walkExprOperands(x, walk)
	}
	walk(e)
	return out
}

// forEachAggCall calls fn on every aggregate call written at e's OWN level --
// never inside a subquery body, which is a different NameContext and so a
// different query's business (walkExprOperands' own shape).
func forEachAggCall(e Expr, fn func(FuncExpr)) {
	if e == nil {
		return
	}
	if fc, ok := e.(FuncExpr); ok && isAggregateCall(fc) {
		fn(fc)
	}
	walkExprOperands(e, func(x Expr) { forEachAggCall(x, fn) })
}

// aggCallNamesAColumn reports whether fc's own arguments (and FILTER) name any
// column at all -- exprRefToSrcList's question (expr.c:7163-7182), minus the
// cursor identity it has and this does not. A nested subquery inside an
// argument is skipped for the reason selectRefEnter gives (expr.c:7126): its
// own FROM tables are EXCLUDED from the scan, so a column there proves nothing
// about this call.
func aggCallNamesAColumn(fc FuncExpr) bool {
	found := false
	var walk func(Expr)
	walk = func(x Expr) {
		if found || x == nil {
			return
		}
		if _, ok := x.(ColumnExpr); ok {
			found = true
			return
		}
		walkExprOperands(x, walk)
	}
	for _, a := range fc.Args {
		walk(a)
	}
	walk(fc.Filter)
	for _, ob := range fc.orderByExprs() {
		walk(ob)
	}
	return found
}
