package engine

import "fmt"

// r41FlattenMultiItem is flattenSubquery for a body r41Arm does not take: a
// join of several FROM items, or one carrying its own WITH -- taken only for a
// body calling random() or randomblob(), where leaving it a subquery is the
// wrong answer rather than only another plan. The body's FROM items move into
// the parent in its place, the first taking its join (select.c:4535-4575);
// its WHERE is ANDed onto the parent's; every parent reference to one of its
// columns becomes a copy of that column's expression (substExpr,
// select.c:3797); and a WITH it carries moves up with it, so the CTE
// references keep naming the definitions they named.
//
// Refused: anything whose names could bind differently once moved -- a body
// item without a name of its own, a name the parent or a CTE in scope already
// uses, an unqualified column more than one body item supplies or none does,
// a subquery inside a moved expression, a rowid -- and every shape cFlattens
// does not answer certainly "flattened".
func (p *ReadOnlyPager) r41FlattenMultiItem(stmt *SelectStmt, i int, expand bool) (*SelectStmt, bool) {
	it := stmt.From[i]
	var body *SelectStmt
	var colNames []string
	// Where the body's own FROM names resolve: here, for an inline subquery;
	// in its definition's scope, for a CTE (owner); against the schema alone,
	// for a view (isView).
	var owner *cteBinding
	isView := false
	switch {
	case it.Subquery != nil:
		body = it.Subquery
	case !expand:
		return nil, false
	default:
		if b, ok := p.lookupCTE(it.Table); ok && it.Schema == "" {
			if b.core == nil || b.recursive != nil || b.selfRef != nil || b.m10d == cteM10dYes ||
				b.scopeDepth != len(p.cteScopes) {
				return nil, false
			}
			body, colNames, owner = b.core, b.colNames, b
		} else {
			isView = true
			pcv, ok := p.r37cViewBody(it)
			if !ok {
				return nil, false
			}
			body, colNames = pcv.selectStmt, pcv.colNames
		}
	}
	if body == nil || !p.cVolatile(body) || it.Join != JoinInner && it.Join != JoinCross ||
		len(it.Using) != 0 || it.Natural || it.IndexedBy != "" || it.NotIndexed {
		return nil, false
	}
	if flat, certain := p.cFlattens(stmt, stmt.From, i); !flat || !certain {
		return nil, false
	}
	// cFlattens has answered for the C side; what remains is what this
	// rewrite can spell.
	if len(body.Compound) != 0 || body.Distinct || len(body.GroupBy) != 0 || body.Having != nil ||
		body.Limit != nil || body.LimitParam != nil || body.Offset != nil || body.OffsetParam != nil ||
		len(body.Windows) != 0 || body.ValuesArms != 0 || body.FromNestedParenJoin || len(body.From) == 0 ||
		r37cMayReferenceRowid(body) {
		return nil, false
	}
	// tag-select-0230 deletes a body ORDER BY that accomplishes nothing; one
	// that survives would move onto the parent, which this does not spell.
	if len(body.OrderBy) != 0 && !((len(stmt.OrderBy) != 0 || len(stmt.From) > 1) && !r41OrderAgg(stmt) && !stmt.hasUpdateTarget()) {
		return nil, false
	}
	for _, d := range body.CTEs {
		if d.Select == nil || len(d.Select.Compound) != 0 && cteSelfReferences(d) {
			return nil, false
		}
	}

	// The CTE names the body defines, and every name the parent's statement
	// tree or the scope already knows, which a moved definition must not
	// shadow and a moved FROM item must not be captured by.
	bodyCTE := map[string]*CTEDef{}
	for k := range body.CTEs {
		bodyCTE[r33sFoldIdent(body.CTEs[k].Name)] = &body.CTEs[k]
	}
	for name := range bodyCTE {
		if _, vis := p.lookupCTE(name); vis || selectNamesFromItem(stmt, name) {
			return nil, false
		}
		for _, d := range stmt.CTEs {
			if equalFoldName(d.Name, name) {
				return nil, false
			}
		}
	}

	// Each body item's name and column names.
	type bodyItem struct {
		name string
		cols map[string]bool
	}
	parentNames := map[string]bool{}
	for j, o := range stmt.From {
		if j == i {
			continue
		}
		name, _, ok := p.r41ItemNames(o)
		if !ok {
			return nil, false
		}
		if name != "" {
			parentNames[r33sFoldIdent(name)] = true
		}
	}
	cteDefs := map[string]*CTEDef{}
	for k, v := range bodyCTE {
		cteDefs[k] = v
	}
	var items []bodyItem
	var itemAlias []string
	for _, b := range body.From {
		if b.TableFunc || b.GroupLen != 0 || len(b.NestedGroupSpan) != 0 || b.Join != JoinInner && b.Join != JoinCross ||
			len(b.Using) != 0 || b.Natural || b.IndexedBy != "" || b.NotIndexed {
			return nil, false
		}
		name := b.Alias
		var cols map[string]bool
		switch {
		case b.Subquery != nil:
			if name == "" {
				// An unaliased subquery's own name ("subquery-N") is one no
				// reference can spell; a name nothing else uses stands in.
				for n := 1; ; n++ {
					name = fmt.Sprintf("sqlite_flat_sub_%d", n)
					if !parentNames[r33sFoldIdent(name)] && !selectNamesFromItem(stmt, name) {
						break
					}
				}
			}
			names, ok := r36dDerivedNames(p, b.Subquery, nil, cteDefs, 1)
			if !ok {
				return nil, false
			}
			cols = names
		default:
			if name == "" {
				name = b.Table
			}
			if d, ok := bodyCTE[r33sFoldIdent(b.Table)]; ok && b.Schema == "" {
				names, ok := r36dDerivedNames(p, d.Select, d.ColNames, cteDefs, 1)
				if !ok {
					return nil, false
				}
				cols = names
				break
			}
			if here, vis := p.lookupCTE(b.Table); vis && b.Schema == "" {
				// Moved into the parent, the name binds to the parent's
				// definition; it must be the one it bound to in the body.
				if isView {
					return nil, false
				}
				if owner != nil {
					var there *cteBinding
					func() {
						defer p.enterCTEBodyScope(it.Table, owner)()
						there, _ = p.lookupCTE(b.Table)
					}()
					if there != here {
						return nil, false
					}
				}
				if here.core == nil || here.recursive != nil || here.selfRef != nil {
					return nil, false
				}
				names, ok := r36dDerivedNames(p, here.core, here.colNames, nil, 1)
				if !ok {
					return nil, false
				}
				cols = names
				break
			}
			cb := b
			cb.On = nil
			tbl, ok := p.r37cOrdinaryBase(cb)
			if !ok {
				// A view: its name binds against the schema from the parent
				// as it did from the body (no CTE in scope shadows it, checked
				// above), and its columns are its body's.
				pcv, isV, verr := p.resolveViewByNameIn(fromItemScope(b), b.Table)
				if verr != nil || !isV || pcv == nil {
					return nil, false
				}
				names, ok := r36dDerivedNames(p, pcv.selectStmt, pcv.colNames, nil, 1)
				if !ok {
					return nil, false
				}
				cols = names
				break
			}
			cols = map[string]bool{}
			for _, c := range tbl.cols {
				if c.Hidden {
					return nil, false
				}
				cols[r33sFoldIdent(c.Name)] = true
			}
		}
		l := r33sFoldIdent(name)
		if parentNames[l] {
			return nil, false
		}
		for _, o := range items {
			if o.name == l {
				return nil, false
			}
		}
		items = append(items, bodyItem{name: l, cols: cols})
		itemAlias = append(itemAlias, name)
	}

	// Body expressions, every column qualified by the body item it binds to.
	qualify := func(ce ColumnExpr) (Expr, bool) {
		if ce.Schema != "" || ce.UsingRepr || ce.UsingPinned {
			return nil, false
		}
		l := r33sFoldIdent(ce.Name)
		if ce.Qualifier != "" {
			q := r33sFoldIdent(ce.Qualifier)
			for _, o := range items {
				if o.name == q && o.cols[l] {
					return ce, true
				}
			}
			return nil, false
		}
		found := ""
		for _, o := range items {
			if o.cols[l] {
				if found != "" {
					return nil, false
				}
				found = o.name
			}
		}
		if found == "" {
			return nil, false
		}
		return ColumnExpr{Qualifier: found, Name: ce.Name}, true
	}
	f := &r41Flat{
		dName:   it.Alias,
		byName:  map[string]int{},
		base:    map[string]string{},
		others:  map[string]bool{},
		scopes:  parentNames,
		aliases: map[string]bool{},
	}
	if f.dName == "" && it.Subquery == nil {
		f.dName = it.Table
	}
	for _, o := range items {
		for c := range o.cols {
			f.base[c] = c
		}
	}
	for j, o := range stmt.From {
		if j == i {
			continue
		}
		_, cols, _ := p.r41ItemNames(o)
		for _, c := range cols {
			f.others[r33sFoldIdent(c)] = true
		}
	}
	for n := 1; ; n++ {
		f.alias = fmt.Sprintf("sqlite_flat_%d", n)
		if !parentNames[r33sFoldIdent(f.alias)] {
			break
		}
	}
	for _, c := range stmt.Columns {
		if !c.Star && c.HasAlias {
			f.aliases[r33sFoldIdent(c.Alias)] = true
		}
	}
	for _, c := range body.Columns {
		if c.Star || containsAggregate(c.Expr) || exprHasWindow(c.Expr) {
			return nil, false
		}
		rw, ok := r41Rewrite(c.Expr, qualify)
		if !ok {
			return nil, false
		}
		dc := r41DerivedCol{name: c.Text, expr: rw}
		if c.HasAlias {
			dc.name = c.Alias
		} else if ce, isCol := c.Expr.(ColumnExpr); isCol {
			dc.name = ce.Name
		}
		_, dc.colRef = rw.(ColumnExpr)
		f.cols = append(f.cols, dc)
	}
	if colNames != nil {
		if len(colNames) != len(f.cols) {
			return nil, false
		}
		for k := range f.cols {
			f.cols[k].name = colNames[k]
		}
	}
	dnames := make([]string, len(f.cols))
	for k := range f.cols {
		dnames[k] = f.cols[k].name
	}
	for k, n := range trueFalseColumnNames(dnames) {
		f.cols[k].name = n
	}
	for k, dc := range f.cols {
		l := r33sFoldIdent(dc.name)
		if _, dup := f.byName[l]; dup {
			return nil, false
		}
		f.byName[l] = k
	}
	var bodyWhere Expr
	if body.Where != nil {
		if containsAggregate(body.Where) {
			return nil, false
		}
		w, ok := r41Rewrite(body.Where, qualify)
		if !ok {
			return nil, false
		}
		bodyWhere = w
	}
	moved := make([]FromItem, len(body.From))
	for k, b := range body.From {
		if b.On != nil {
			on, ok := r41Rewrite(b.On, qualify)
			if !ok {
				return nil, false
			}
			b.On = on
		}
		b.Alias = itemAlias[k]
		cb := b
		cb.On = nil
		_, isBase := p.r37cOrdinaryBase(cb)
		b.flattened = b.Subquery == nil && isBase
		moved[k] = b
	}
	a := &r41ArmFlat{f: f, where: bodyWhere, p: p, multi: moved}
	out, ok := a.parent(stmt, i, it)
	if !ok {
		return nil, false
	}
	out.CTEs = append(append([]CTEDef(nil), stmt.CTEs...), body.CTEs...)
	return out, true
}

// cteSelfReferences reports a CTE definition whose body names the CTE itself:
// a recursive one, which does not move.
func cteSelfReferences(d CTEDef) bool {
	return selectNamesFromItem(d.Select, d.Name)
}

// selectNamesFromItem reports a FROM item named name anywhere in s's tree:
// its FROM clauses, its compound arms, its WITH bodies and the subqueries of
// its expressions.
func selectNamesFromItem(s *SelectStmt, name string) bool {
	if s == nil {
		return false
	}
	found := false
	var walkSel func(*SelectStmt)
	var walkExpr func(Expr)
	walkExpr = func(e Expr) {
		if e == nil || found {
			return
		}
		switch x := e.(type) {
		case SubqueryExpr:
			walkSel(x.Stmt)
			return
		case ExistsExpr:
			walkSel(x.Stmt)
			return
		case InExpr:
			if x.Sub != nil {
				walkSel(x.Sub)
			}
		}
		wherePlanWalkChildren(e, walkExpr)
	}
	walkSel = func(s *SelectStmt) {
		if s == nil || found {
			return
		}
		for _, d := range s.CTEs {
			walkSel(d.Select)
		}
		for _, it := range s.From {
			if it.Subquery != nil {
				walkSel(it.Subquery)
			} else if equalFoldName(it.Table, name) {
				found = true
				return
			}
			walkExpr(it.On)
			for _, a := range it.TableFuncArgs {
				walkExpr(a)
			}
		}
		for _, c := range s.Columns {
			if !c.Star {
				walkExpr(c.Expr)
			}
		}
		walkExpr(s.Where)
		walkExpr(s.Having)
		for _, g := range s.GroupBy {
			walkExpr(g)
		}
		for _, ot := range s.OrderBy {
			walkExpr(ot.Expr)
		}
		for _, a := range s.Compound {
			walkSel(a.Stmt)
		}
	}
	walkSel(s)
	return found
}
