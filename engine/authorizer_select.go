package engine

import "strings"

// The resolution half of the authorizer: the column reads and function calls
// of a SELECT, in the order resolve.c resolves them, then the SELECT calls of
// its subqueries in the order select.c codes them.

// authItem is one FROM item, C's SrcItem.
type authItem struct {
	name    string     // what a qualifier matches: the alias, else the table name
	tab     *authTable // a table or view; nil for a subquery or CTE
	sub     *authSel   // a subquery, view or CTE body
	cols    []string   // a subquery's or CTE's column names
	merged  map[string]bool
	used    bool
	zdb     string // the schema as written, C's zDatabase
	aliased bool
	// cteWith is the WITH a CTE's or view's body resolves in.
	cteWith  *authWith
	resolved bool
}

// authSel is one SELECT (or compound arm) as resolution left it, for the
// codegen pass.
type authSel struct {
	s        *SelectStmt
	items    []*authItem
	exprSubs []*authSel
	arms     []*authSel // a compound's arms, left to right; nil otherwise
	recurs   bool       // a recursive CTE's body
	window   bool       // has window functions, which sqlite3WindowRewrite moves into a subquery
	values   bool       // a VALUES list of several rows, which C codes as a co-routine
	exists   bool       // an EXISTS subquery, which C codes without a SELECT call
	ctxName  string
}

// authScope is one name context, C's NameContext.
type authScope struct {
	sel     *authSel
	items   []*authItem
	outer   *authScope
	aliases []SelectColumn // result columns WHERE, HAVING and GROUP BY may name
	trig    *authTrigger
	excl    bool // an upsert's DO UPDATE, where "excluded" names the new row
}

type authTrigger struct {
	name string
	tab  *authTable
}

type authWith struct {
	defs  []CTEDef
	outer *authWith
	// self is the CTE whose body is being resolved, which its own body names
	// as itself: a recursive reference.
	self string
}

func (w *authWith) find(name string) (*CTEDef, *authWith) {
	for ; w != nil; w = w.outer {
		for i := range w.defs {
			if equalFoldName(w.defs[i].Name, name) {
				return &w.defs[i], w
			}
		}
	}
	return nil, nil
}

// selectBody authorizes a SELECT whose SELECT call has been made: resolution,
// then codegen. top is a statement's own SELECT, whose IGNOREd result
// columns keep their names.
func (a *authPass) selectBody(s *SelectStmt, outer *authScope, w *authWith, top bool) {
	as := a.resolveSelect(s, outer, w, "", top)
	a.codegen(as, false)
}

// resolveSelect is sqlite3SelectPrep: expand the FROM clause and resolve
// every name in s and its subqueries.
func (a *authPass) resolveSelect(s *SelectStmt, outer *authScope, w *authWith, ctxName string, top bool) *authSel {
	as := &authSel{s: s, ctxName: ctxName}
	if a.stopped() {
		return as
	}
	if len(s.CTEs) > 0 {
		w = &authWith{defs: s.CTEs, outer: w}
	}
	if s.ValuesArms > 0 || len(s.Compound) == 0 {
		as.values = s.ValuesArms > 0
		a.resolveCore(as, s, outer, w, top)
		return as
	}
	arms := make([]*authSel, 1+len(s.Compound))
	arms[0] = &authSel{s: s}
	for i, c := range s.Compound {
		arms[i+1] = &authSel{s: c.Stmt}
	}
	as.arms = arms
	a.resolveLimit(as, outer, w)
	// resolveSelectStep walks a compound from its last arm to its first.
	for i := len(arms) - 1; i >= 0; i-- {
		a.resolveArm(arms[i], outer, w, top && i == 0)
	}
	if w != nil && w.self != "" {
		for _, arm := range arms[1:] {
			if armNames(arm.s, w.self) {
				as.recurs = true
			}
		}
	}
	return as
}

func armNames(s *SelectStmt, cte string) bool {
	for _, f := range s.From {
		if f.Subquery == nil && f.Schema == "" && equalFoldName(f.Table, cte) {
			return true
		}
	}
	return false
}

func (a *authPass) resolveLimit(as *authSel, outer *authScope, w *authWith) {
	sc := &authScope{sel: as, outer: outer}
	a.expr(as.s.LimitParam, sc, w)
	a.expr(as.s.OffsetParam, sc, w)
}

// resolveCore resolves one non-compound SELECT (or a VALUES list).
func (a *authPass) resolveCore(as *authSel, s *SelectStmt, outer *authScope, w *authWith, top bool) {
	a.resolveLimit(as, outer, w)
	a.resolveArm(as, outer, w, top)
}

// resolveArm is one turn of resolveSelectStep's loop.
func (a *authPass) resolveArm(as *authSel, outer *authScope, w *authWith, top bool) {
	s := as.s
	if a.stopped() {
		return
	}
	sc := &authScope{sel: as, outer: outer}
	if s.ValuesArms > 0 {
		a.columns(s, sc, w, false)
		for _, c := range s.Compound {
			a.columns(c.Stmt, sc, w, false)
		}
		return
	}
	as.items = a.fromItems(s.From, outer, w)
	sc.items = as.items
	// A FROM subquery is resolved before anything else in its SELECT.
	for _, it := range as.items {
		if it.sub != nil && it.sub.s != nil && !it.resolved {
			name := ""
			if it.tab != nil || it.aliased {
				name = it.name
			}
			saved := a.ctx
			if name != "" {
				a.ctx = name
			}
			cw := w
			if it.cteWith != nil {
				cw = it.cteWith
			}
			it.sub = a.resolveSelect(it.sub.s, outer, cw, name, false)
			it.resolved = true
			a.ctx = saved
		}
	}
	a.columns(s, sc, w, top)
	sc.aliases = s.Columns
	a.expr(s.Having, sc, w)
	a.expr(s.Where, sc, w)
	for _, f := range s.From {
		a.expr(f.On, sc, w)
	}
	for _, f := range s.From {
		for _, e := range f.TableFuncArgs {
			a.expr(e, sc, w)
		}
	}
	for _, o := range s.OrderBy {
		if a.orderTermNamesResult(o.Expr, s) {
			continue
		}
		a.expr(o.Expr, sc, w)
	}
	for _, g := range s.GroupBy {
		if isIntegerLiteral(g) {
			continue
		}
		a.expr(g, sc, w)
	}
}

func isIntegerLiteral(e Expr) bool {
	l, ok := e.(LiteralExpr)
	return ok && l.Val.Typ == Int
}

// orderTermNamesResult reports whether an ORDER BY term names a result
// column by alias or number, which resolveOrderGroupBy matches without
// resolving a name.
func (a *authPass) orderTermNamesResult(e Expr, s *SelectStmt) bool {
	if isIntegerLiteral(e) {
		return true
	}
	c, ok := e.(ColumnExpr)
	if !ok || c.Qualifier != "" {
		return false
	}
	for _, rc := range s.Columns {
		if rc.HasAlias && equalFoldName(rc.Alias, c.Name) {
			return true
		}
	}
	return false
}

// columns resolves a result list, expanding each "*" as sqlite3SelectExpand
// does.
func (a *authPass) columns(s *SelectStmt, sc *authScope, w *authWith, top bool) {
	for _, rc := range s.Columns {
		if a.stopped() {
			return
		}
		if rc.Star {
			a.star(rc, sc)
			continue
		}
		name := ""
		if !rc.HasAlias {
			name = rc.Text
		}
		a.exprNamed(rc.Expr, sc, w, name)
	}
}

// star reads the columns a "*" expands to. When the authorizer ignores some,
// the "*" is written out with those columns as NULL.
func (a *authPass) star(rc SelectColumn, sc *authScope) {
	var parts []string
	ignored := false
	for _, it := range sc.items {
		if rc.StarQualifier != "" && !equalFoldName(rc.StarQualifier, it.name) {
			continue
		}
		for _, col := range it.visibleCols() {
			if rc.StarQualifier == "" && it.merged[strings.ToLower(col)] {
				continue
			}
			switch {
			case a.readItem(it, col) == AuthIgnore:
				ignored = true
				parts = append(parts, "NULL AS "+quoteIdent(col))
			case it.name == "":
				parts = append(parts, "") // an unnamed subquery, which no qualifier reaches
			default:
				parts = append(parts, quoteIdent(it.name)+"."+quoteIdent(col))
			}
		}
	}
	if !ignored || a.err != nil {
		return
	}
	for _, p := range parts {
		if p == "" {
			a.err = errAuthIgnoreUnsupported
			return
		}
	}
	a.splice(rc.authRef, strings.Join(parts, ", "))
}

func (it *authItem) visibleCols() []string {
	if it.tab != nil {
		var out []string
		for i, c := range it.tab.cols {
			if i < len(it.tab.hidden) && it.tab.hidden[i] {
				continue
			}
			out = append(out, c)
		}
		return out
	}
	return it.cols
}

// readItem is a read of col through it: a READ when it is a table or view.
func (a *authPass) readItem(it *authItem, col string) int {
	if it.tab == nil {
		it.used = true
		return AuthOK
	}
	idx := it.tab.colIndex(col)
	if idx >= 0 && idx != it.tab.ipk {
		it.used = true
	}
	if idx < 0 || idx == it.tab.ipk {
		col = it.tab.rowidName()
	} else {
		col = it.tab.cols[idx]
	}
	rc := a.readCol(it.tab.name, col, it.tab.db)
	if rc == AuthIgnore && a.keepReads {
		return AuthOK
	}
	return rc
}

// readItemCol is readItem for the reference ref, which an IGNORE makes NULL.
func (a *authPass) readItemCol(it *authItem, col string, ref int32, name string) {
	if a.readItem(it, col) == AuthIgnore {
		a.ignore(ref, name)
	}
}

func (a *authPass) stopped() bool { return a.err != nil || a.halt }

// fromItems expands a FROM clause: tables, views and CTEs found, USING and
// NATURAL columns merged.
func (a *authPass) fromItems(from []FromItem, outer *authScope, w *authWith) []*authItem {
	var items []*authItem
	for _, f := range from {
		it := &authItem{name: f.Alias, zdb: f.Schema, aliased: f.Alias != ""}
		switch {
		case f.Subquery != nil:
			it.sub = &authSel{s: f.Subquery}
			it.cols = a.selectColNames(f.Subquery, w)
		default:
			if it.name == "" {
				it.name = f.Table
			}
			if def, dw := w.find(f.Table); f.Schema == "" && def != nil {
				if dw.self != "" && equalFoldName(dw.self, f.Table) {
					it.cols = def.ColNames
					break
				}
				it.sub = &authSel{s: def.Select}
				it.cols = def.ColNames
				if len(it.cols) == 0 {
					it.cols = a.selectColNames(def.Select, w)
				}
				it.cteWith = &authWith{defs: dw.defs, outer: dw.outer, self: def.Name}
				break
			}
			t := a.lookupTable(f.Schema, f.Table)
			if t == nil && f.TableFunc {
				t = tableFuncTable(f.Table)
			}
			if t == nil {
				a.halt = true // C fails with "no such table"
				return items
			}
			it.tab = t
			if t.view != nil {
				it.sub = &authSel{s: t.view}
				it.cteWith = &authWith{}
			}
		}
		if len(items) > 0 {
			it.merged = map[string]bool{}
			for _, u := range f.Using {
				it.merged[strings.ToLower(u)] = true
			}
			if f.Natural {
				for _, c := range it.visibleCols() {
					for _, prev := range items {
						if prev.hasCol(c) {
							it.merged[strings.ToLower(c)] = true
						}
					}
				}
			}
			// sqlite3ProcessJoin's join term reads the column on both sides.
			for c := range it.merged {
				it.markUsed(c)
				for _, prev := range items {
					if prev.hasCol(c) {
						prev.markUsed(c)
						break
					}
				}
			}
		}
		items = append(items, it)
	}
	return items
}

func (it *authItem) markUsed(c string) {
	if it.tab != nil {
		if i := it.tab.colIndex(c); i >= 0 && i != it.tab.ipk {
			it.used = true
		}
	}
}

func (it *authItem) hasCol(c string) bool {
	if it.tab != nil {
		return it.tab.colIndex(c) >= 0
	}
	for _, x := range it.cols {
		if equalFoldName(x, c) {
			return true
		}
	}
	return false
}

// selectColNames is the column names a subquery yields.
func (a *authPass) selectColNames(s *SelectStmt, w *authWith) []string {
	var out []string
	for _, rc := range s.Columns {
		switch {
		case rc.Star:
			for _, f := range s.From {
				if rc.StarQualifier != "" && !equalFoldName(rc.StarQualifier, f.Alias) && !equalFoldName(rc.StarQualifier, f.Table) {
					continue
				}
				switch {
				case f.Subquery != nil:
					out = append(out, a.selectColNames(f.Subquery, w)...)
				default:
					if def, _ := w.find(f.Table); def != nil && f.Schema == "" {
						if len(def.ColNames) > 0 {
							out = append(out, def.ColNames...)
						} else if def.Select != nil {
							out = append(out, a.selectColNames(def.Select, nil)...)
						}
						continue
					}
					if t := a.lookupTable(f.Schema, f.Table); t != nil {
						out = append(out, t.cols...)
					}
				}
			}
		case rc.HasAlias:
			out = append(out, rc.Alias)
		default:
			if c, ok := rc.Expr.(ColumnExpr); ok {
				out = append(out, c.Name)
			} else {
				out = append(out, rc.Text)
			}
		}
	}
	return out
}

// codegen is the SELECT calls sqlite3Select makes after resolution: each
// compound arm's, each FROM subquery's and each expression subquery's, and a
// READ with no column for a table nothing was read from.
func (a *authPass) codegen(as *authSel, withSelect bool) {
	if a.stopped() || as == nil {
		return
	}
	if withSelect && !as.exists {
		saved := a.ctx
		if as.ctxName != "" {
			a.ctx = as.ctxName
		}
		ok := a.mustOK(AuthSelect, "", "", "")
		a.ctx = saved
		if !ok {
			return
		}
	}
	if as.arms != nil {
		if as.recurs && !a.mustOK(AuthRecursive, "", "", "") {
			return
		}
		for _, arm := range as.arms {
			if !a.mustOK(AuthSelect, "", "", "") {
				return
			}
			a.codegenCore(arm)
		}
		for _, sub := range as.exprSubs {
			a.codegen(sub, true)
		}
		return
	}
	a.codegenCore(as)
}

func (a *authPass) codegenCore(as *authSel) {
	if (as.window || as.values) && !a.mustOK(AuthSelect, "", "", "") {
		return
	}
	// flattenSubquery authorizes each subquery it flattens before the FROM
	// loop, whose tables then join the outer query's.
	flat := make([]bool, len(as.items))
	for i, it := range as.items {
		if it.sub != nil && it.sub.s != nil && a.flattens(as, it) {
			flat[i] = true
			saved := a.ctx
			a.ctx = it.name
			ok := a.mustOK(AuthSelect, "", "", "")
			a.ctx = saved
			if !ok {
				return
			}
		}
	}
	for i, it := range as.items {
		if a.stopped() {
			return
		}
		if flat[i] {
			a.flatReads(it)
			for _, sub := range it.sub.exprSubs {
				a.codegen(sub, true)
			}
			continue
		}
		if it.tab != nil && !it.used && it.tab.view == nil {
			a.mustOK(AuthRead, it.tab.name, "", it.zdb)
		}
		if it.sub != nil && it.sub.s != nil {
			a.codegen(it.sub, true)
		}
	}
	for _, sub := range as.exprSubs {
		a.codegen(sub, true)
	}
}

// flattens reports whether sqlite3Select's query flattener folds it, a FROM
// subquery, view or CTE of as, into as: the plain cases of its conditions.
func (a *authPass) flattens(as *authSel, it *authItem) bool {
	sub := it.sub
	s := sub.s
	if sub.arms != nil || sub.window || sub.values || len(s.From) == 0 || s.Distinct ||
		s.Limit != nil || s.LimitParam != nil || len(s.Compound) > 0 || selectIsAggregateQuery(s) {
		return false
	}
	if as.s != nil && (selectIsAggregateQuery(as.s) && len(s.From) > 1) {
		return false
	}
	if it.tab == nil && it.cteWith != nil {
		n := 0
		for _, o := range as.items {
			if o.sub != nil && o.sub.s == s {
				n++
			}
		}
		if n > 1 {
			return false
		}
	}
	return true
}

// flatReads is the FROM loop's READ for each table of a flattened subquery the
// outer query reads nothing from.
func (a *authPass) flatReads(it *authItem) {
	for _, in := range it.sub.items {
		if a.stopped() {
			return
		}
		if in.sub != nil && in.sub.s != nil {
			a.codegen(in.sub, true)
			continue
		}
		used := in.used && (it.used || it.sub.s.Where != nil)
		if in.tab != nil && !used && in.tab.view == nil {
			a.mustOK(AuthRead, in.tab.name, "", in.zdb)
		}
	}
}
