package engine

import "strings"

// nthValueReach hands a window select the number of rows its reader takes.
//
// C checks nth_value's N on the row its window code returns
// (windowReturnOneRow, window.c), and a window select read from a FROM clause
// is a co-routine of its reader (fromClauseTermCanBeCoroutine, select.c:7266:
// the only item, (1a)), so a reader that stops after k rows never returns the
// rows past them, and an invalid N there raises nothing:
//
//	SELECT id FROM (SELECT id, nth_value(v, n) OVER (ORDER BY id) FROM w) LIMIT 2
//
// answers the first two ids over a w whose third row has n = 0. windowFinal
// decides which rows were returned from the window select's own LIMIT
// (windowOuterOrderDropped), so the reader's count is given to it as a LIMIT on
// a copy of the select; the rows past it are never read, so it changes no
// answer. Readers whose count is fixed by their text are followed:
//
//   - a scalar subquery and EXISTS, which C codes with LIMIT 1
//     (sqlite3CodeSubselect, expr.c);
//   - a SELECT whose only FROM item is the window select and which does
//     nothing but project its rows under a constant LIMIT (and OFFSET).
//
// pull is the reader's row count, negative for all of them. sel is not
// modified; it is returned itself when nothing changes.
func nthValueReach(sel *SelectStmt, pull int64) *SelectStmt {
	if sel == nil || pull < 0 || len(sel.Compound) != 0 || sel.LimitParam != nil || sel.OffsetParam != nil {
		return sel
	}
	if selectCallsNthValue(sel) {
		// Its own LIMIT stays where it is smaller, and its OFFSET rows are
		// returned ahead of the ones the reader takes, as before.
		if sel.Limit != nil && *sel.Limit >= 0 && *sel.Limit <= pull {
			return sel
		}
		cp := *sel
		cp.Limit = &pull
		return &cp
	}
	if len(sel.From) != 1 || sel.From[0].Subquery == nil || sel.Where != nil || len(sel.GroupBy) != 0 ||
		sel.Having != nil || sel.Distinct || len(sel.OrderBy) != 0 || len(sel.CTEs) != 0 ||
		cHasWindow(sel) || cSelectIsAggregate(sel) {
		return sel
	}
	it := sel.From[0]
	if it.On != nil || len(it.Using) != 0 || it.Natural || it.TableFunc || it.GroupLen != 0 ||
		it.NestedGroupID != 0 || len(it.NestedGroupSpan) != 0 || sel.FromNestedParenJoin {
		return sel
	}
	n := pull
	if sel.Limit != nil && *sel.Limit >= 0 && *sel.Limit < n {
		n = *sel.Limit
	}
	// A LIMIT of 0 leaves before the loop (computeLimitRegisters,
	// select.c:2531); OFFSET rows are read and skipped.
	if n > 0 && sel.Offset != nil && *sel.Offset > 0 {
		n += *sel.Offset
	}
	sub := nthValueReach(it.Subquery, n)
	if sub == it.Subquery {
		return sel
	}
	cp := *sel
	cp.From = []FromItem{it}
	cp.From[0].Subquery = sub
	return &cp
}

// nthValueReachOwn is nthValueReach for a SELECT read to its end: only its own
// LIMIT bounds what it takes from a window select in its FROM clause.
func nthValueReachOwn(sel *SelectStmt) *SelectStmt {
	if sel == nil || sel.Limit == nil || *sel.Limit < 0 || selectCallsNthValue(sel) {
		return sel
	}
	return nthValueReach(sel, *sel.Limit)
}

// selectCallsNthValue reports whether sel's own result list or ORDER BY calls
// the nth_value window function.
func selectCallsNthValue(sel *SelectStmt) bool {
	found := false
	visit := func(fc FuncExpr) bool {
		if fc.Over != nil && strings.EqualFold(fc.Name, "nth_value") {
			found = true
		}
		return !found
	}
	for _, c := range sel.Columns {
		if !c.Star && !walkExprShallow(c.Expr, visit) {
			return true
		}
	}
	for _, o := range sel.OrderBy {
		if !walkExprShallow(o.Expr, visit) {
			return true
		}
	}
	return found
}
