package engine

// This file implements WHERE-clause push-down: copying parent WHERE conditions
// into subqueries to guide their plans and decide materialization order.
// duplicates are removed, and C's answer is the one that reflects that.
//
// The parent keeps every term (pushDownWhereTerms duplicates, never moves), so
// the rows the parent sees are a subset it filters again. Terms are pushed in
// C's order -- the WHERE's AND-tree walked right to left (select.c:5205-5208),
// each appended to the subquery's WHERE (sqlite3ExprAnd, select.c:5289) -- so
// the subquery's WHERE carries them in the order C's planner analyses them.
//
// Only the subqueries C is certain NOT to flatten are pushed into, since for
// one it would flatten the flattener (flatten_projection.go) is the port and a
// push-down would be a different plan:
//
//   - a DISTINCT body (restriction (4));
//   - a UNION ALL compound under an aggregate or DISTINCT parent ((17d));
//   - a body keeping an ORDER BY under a parent that aggregates with something
//     other than count()/min()/max() ((16), with tag-select-0230 declining to
//     drop the ORDER BY).
//
// And only what can be reproduced EXACTLY is pushed. Wherever this cannot tell
// what C would push -- a name it cannot bind, a subquery in a term (pushed by C
// only when uncorrelated), a result column that is not a bare column (substExpr
// would wrap its copy in an implicit COLLATE this AST cannot spell), a compound
// arm whose column collation differs from the leftmost arm's -- NOTHING is
// pushed and the statement keeps its old plan, rather than half of C's.
// Restrictions (2), (3), (5), (6), (7), (8), (9), (10), (11) and (12) are each
// either checked below or unreachable for the shapes let through.
func (p *ReadOnlyPager) r41PushDown(stmt *SelectStmt) *SelectStmt {
	if p == nil || stmt == nil || len(stmt.From) == 0 || len(stmt.Compound) != 0 ||
		stmt.FromNestedParenJoin || r37cMayReferenceRowid(stmt) {
		return stmt
	}
	// The parent's WHERE as sqlite3ProcessJoin leaves it: the WHERE, then each
	// INNER join's ON clause ANDed on in FROM order (select.c:655-658). An
	// OUTER join's ON is there too, tagged EP_OuterON, which rule (5) of
	// sqlite3ExprIsSingleTableConstraint bars from every item that is not its
	// own right operand -- and no LEFT item is pushed into here.
	var conj []Expr
	leftOn := false
	conj = append(conj, splitTopLevelAnd(stmt.Where)...)
	for _, it := range stmt.From {
		if it.Join == JoinRight || it.Join == JoinFull || len(it.Using) != 0 || it.Natural ||
			it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias || len(it.NestedGroupSpan) != 0 ||
			it.NestFromWrapDepth != 0 || it.NestedNonLeading || it.UpdateTarget || it.TableFunc {
			return stmt
		}
		if it.Join != JoinLeft {
			conj = append(conj, splitTopLevelAnd(it.On)...)
		} else if it.On != nil {
			leftOn = true
		}
	}
	if len(conj) == 0 && !leftOn {
		return stmt
	}
	out := stmt
	for i := range stmt.From {
		it, ok := p.r41PushInto(stmt, i, conj)
		if !ok {
			continue
		}
		if out == stmt {
			cp := *stmt
			cp.From = append([]FromItem(nil), stmt.From...)
			out = &cp
		}
		out.From[i] = it
	}
	return out
}

// r41PushInto is pushDownWhereTerms for FROM item i: the item with its
// subquery's WHERE extended, or false when nothing is pushed.
func (p *ReadOnlyPager) r41PushInto(stmt *SelectStmt, i int, conj []Expr) (FromItem, bool) {
	it := stmt.From[i]
	// The right operand of a LEFT JOIN takes only the terms of its OWN ON
	// clause (sqlite3ExprIsSingleTableConstraint rule (4), expr.c:2763-2767);
	// any other item takes none of an outer join's ON terms (rule (5)).
	left := it.Join == JoinLeft
	if !left && it.Join != JoinInner && it.Join != JoinCross {
		return FromItem{}, false
	}
	if left {
		conj = splitTopLevelAnd(it.On)
		if len(conj) == 0 {
			return FromItem{}, false
		}
	}
	if it.Subquery == nil {
		// A view reference is this same subquery once sqlite3SelectExpand has
		// run. A CTE is not pushed into: tag-select-0420 skips one referenced
		// twice (nUse>=2), a count this package does not keep.
		ex, ok := p.r37cExpandViewRef(it)
		if !ok {
			return FromItem{}, false
		}
		it = ex
	}
	body := it.Subquery
	if body.Limit != nil || body.LimitParam != nil || body.Offset != nil || body.OffsetParam != nil || // (3)
		body.ValuesArms != 0 || len(body.Windows) != 0 { // (11), (6)
		return FromItem{}, false
	}
	arms := []*SelectStmt{body}
	for _, a := range body.Compound {
		if a.Op != "UNION ALL" || a.Stmt == nil { // (8) is never needed for UNION ALL
			return FromItem{}, false
		}
		arms = append(arms, a.Stmt)
	}
	switch {
	case len(arms) > 1:
		if len(body.OrderBy) != 0 || len(body.CTEs) != 0 {
			return FromItem{}, false
		}
		// (17f): a compound is never flattened into an outer join.
		if !left && !p.r41CompoundNotFlattened(stmt, arms) {
			return FromItem{}, false
		}
	case body.Distinct:
	case left:
		// C flattens a plain right operand of a LEFT JOIN (isOuterJoin,
		// select.c:4373-4385) unless the parent is DISTINCT (3d).
		if !stmt.Distinct {
			return FromItem{}, false
		}
	case r41PlainBodyNotFlattened(stmt, i, body):
	default:
		return FromItem{}, false
	}
	for _, a := range arms {
		if len(a.GroupBy) != 0 || a.Having != nil || selectIsAggregateQuery(a) || len(a.Columns) != len(body.Columns) {
			return FromItem{}, false
		}
		for _, c := range a.Columns {
			if c.Star || exprHasWindow(c.Expr) { // "*" positions are unknown here; (6)
				return FromItem{}, false
			}
		}
	}

	// The subquery's column names, from its leftmost arm.
	byName := map[string]int{}
	for k, c := range body.Columns {
		name := c.Text
		switch {
		case c.HasAlias:
			name = c.Alias
		default:
			if ce, isCol := c.Expr.(ColumnExpr); isCol {
				name = ce.Name
			}
		}
		l := r33sFoldIdent(name)
		if _, dup := byName[l]; dup {
			return FromItem{}, false
		}
		byName[l] = k
	}
	others := map[string]bool{}
	scopes := map[string]bool{}
	for j, o := range stmt.From {
		if j == i {
			continue
		}
		name, cols, ok := p.r41ItemNames(o)
		if !ok {
			return FromItem{}, false
		}
		if name != "" {
			scopes[r33sFoldIdent(name)] = true
		}
		for _, c := range cols {
			others[r33sFoldIdent(c)] = true
		}
	}
	if it.Alias != "" && scopes[r33sFoldIdent(it.Alias)] {
		return FromItem{}, false
	}

	// Collations, for a compound: substExpr gives a copy whose collation is not
	// the LEFTMOST arm's an implicit COLLATE (select.c:3857-3870).
	var armColls [][]string
	if len(arms) > 1 {
		for _, a := range arms {
			colls, ok := p.r41ArmCollations(a)
			if !ok {
				return FromItem{}, false
			}
			armColls = append(armColls, colls)
		}
	}

	pushed := make([][]Expr, len(arms))
	for t := len(conj) - 1; t >= 0; t-- {
		refs, eligible, certain := r41SingleItemTerm(conj[t], it.Alias, byName, others, scopes)
		if !certain {
			return FromItem{}, false
		}
		if !eligible {
			continue
		}
		for a, arm := range arms {
			for k := range refs {
				ce, isCol := arm.Columns[k].Expr.(ColumnExpr)
				if !isCol || ce.UsingRepr || ce.UsingPinned {
					return FromItem{}, false
				}
				if len(arms) > 1 && armColls[a][k] != armColls[0][k] {
					return FromItem{}, false
				}
			}
			term, ok := r41Rewrite(conj[t], func(ce ColumnExpr) (Expr, bool) {
				if k, ok := r41TermRef(ce, it.Alias, byName); ok {
					return arm.Columns[k].Expr, true
				}
				return ce, true
			})
			if !ok {
				return FromItem{}, false
			}
			pushed[a] = append(pushed[a], term)
		}
	}
	if len(pushed[0]) == 0 {
		return FromItem{}, false
	}
	nb := *arms[0]
	nb.Where = r41AndOnto(nb.Where, pushed[0])
	if len(arms) > 1 {
		nb.Compound = append([]CompoundArm(nil), body.Compound...)
		for a := 1; a < len(arms); a++ {
			na := *arms[a]
			na.Where = r41AndOnto(na.Where, pushed[a])
			nb.Compound[a-1].Stmt = &na
		}
	}
	it.Subquery = &nb
	return it, true
}

// r41PlainBodyNotFlattened reports that flattenSubquery certainly refuses the
// plain (not DISTINCT, not compound) body of FROM item i: a window function in
// the parent (25), or a body ORDER BY that tag-select-0230 keeps
// (select.c:7811-7848) and that then blocks the flatten by (11) or (16), or by
// the complex-result rule (select.c:7869-7876).
func r41PlainBodyNotFlattened(stmt *SelectStmt, i int, body *SelectStmt) bool {
	for _, c := range stmt.Columns {
		if !c.Star && exprHasWindow(c.Expr) {
			return true
		}
	}
	for _, t := range stmt.OrderBy {
		if exprHasWindow(t.Expr) {
			return true
		}
	}
	if len(body.OrderBy) == 0 {
		return false
	}
	if (len(stmt.OrderBy) != 0 || len(stmt.From) > 1) && body.Limit == nil && body.LimitParam == nil && !r41OrderAgg(stmt) {
		return false // the ORDER BY is dropped, and the body flattened
	}
	return len(stmt.OrderBy) != 0 || selectIsAggregateQuery(stmt) ||
		i == 0 && r41ComplexResult(stmt) &&
			(len(stmt.From) == 1 || stmt.From[1].CrossKeyword || stmt.From[1].Join == JoinLeft)
}

// r41CompoundNotFlattened reports that flattenSubquery certainly refuses the
// UNION ALL body whose arms are given -- by (17d) (an aggregate or DISTINCT
// parent), (17b) (a DISTINCT arm; an aggregate one is refused by the caller),
// (25) (a window function in the parent) or (17h) (two arms disagree on a
// column's affinity, compoundHasDifferentAffinities, select.c:4092). Any other
// compound C may well flatten, and then a push-down is not what it plans.
func (p *ReadOnlyPager) r41CompoundNotFlattened(stmt *SelectStmt, arms []*SelectStmt) bool {
	if selectIsAggregateQuery(stmt) || stmt.Distinct {
		return true
	}
	for _, c := range stmt.Columns {
		if !c.Star && exprHasWindow(c.Expr) {
			return true
		}
	}
	for _, a := range arms {
		if a.Distinct {
			return true
		}
	}
	core := *arms[0]
	core.Compound = nil
	aff0, ok := p.r37cArmAffinities(&core)
	if !ok {
		return false
	}
	for _, a := range arms[1:] {
		affs, aok := p.r37cArmAffinities(a)
		if !aok || len(affs) != len(aff0) {
			return false
		}
		for k := range affs {
			if affs[k] != aff0[k] {
				return true
			}
		}
	}
	return false
}

// r41ArmCollations is each bare-column result of a compound arm's declared
// collating sequence, folded ("binary" when it declares none), and "" for a
// result that is not a bare column.
func (p *ReadOnlyPager) r41ArmCollations(a *SelectStmt) ([]string, bool) {
	jts, _, err := p.resolveFrom(a.From, nil)
	if err != nil {
		return nil, false
	}
	ctx := &evalCtx{tables: buildScopes(jts)}
	colls := make([]string, len(a.Columns))
	for k, c := range a.Columns {
		ce, isCol := c.Expr.(ColumnExpr)
		if !isCol {
			continue
		}
		name, ok := columnDeclaredCollation(ctx, ce)
		if !ok || name == "" {
			name = "BINARY"
		}
		colls[k] = r33sFoldIdent(name)
	}
	return colls, true
}

// r41AndOnto is sqlite3ExprAnd(pSubq->pWhere, pNew) once per pushed term, in
// push order.
func r41AndOnto(w Expr, terms []Expr) Expr {
	for _, t := range terms {
		if w == nil {
			w = t
			continue
		}
		w = BinaryExpr{Op: "AND", L: w, R: t}
	}
	return w
}

// r41TermRef reports which subquery column ce names, if it names one.
func r41TermRef(ce ColumnExpr, dName string, byName map[string]int) (int, bool) {
	if ce.Qualifier != "" && (dName == "" || !equalFoldName(ce.Qualifier, dName)) {
		return 0, false
	}
	k, ok := byName[r33sFoldIdent(ce.Name)]
	return k, ok
}

// r41SingleItemTerm is sqlite3ExprIsSingleTableConstraint (expr.c:2753) on one
// parent conjunct, for an item that is not an outer join's operand: the term
// must reference no other FROM item and no enclosing query, and call no
// function that is not SQLITE_FUNC_CONSTANT or SQLITE_FUNC_SLOCHNG
// (exprNodeIsConstant, expr.c:2541, with pParse==0 aborting on anything else).
// refs is the set of subquery columns it reads. certain is false wherever C's
// answer cannot be known here -- then the caller pushes nothing.
func r41SingleItemTerm(e Expr, dName string, byName map[string]int, others, scopes map[string]bool) (refs map[int]bool, eligible, certain bool) {
	refs = map[int]bool{}
	eligible, certain = true, true
	var walk func(Expr)
	walk = func(e Expr) {
		if e == nil || !certain {
			return
		}
		switch x := e.(type) {
		case LiteralExpr, ParamExpr:
		case ColumnExpr:
			if x.Schema != "" || x.UsingRepr || x.UsingPinned {
				certain = false
				return
			}
			l := r33sFoldIdent(x.Name)
			if x.Qualifier != "" {
				if dName != "" && equalFoldName(x.Qualifier, dName) {
					k, ok := byName[l]
					if !ok {
						certain = false
						return
					}
					refs[k] = true
					return
				}
				if !scopes[r33sFoldIdent(x.Qualifier)] {
					certain = false // an enclosing query's, or no such table
				}
				eligible = false
				return
			}
			if k, ok := byName[l]; ok {
				if others[l] {
					certain = false // ambiguous
					return
				}
				refs[k] = true
				return
			}
			if others[l] {
				eligible = false
				return
			}
			certain = false // a result alias, an enclosing query's column or a string
		case FuncExpr:
			if x.Over != nil || isAggregateCall(x) {
				certain = false
				return
			}
			if nonConstantFuncs[r33sFoldIdent(x.Name)] {
				eligible = false
			}
			for _, a := range x.walkArgs() {
				walk(a)
			}
			walk(x.Filter)
		case MatchExpr:
			walk(x.X)
			walk(x.Pattern)
		case RaiseExpr:
			eligible = false // TK_RAISE aborts exprNodeIsConstant
		case SubqueryExpr, ExistsExpr:
			certain = false // pushed only when uncorrelated (bAllowSubq)
		case InExpr:
			if x.Sub != nil {
				certain = false
				return
			}
			walk(x.X)
			for _, a := range x.List {
				walk(a)
			}
		default:
			if !r41Walk(e, func(Expr) {}) {
				certain = false
				return
			}
			walkExprOperands(e, walk)
		}
	}
	walk(e)
	return refs, eligible, certain
}
