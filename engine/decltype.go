// Result-column DECLARED TYPES answer for a statement's result set.
// This is used by drivers (e.g. time.Time coercion) and database/sql
// ColumnTypes().DatabaseTypeName(). The rule follows SQL standards with
// special handling for rowid and compound queries.
package engine

// ResultDeclTypes returns one declared type per result column ("" if untyped).
// It parses and resolves but never executes the statement.
func (p *ReadOnlyPager) ResultDeclTypes(sqlText string) (types []string, ok bool) {
	stmt, err := ParseSelect(sqlText)
	if err != nil || stmt == nil {
		return nil, false
	}
	return p.selectDeclTypes(stmt, 0)
}

// declTypeMaxDepth bounds recursion through derived tables and views.
const declTypeMaxDepth = 16

// selectDeclTypes is ResultDeclTypes over an already-parsed statement.
func (p *ReadOnlyPager) selectDeclTypes(stmt *SelectStmt, depth int) ([]string, bool) {
	if stmt == nil || depth > declTypeMaxDepth {
		return nil, false
	}
	// A COMPOUND reports its LEFTMOST arm's types.
	if len(stmt.Compound) > 0 {
		lead := *stmt
		lead.Compound = nil
		lead.OrderBy = nil
		lead.Limit, lead.LimitParam = nil, nil
		lead.Offset, lead.OffsetParam = nil, nil
		return p.selectDeclTypes(&lead, depth)
	}
	var scopes []tableScope
	if len(stmt.From) > 0 {
		if len(stmt.CTEs) > 0 {
			pop := p.pushCTEScope(stmt.CTEs)
			defer pop()
		}
		jts, _, err := p.resolveFrom(stmt.From, nil)
		if err != nil {
			return nil, false
		}
		scopes = buildScopes(jts)
	}
	outCols, err := expandSelectList(stmt.Columns, scopes, defaultColNameMode)
	if err != nil {
		return nil, false
	}
	nested := p.declTypeNestedSelects(stmt.From)
	types := make([]string, len(outCols))
	for i, oc := range outCols {
		t, found := p.viewColumnDeclType(scopes, oc.expr, 0)
		if !found {
			t = p.declTypeThroughNested(scopes, nested, oc.expr, depth)
		}
		types[i] = t
	}
	return types, true
}

// declTypeNestedSelects maps each FROM item that has a SELECT behind it -- a
// derived table, or a VIEW -- to that SELECT, keyed by the name a column
// reference into it resolves under (its alias, else the object's own name).
//
// This is select.c:1954-1955's "pTabList->a[j].fg.isSubquery" branch: C reaches
// the sub-select straight off the SrcItem, where this package has already
// collapsed the FROM into scopes that carry only columnInfo. Rebuilding the
// pairing by NAME is exact for every shape a decltype can be asked about, since
// a reference that resolved into the scope resolved by that same name.
func (p *ReadOnlyPager) declTypeNestedSelects(from []FromItem) map[string]*SelectStmt {
	var out map[string]*SelectStmt
	add := func(name string, sel *SelectStmt) {
		if name == "" || sel == nil {
			return
		}
		if out == nil {
			out = map[string]*SelectStmt{}
		}
		out[r33sFoldIdent(name)] = sel
	}
	for _, it := range from {
		switch {
		case it.Subquery != nil:
			add(it.Alias, it.Subquery)
			// An UNALIASED derived table's columns resolve bare, so it is keyed
			// under "" too -- looked up by declTypeThroughNested when the
			// reference carried no qualifier.
			if it.Alias == "" {
				add(declTypeBareKey, it.Subquery)
			}
		case it.Table != "":
			if v, ok := p.r37cViewBody(it); ok && v != nil {
				name := it.Alias
				if name == "" {
					name = it.Table
				}
				add(name, v.selectStmt)
				add(declTypeBareKey, v.selectStmt)
			}
		}
	}
	return out
}

// declTypeBareKey is declTypeNestedSelects' key for "the single FROM item a bare
// reference could have come from". A statement whose FROM holds more than one
// such item overwrites it, and declTypeThroughNested's answer for a bare
// reference there is whichever one landed last -- which is why it is only ever
// consulted after viewColumnDeclType has already failed to find a real column,
// i.e. when exactly one FROM item can own the name.
const declTypeBareKey = "\x00bare"

// declTypeThroughNested is select.c:1986-2005: a column reference whose source
// is a SUBQUERY or VIEW in the FROM reports the declared type of THAT SELECT's
// matching result column, recursively.
//
// Reached only when viewColumnDeclType already answered "no type", which for a
// derived scope it always does -- derivedColumnInfos (join.go) builds those
// columnInfos with a Name, affinity and collation but no declared type, since
// nothing inside the engine needed one.
func (p *ReadOnlyPager) declTypeThroughNested(scopes []tableScope, nested map[string]*SelectStmt, e Expr, depth int) string {
	if len(nested) == 0 || depth > declTypeMaxDepth {
		return ""
	}
	ce, isCol := e.(ColumnExpr)
	if !isCol {
		return ""
	}
	key := declTypeBareKey
	if ce.Qualifier != "" {
		key = r33sFoldIdent(ce.Qualifier)
	}
	sel, ok := nested[key]
	if !ok {
		return ""
	}
	// The POSITION within the nested select is what C uses (pExpr->iColumn), and
	// the scope's own colIndex is that position -- the two lists are built from
	// the same expansion.
	idx := -1
	lname := r33sFoldIdent(ce.Name)
	for _, ts := range scopes {
		if ce.Qualifier != "" && !equalFoldName(ts.name, ce.Qualifier) {
			continue
		}
		if i, found := ts.colIndex[lname]; found {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ""
	}
	inner, ok := p.selectDeclTypes(sel, depth+1)
	if !ok || idx >= len(inner) {
		return ""
	}
	return inner[idx]
}
