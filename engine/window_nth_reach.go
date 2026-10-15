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

// nthValueTail marks the window select a reader with one FROM item reads, so
// that the reader meets an invalid nth_value N only on the row that carries it
// (windowPlan.tail). C codes that item as a co-routine (select.c:7266, (1a))
// and its window code checks N on the row it returns (windowReturnOneRow), so
// a reader that stops early -- a LIMIT reached through a WHERE or a DISTINCT,
// counts no text fixes -- never sees the rows past its stop:
//
//	SELECT id FROM (SELECT id, nth_value(v, n) OVER (ORDER BY id) FROM w)
//	WHERE id < 3 LIMIT 2
//
// answers over a w whose third row has n = 0. The reader here walks the
// materialized rows in the same order and stops where C's does; an aggregate,
// GROUP BY or window function in the reader is left alone, as is a CTE C
// would materialize (referenced again, or AS MATERIALIZED).
func (p *ReadOnlyPager) nthValueTail(sel *SelectStmt) *SelectStmt {
	if p == nil || sel == nil || len(sel.From) == 0 || len(sel.Compound) != 0 || len(sel.GroupBy) != 0 ||
		sel.Having != nil || cHasWindow(sel) || cSelectIsAggregate(sel) || sel.FromNestedParenJoin {
		return sel
	}
	if len(sel.From) > 1 {
		// In a join, a derived window select C codes as a co-routine; the
		// loops around it must then be C's (markCoroutineReruns).
		var out *SelectStmt
		for i, it := range sel.From {
			body := it.Subquery
			if body == nil || body.nthTail || len(body.Compound) != 0 || !selectCallsNthValue(body) ||
				it.GroupLen != 0 || it.NestedGroupID != 0 || len(it.NestedGroupSpan) != 0 {
				continue
			}
			if co, certain := p.cCoroutineVerdict(sel, sel.From, i, nil); !co || !certain {
				continue
			}
			if out == nil {
				cp := *sel
				cp.From = append([]FromItem(nil), sel.From...)
				out = &cp
			}
			nb := *body
			nb.nthTail = true
			out.From[i].Subquery = &nb
		}
		if out == nil {
			return sel
		}
		return out
	}
	it := sel.From[0]
	if it.On != nil || len(it.Using) != 0 || it.Natural || it.TableFunc || it.GroupLen != 0 ||
		it.NestedGroupID != 0 || len(it.NestedGroupSpan) != 0 {
		return sel
	}
	if it.Subquery == nil {
		// A CTE this statement defines and names only here: nUse 1, a
		// co-routine like a subquery.
		defined := false
		for _, d := range sel.CTEs {
			if equalFoldName(d.Name, it.Table) {
				defined = d.Materialized != cteM10dYes
			}
		}
		if !defined || it.Schema != "" || countMentions(sel, func(o FromItem) bool {
			return o.Schema == "" && !o.TableFunc && equalFoldName(o.Table, it.Table)
		}) != 1 {
			return sel
		}
		ex, ok := p.r38cExpandCTERef(it)
		if !ok {
			return sel
		}
		it = ex
	}
	body := it.Subquery
	if body == nil || body.nthTail || len(body.Compound) != 0 || !selectCallsNthValue(body) {
		return sel
	}
	nb := *nthTailPushDown(sel, it, body)
	nb.nthTail = true
	it.Subquery = &nb
	cp := *sel
	cp.From = []FromItem{it}
	return &cp
}

// nthTailPushDown is pushDownWhereTerms (select.c:5134) into the window select
// it reads, for a reader nthValueTail marks: a WHERE term of the reader over
// columns every window of the body partitions by is copied into the body's
// WHERE (restriction (6c) allows exactly that), so the partitions it rules out
// are never returned -- and never meet an nth_value check. The reader keeps
// the term too. Only bare columns, with no function or subquery, are copied.
func nthTailPushDown(sel *SelectStmt, it FromItem, body *SelectStmt) *SelectStmt {
	if sel.Where == nil || body.Limit != nil || body.LimitParam != nil || body.Offset != nil || body.OffsetParam != nil ||
		len(body.Compound) != 0 || len(body.Windows) != 0 || len(body.GroupBy) != 0 || cSelectIsAggregate(body) {
		return body
	}
	var specs []*WindowSpec
	ok := true
	collect := func(fc FuncExpr) bool {
		if fc.Over != nil {
			if fc.Over.Ref != "" {
				ok = false
			}
			specs = append(specs, fc.Over)
		}
		return true
	}
	for _, c := range body.Columns {
		if c.Star {
			return body
		}
		walkExprShallow(c.Expr, collect)
	}
	for _, o := range body.OrderBy {
		walkExprShallow(o.Expr, collect)
	}
	if !ok || len(specs) == 0 {
		return body
	}
	partitioned := func(e Expr) bool {
		for _, sp := range specs {
			found := false
			for _, p := range sp.PartitionBy {
				if exprEqual(p, e) {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	colFor := func(ce ColumnExpr) (Expr, bool) {
		if ce.Schema != "" || (ce.Qualifier != "" && !equalFoldName(ce.Qualifier, it.Alias)) {
			return nil, false
		}
		for _, c := range body.Columns {
			name := c.Alias
			if !c.HasAlias {
				name = c.cName
				if bc, isCol := c.Expr.(ColumnExpr); isCol && name == "" {
					name = bc.Name
				}
			}
			if equalFoldName(name, ce.Name) {
				bc, isCol := c.Expr.(ColumnExpr)
				if !isCol || !partitioned(bc) {
					return nil, false
				}
				return bc, true
			}
		}
		return nil, false
	}
	var pushed []Expr
	for _, t := range splitTopLevelAnd(sel.Where) {
		plain := true
		if !r41Walk(t, func(e Expr) {
			switch e.(type) {
			case FuncExpr, LikeExpr, GlobExpr, MatchExpr:
				plain = false
			}
		}) || !plain {
			continue
		}
		rw, rok := r41Rewrite(t, colFor)
		if rok {
			pushed = append(pushed, rw)
		}
	}
	if len(pushed) == 0 {
		return body
	}
	nb := *body
	nb.Where = r41AndOnto(nb.Where, pushed)
	return &nb
}
