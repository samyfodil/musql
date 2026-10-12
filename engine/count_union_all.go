package engine

import "strings"

// countUnionAll rewrites "SELECT count(*) FROM (A UNION ALL B ...)" to the sum
// of its arms' counts,
//
//	SELECT (SELECT count(*) FROM (A)) + (SELECT count(*) FROM (B)) ...
//
// which is what the query means: UNION ALL concatenates, so no row of one arm
// can cancel a row of another, and count(*) reads no column. Each arm then
// meets the ordinary flattener on its own, so "SELECT count(*) FROM (SELECT k
// FROM t WHERE k < 3)" is the single-table count it flattens to, instead of
// the whole row set being built (or streamed) only to be counted. C does not
// compute a result column nothing reads either (disableUnusedSubqueryResultColumns,
// select.c), so an arm whose column would raise an error nobody sees raises
// none here.
//
// It returns nil, leaving stmt alone, for anything else: another select
// list, a WHERE, GROUP BY, DISTINCT, ORDER BY or LIMIT on either side, a join,
// a WITH, a VALUES arm, any operator but UNION ALL, and arms whose column
// counts differ or cannot be counted without expanding a "*" -- that error is
// C's and is raised by the compound the statement still is. The result column
// keeps the original's name, whatever its spelling.
func (p *ReadOnlyPager) countUnionAll(stmt *SelectStmt) *SelectStmt {
	if p == nil || stmt == nil || len(stmt.Columns) != 1 || len(stmt.From) != 1 {
		return nil
	}
	if stmt.Distinct || stmt.Where != nil || len(stmt.GroupBy) != 0 || stmt.Having != nil ||
		len(stmt.Compound) != 0 || len(stmt.OrderBy) != 0 || stmt.Limit != nil || stmt.Offset != nil ||
		stmt.LimitParam != nil || stmt.OffsetParam != nil || len(stmt.Windows) != 0 || len(stmt.CTEs) != 0 ||
		stmt.ValuesArms != 0 || stmt.FromNestedParenJoin {
		return nil
	}
	col := stmt.Columns[0]
	fn, ok := col.Expr.(FuncExpr)
	if col.Star || !ok || !strings.EqualFold(fn.Name, "count") || !fn.Star || len(fn.Args) != 0 ||
		fn.Distinct || fn.Over != nil || fn.Filter != nil || len(fn.OrderBy) != 0 {
		return nil
	}
	it := stmt.From[0]
	sub := it.Subquery
	if sub == nil || it.On != nil || len(it.Using) != 0 || it.Natural || it.TableFunc || len(it.TableFuncArgs) != 0 ||
		it.IndexedBy != "" || it.NotIndexed || it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias ||
		it.GroupRebuildID != 0 || it.NestFromWrapDepth != 0 || len(it.NestedGroupSpan) != 0 ||
		it.NestedNonLeading || it.NestedGroupID != 0 || it.UpdateTarget {
		return nil
	}
	if len(sub.Compound) == 0 || len(sub.OrderBy) != 0 || sub.Limit != nil || sub.Offset != nil ||
		sub.LimitParam != nil || sub.OffsetParam != nil || len(sub.CTEs) != 0 || sub.ValuesArms != 0 {
		return nil
	}
	arms := make([]*SelectStmt, 0, len(sub.Compound)+1)
	first := *sub
	first.Compound = nil
	arms = append(arms, &first)
	for _, a := range sub.Compound {
		if a.Op != "UNION ALL" || a.Stmt == nil || len(a.Stmt.Compound) != 0 || len(a.Stmt.CTEs) != 0 {
			return nil
		}
		arms = append(arms, a.Stmt)
	}
	// Every arm lists the same number of explicit columns: a "*" is expanded
	// against the arm's FROM, which only the compound's own check does.
	for _, arm := range arms {
		if len(arm.Columns) != len(first.Columns) {
			return nil
		}
		for _, c := range arm.Columns {
			if c.Star {
				return nil
			}
		}
	}
	var sum Expr
	for _, arm := range arms {
		count := &SelectStmt{
			Columns: []SelectColumn{{Expr: FuncExpr{Name: "count", Star: true}, Text: "count(*)"}},
			From:    []FromItem{{Subquery: arm}},
		}
		if sum == nil {
			sum = SubqueryExpr{Stmt: count}
			continue
		}
		sum = BinaryExpr{Op: "+", L: sum, R: SubqueryExpr{Stmt: count}}
	}
	out := *stmt
	out.Columns = []SelectColumn{col}
	out.Columns[0].Expr = sum
	out.From = nil
	out.FromParenthesized = false
	return &out
}
