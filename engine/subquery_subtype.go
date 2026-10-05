package engine

// A value's subtype survives a FROM-clause subquery, view or CTE in exactly one
// case: when C flattens the subquery and the result column is a bare column
// reference. Everything else loses it at the boundary.
// flattenedSubtypeKeep identifies which result columns keep their subtype.
func flattenedSubtypeKeep(outer *SelectStmt, items []FromItem, i int, sub *SelectStmt, ncol int) []bool {
	if outer == nil || sub == nil || i >= len(items) {
		return nil
	}
	// Not modelled: LIMIT, ORDER BY, or compound subqueries.
	if sub.Limit != nil || sub.Offset != nil || sub.LimitParam != nil || sub.OffsetParam != nil ||
		len(sub.OrderBy) > 0 || len(sub.Compound) > 0 {
		return nil
	}
	if selectHasWindow(outer) || orderByHasWindow(outer) || len(outer.Windows) > 0 ||
		selectHasWindow(sub) || orderByHasWindow(sub) || len(sub.Windows) > 0 { // (25)
		return nil
	}
	if len(sub.From) == 0 || sub.Distinct { // (7), (4)
		return nil
	}
	// sqlite3Select skips an aggregate subquery before asking (select.c:7808).
	if selectIsAggregateQuery(sub) || sub.Having != nil {
		return nil
	}
	for _, it := range sub.From {
		if it.Join == JoinRight || it.Join == JoinFull { // (27a), conservatively
			return nil
		}
	}
	if items[i].Join == JoinRight || items[i].Join == JoinFull { // (26)
		return nil
	}
	// JT_OUTER, or JT_LTORJ: an item left of a later RIGHT or FULL JOIN
	// (select.c:4373). Either way (3a) the subquery is not itself a join and
	// (3d) the outer is not DISTINCT. C may first turn that RIGHT JOIN into a
	// LEFT or INNER one (select.c:7740-7779), which only flattens more.
	outerJoin := items[i].Join == JoinLeft
	for _, it := range items[i+1:] {
		outerJoin = outerJoin || it.Join == JoinRight || it.Join == JoinFull
	}
	if outerJoin && (len(sub.From) != 1 || sub.From[0].GroupLen > 0 || sub.From[0].NestedGroupSpan != nil || outer.Distinct) {
		return nil
	}

	keep := make([]bool, ncol)
	stars, bare := 0, true
	for _, sc := range sub.Columns {
		if sc.Star {
			stars++
		} else if _, ok := sc.Expr.(ColumnExpr); !ok {
			bare = false
		}
	}
	switch {
	case bare:
		// A "*" expands to bare column references too (selectExpander).
		for j := range keep {
			keep[j] = true
		}
	case stars == 0:
		for j, sc := range sub.Columns {
			if j < ncol {
				_, keep[j] = sc.Expr.(ColumnExpr)
			}
		}
	default:
		// A "*" beside a non-column expression is unmodelled.
		return nil
	}
	return keep
}
