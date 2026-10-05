// The ACCESS-PATH half of the anchor-row proof for a reference to a
// RECURSIVE CTE whose body reads nothing but itself (anchorNoIndexInPlay,
// vdbe_agg_codegen.go).
package engine

// recursiveCTEIndexUnreachable reports whether NO index anywhere in the schema
// can reach b's rows or decide the order they are produced in -- the exact
// question anchorNoIndexInPlay asks of every source, answered structurally for a
// recursive CTE instead of falling back to "does this database hold an index at
// all" (noIndexAnywhere).
//
// Two independent facts make it provable, and both are C, not judgement:
//
//   - A recursive CTE is NEVER FLATTENED into the query that references it.
//     flattenSubquery's restriction (22) is a bare early return:
//
//     select.c:4354   if( pSub->selFlags & (SF_Recursive) ){
//     select.c:4355     return 0; /* Restrictions (22) */
//
//     and pushDownWhereTerms refuses it too (select.c:5146, "restrictions (2)
//     and (11)": `if( pSubq->selFlags & (SF_Recursive|SF_MultiPart) ) return 0`).
//     So the route by which an index reaches "through" an ordinary derived
//     table, view or non-recursive CTE -- the one anchorNoIndexInPlay's
//     `opaque` fallback exists to cover, because flattening splices the inner
//     FROM into the outer query where whereLoopAddBtree then prices its
//     indexes -- is closed by the C itself for this shape. The reference stays
//     a materialized subquery whose Table carries no index for
//     whereLoopAddBtree to consider.
//
//   - The rows themselves are produced by the recursion's own fixed-point
//     queue, and this function additionally requires that queue to be fed by
//     NOTHING but the CTE: every arm's FROM clause is either empty or the
//     single self-reference, so no base table is scanned anywhere inside the
//     body and there is no scan for an index to reorder in the first place.
//
// The second condition is checked, not assumed, and it is what keeps this
// narrower than "b.recursive != nil". A body whose anchor arm reads a real
// table ("WITH r(x) AS (SELECT a FROM t UNION ALL SELECT x+1 FROM r WHERE
// x<9)") has exactly the hazard the guard exists for: an index on t can change
// which row enters the queue first, and so which row of a group is its anchor.
//
// cteOrderProvable (vdbe_join_codegen.go) deliberately answers FALSE for every
// recursive CTE: it asks whether the CTE's scan order matches the planner's
// decision, which has no answer for rows from a queue. This function asks only
// whether an index can matter structurally.
func recursiveCTEIndexUnreachable(b *cteBinding) bool {
	if b == nil || b.recursive == nil {
		return false
	}
	if !recursiveCTEArmSelfContained(b.recursive.initial, b.name) {
		return false
	}
	for _, arm := range b.recursive.recursiveArms {
		if !recursiveCTEArmSelfContained(arm, b.name) {
			return false
		}
	}
	return true
}

// recursiveCTEArmSelfContained reports whether one arm of a recursive CTE
// reads no table, view, virtual table or derived table other than itself.
// Derived tables, TVFs, schema-qualified items, arms with their own WITH clause,
// subqueries, and COMPOUND clauses are all rejected (see select.c).
func recursiveCTEArmSelfContained(stmt *SelectStmt, name string) bool {
	if stmt == nil || len(stmt.CTEs) > 0 || len(stmt.Compound) > 0 {
		return false
	}
	for i := range stmt.From {
		f := &stmt.From[i]
		if f.Subquery != nil || f.TableFunc || f.GroupLen > 0 ||
			f.Schema != "" || !equalFoldName(f.Table, name) {
			return false
		}
	}
	for _, sc := range stmt.Columns {
		if exprContainsSubquery(sc.Expr) {
			return false
		}
	}
	for i := range stmt.From {
		if exprContainsSubquery(stmt.From[i].On) {
			return false
		}
	}
	if exprContainsSubquery(stmt.Where) || exprContainsSubquery(stmt.Having) ||
		exprContainsSubquery(stmt.LimitParam) || exprContainsSubquery(stmt.OffsetParam) {
		return false
	}
	for _, g := range stmt.GroupBy {
		if exprContainsSubquery(g) {
			return false
		}
	}
	for _, ot := range stmt.OrderBy {
		if exprContainsSubquery(ot.Expr) {
			return false
		}
	}
	return true
}
