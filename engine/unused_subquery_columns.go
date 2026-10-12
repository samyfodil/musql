package engine

import "reflect"

// nullUnusedSubqueryColumns is disableUnusedSubqueryResultColumns (select.c:5305),
// applied from sqlite3Select before a FROM subquery's code is generated
// (select.c:8038): a result column of a subquery that the enclosing query never
// names is changed to NULL, so computing it -- and any error it would raise --
// does not happen. "SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT
// abs(-9223372036854775807-1) FROM t)" is the count in C, not "integer
// overflow".
//
// C's conditions, all kept:
//
//   - the item is not correlated and not a CTE reference;
//   - no arm is DISTINCT or an aggregate query, none has a window function, and
//     every compound operator is UNION ALL;
//   - a column the subquery's own ORDER BY names by ordinal stays.
//
// colUsed is C's SrcItem.colUsed: every column of the item that any expression
// of the enclosing query names, a "*" naming them all. It is over-approximated
// here by NAME across the whole enclosing statement (nested subqueries
// included), which can only keep a column that C would blank, never blank one
// C keeps.
//
// Narrower than C, each a reason to leave the subquery alone: a "*" or a
// subquery in an arm's select list, an ORDER BY term that is not an ordinal, a
// WITH, a multi-row VALUES, a NATURAL join, a table-valued function or a
// parenthesized join group anywhere in the FROM, and a subquery whose columns
// cannot be derived without running it. Only a column that can raise or has an
// effect (neither a literal nor a plain column reference) is changed.
//
// The column keeps its name: an unaliased one is given its derived name as an
// alias, so the ":N" suffixes of its siblings do not shift.
//
// stmt is not modified; the same *SelectStmt is compiled again per enclosing
// row for a correlated subquery. It returns stmt itself when nothing changes.
func (p *ReadOnlyPager) nullUnusedSubqueryColumns(stmt *SelectStmt) *SelectStmt {
	if p == nil || stmt == nil || len(stmt.From) == 0 {
		return stmt
	}
	// Cheap gate first: some derived table with a column worth blanking.
	candidate := false
	subs := make([]FromItem, len(stmt.From)) // each item as a derived table, or zero
	for i, it := range stmt.From {
		if it.TableFunc || it.Natural || it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias ||
			it.NestedGroupID != 0 || len(it.NestedGroupSpan) != 0 {
			return stmt
		}
		d, ok := p.fromItemAsDerived(it)
		if ok && unusedColumnsEligible(d.Subquery) && hasBlankableColumn(d.Subquery) {
			subs[i] = d
			candidate = true
		}
	}
	if stmt.FromNestedParenJoin || !candidate {
		return stmt
	}
	refs := newColumnRefSet()
	skip := make(map[uintptr]bool, len(stmt.From))
	for _, it := range stmt.From {
		if it.Subquery != nil {
			skip[reflect.ValueOf(it.Subquery).Pointer()] = true // uncorrelated: it names nothing of this query
		}
	}

	for _, c := range stmt.Columns {
		if c.Star {
			refs.star(c.StarQualifier)
		}
	}
	refs.walk(reflect.ValueOf(stmt), skip, map[uintptr]bool{})
	for _, it := range stmt.From {
		for _, u := range it.Using {
			refs.name("", u)
		}
	}

	var out *SelectStmt
	for i, it := range stmt.From {
		d := subs[i]
		sub := d.Subquery
		if sub == nil {
			continue
		}
		it = d
		cols, ok := p.derivedSchemaColsOnly(sub)
		if !ok || len(cols) != len(sub.Columns) {
			continue
		}
		display := it.Alias
		if display == "" {
			display = it.Table
		}
		unused := make([]bool, len(cols))
		anyUnused := false
		for j, ci := range cols {
			if !refs.used(display, ci.Name) && !orderByNamesColumn(sub, ci.Name, j) {
				unused[j] = true
				anyUnused = true
			}
		}
		if !anyUnused {
			continue
		}
		blanked := blankColumns(sub, cols, unused)
		if blanked == sub {
			continue
		}
		if out == nil {
			cp := *stmt
			cp.From = append([]FromItem(nil), stmt.From...)
			out = &cp
		}
		out.From[i] = d
		out.From[i].Subquery = blanked
	}
	if out == nil {
		return stmt
	}
	return out
}

// fromItemAsDerived is the FROM item as the derived table sqlite3SelectExpand
// makes of it: a subquery as written, or a stored view through the flattener's
// expander, which refuses every shape whose columns or errors the expansion
// would change. A CTE reference is not: C leaves those alone (pItem->fg.isCte).
func (p *ReadOnlyPager) fromItemAsDerived(it FromItem) (FromItem, bool) {
	if it.Subquery != nil {
		return it, true
	}
	if it.Table == "" {
		return FromItem{}, false
	}
	return p.r37cExpandViewRef(it)
}

// derivedSchemaColsOnly is derivedSchemaCols outside the schema-only retry: the
// subquery's columns from its compiled body, nothing run. A body that does not
// compile on its own (a correlated one, or one this engine declines) answers
// false.
func (p *ReadOnlyPager) derivedSchemaColsOnly(sub *SelectStmt) ([]columnInfo, bool) {
	saved := p.derivedSchemaOnly
	p.derivedSchemaOnly = true
	defer func() { p.derivedSchemaOnly = saved }()
	return p.derivedSchemaCols(sub)
}

// unusedColumnsEligible is C's list of subqueries disableUnusedSubqueryResultColumns
// leaves alone, plus this port's narrower refusals (see nullUnusedSubqueryColumns).
func unusedColumnsEligible(sub *SelectStmt) bool {
	if len(sub.CTEs) != 0 || sub.ValuesArms != 0 {
		return false
	}
	n := len(sub.Columns)
	arm := sub
	for k := 0; ; k++ {
		if arm == nil || arm.Distinct || len(arm.GroupBy) != 0 || arm.Having != nil ||
			len(arm.Windows) != 0 || arm.ValuesArms != 0 || len(arm.CTEs) != 0 ||
			len(arm.Columns) != n {
			return false
		}
		for _, c := range arm.Columns {
			if c.Star || exprContainsSubquery(c.Expr) || containsAggregate(c.Expr) || exprHasWindow(c.Expr) {
				return false
			}
		}
		for _, o := range arm.OrderBy {
			if containsAggregate(o.Expr) || exprHasWindow(o.Expr) {
				return false
			}
		}
		if k >= len(sub.Compound) {
			return true
		}
		a := sub.Compound[k]
		if a.Op != "UNION ALL" {
			return false
		}
		arm = a.Stmt
	}
}

// hasBlankableColumn reports whether some column of some arm is neither a
// literal nor a plain column reference, the only expressions worth blanking.
func hasBlankableColumn(sub *SelectStmt) bool {
	arms := []*SelectStmt{sub}
	for _, a := range sub.Compound {
		arms = append(arms, a.Stmt)
	}
	for _, arm := range arms {
		if arm == nil {
			continue
		}
		for _, c := range arm.Columns {
			switch c.Expr.(type) {
			case LiteralExpr, ColumnExpr, ParamExpr:
			default:
				return true
			}
		}
	}
	return false
}

// orderByNamesColumn reports whether the subquery's own ORDER BY may read result
// column j (named name) instead of evaluating its own expression: by ordinal
// (iOrderByCol, select.c:5337), by the column's name or an arm's alias for it,
// or by an expression equal to the column's. Over-approximate on purpose; a
// column kept is only a column not skipped.
func orderByNamesColumn(sub *SelectStmt, name string, j int) bool {
	arms := []*SelectStmt{sub}
	for _, a := range sub.Compound {
		arms = append(arms, a.Stmt)
	}
	for _, o := range sub.OrderBy {
		t := stripOrderCollate(o.Expr)
		if n, ok := orderByOrdinal(t); ok {
			if n == int64(j+1) {
				return true
			}
			continue
		}
		if ce, ok := t.(ColumnExpr); ok && ce.Qualifier == "" &&
			(equalFoldName(ce.Name, name) || equalFoldName(ce.Name, r32mTrimDupSuffix(name))) {
			return true
		}
		for _, arm := range arms {
			if arm == nil || j >= len(arm.Columns) || arm.Columns[j].Star {
				continue
			}
			c := arm.Columns[j]
			if exprEqual(t, c.Expr) {
				return true
			}
			if ce, ok := t.(ColumnExpr); ok && ce.Qualifier == "" && c.HasAlias && equalFoldName(ce.Name, c.Alias) {
				return true
			}
		}
	}
	return false
}

// blankColumns returns sub with each unused column of every arm replaced by
// NULL, or sub itself when no replacement applies. Names are pinned on the
// first arm (the only one that names the columns).
func blankColumns(sub *SelectStmt, cols []columnInfo, unused []bool) *SelectStmt {
	changed := false
	rewrite := func(arm *SelectStmt, first bool) *SelectStmt {
		var cs []SelectColumn
		for j, c := range arm.Columns {
			if !unused[j] {
				continue
			}
			switch c.Expr.(type) {
			case LiteralExpr, ColumnExpr, ParamExpr:
				continue
			}
			if cs == nil {
				cs = append([]SelectColumn(nil), arm.Columns...)
			}
			c.Expr = LiteralExpr{Val: Value{Typ: Null}}
			if first && !c.HasAlias {
				c.Alias, c.HasAlias = cols[j].Name, true
			}
			cs[j] = c
		}
		if cs == nil {
			return arm
		}
		changed = true
		cp := *arm
		cp.Columns = cs
		return &cp
	}
	out := rewrite(sub, true)
	if len(sub.Compound) != 0 {
		arms := make([]CompoundArm, len(sub.Compound))
		for k, a := range sub.Compound {
			arms[k] = a
			arms[k].Stmt = rewrite(a.Stmt, false)
		}
		if out == sub {
			cp := *sub
			out = &cp
		}
		out.Compound = arms
	}
	if !changed {
		return sub
	}
	return out
}

// columnRefSet is the names an enclosing statement uses, for colUsed.
type columnRefSet struct {
	all     bool            // a bare "*"
	stars   map[string]bool // "t.*" qualifiers, folded
	unqual  map[string]bool // names written without a qualifier, folded
	qualify map[string]map[string]bool
}

func newColumnRefSet() *columnRefSet {
	return &columnRefSet{stars: map[string]bool{}, unqual: map[string]bool{}, qualify: map[string]map[string]bool{}}
}

func (s *columnRefSet) star(q string) {
	if q == "" {
		s.all = true
		return
	}
	s.stars[r33sFoldIdent(q)] = true
}

func (s *columnRefSet) name(q, n string) {
	n = r33sFoldIdent(n)
	if q == "" {
		s.unqual[n] = true
		return
	}
	q = r33sFoldIdent(q)
	if s.qualify[q] == nil {
		s.qualify[q] = map[string]bool{}
	}
	s.qualify[q][n] = true
}

// used reports whether a column called name of the FROM item displayed as item
// may be named by the statement.
func (s *columnRefSet) used(item, name string) bool {
	if s.all {
		return true
	}
	item, name = r33sFoldIdent(item), r33sFoldIdent(name)
	if item != "" && s.stars[item] {
		return true
	}
	if s.unqual[name] {
		return true
	}
	return item != "" && s.qualify[item][name]
}

var columnExprType = reflect.TypeOf(ColumnExpr{})

// walk records every ColumnExpr reachable from v. It is reflective so that no
// expression kind, present or added later, can hide a name: a miss here would
// blank a column the query reads. Subqueries named in skip are not entered.
func (s *columnRefSet) walk(v reflect.Value, skip map[uintptr]bool, seen map[uintptr]bool) {
	switch v.Kind() {
	case reflect.Interface:
		if !v.IsNil() {
			s.walk(v.Elem(), skip, seen)
		}
	case reflect.Ptr:
		if v.IsNil() {
			return
		}
		if skip[v.Pointer()] || seen[v.Pointer()] {
			return
		}
		seen[v.Pointer()] = true
		s.walk(v.Elem(), skip, seen)
	case reflect.Struct:
		if v.Type() == columnExprType {
			s.name(v.FieldByName("Qualifier").String(), v.FieldByName("Name").String())
			return
		}
		for i := 0; i < v.NumField(); i++ {
			s.walk(v.Field(i), skip, seen)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			s.walk(v.Index(i), skip, seen)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			s.walk(it.Value(), skip, seen)
		}
	}
}

// existsBlankedStmt is stmt as EXISTS runs it, or nil when that is stmt itself.
// EXISTS delivers its rows to SRT_Exists: selectInnerLoop codes no result
// expression for it (select.c:1202), and sqlite3Select drops its DISTINCT and
// ORDER BY as superfluous (IgnorableDistinct, select.c:7637). So "EXISTS(SELECT
// abs(-9223372036854775807-1) FROM t ORDER BY 1)" is true wherever t has a row.
// Narrower than C: only a plain select (no compound, aggregate, window or GROUP
// BY, which read the list for other reasons). The caller has already compiled
// stmt unchanged, so unknown names have been reported.
func existsBlankedStmt(stmt *SelectStmt) *SelectStmt {
	if stmt == nil || len(stmt.Compound) != 0 || len(stmt.GroupBy) != 0 ||
		stmt.Having != nil || len(stmt.Windows) != 0 || len(stmt.CTEs) != 0 || stmt.ValuesArms != 0 {
		return nil
	}
	for _, o := range stmt.OrderBy {
		if containsAggregate(o.Expr) || exprHasWindow(o.Expr) {
			return nil
		}
	}
	changed := stmt.Distinct || len(stmt.OrderBy) != 0
	var cs []SelectColumn
	for j, c := range stmt.Columns {
		if c.Star {
			continue
		}
		if c.Expr == nil || containsAggregate(c.Expr) || exprHasWindow(c.Expr) {
			return nil
		}
		switch c.Expr.(type) {
		case LiteralExpr, ColumnExpr, ParamExpr:
			continue
		}
		if cs == nil {
			cs = append([]SelectColumn(nil), stmt.Columns...)
		}
		c.Expr = LiteralExpr{Val: Value{Typ: Null}}
		cs[j] = c
		changed = true
	}
	if !changed {
		return nil
	}
	cp := *stmt
	if cs != nil {
		cp.Columns = cs
	}
	cp.Distinct = false
	cp.OrderBy = nil
	return &cp
}
