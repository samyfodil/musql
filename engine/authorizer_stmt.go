package engine

import "strings"

// The statement half of the authorizer: what insert.c, update.c, delete.c,
// build.c, trigger.c, alter.c, pragma.c and attach.c check, in their order.
// DDL also checks what its nested statements on sqlite_schema check, as C's
// sqlite3NestedParse runs them through the same authorizer.

func schemaTableFor(db string) string {
	if db == "temp" {
		return "sqlite_temp_master"
	}
	return "sqlite_master"
}

// target resolves a DML statement's table; nil stops (C fails with "no such
// table").
func (a *authPass) target(schema, name string) *authTable {
	t := a.lookupTable(schema, name)
	if t == nil {
		a.halt = true
	}
	return t
}

func (a *authPass) insertText() {
	p, err := a.parser()
	if err != nil {
		return
	}
	st, err := parseInsertStmtTokens(p, false)
	if err != nil {
		return
	}
	a.insert(st, nil)
}

func (a *authPass) insert(st *insertStmt, w *authWith) {
	if len(st.ctes) > 0 {
		w = &authWith{defs: st.ctes, outer: w}
	}
	t := a.target(st.schema, st.table)
	if t == nil {
		return
	}
	// Several rows of VALUES are a co-routine whose SELECT the parser codes.
	if len(st.rows) >= 2 && !a.mustOK(AuthSelect, "", "", "") {
		return
	}
	if !a.head(AuthInsert, t.name, "", t.db) {
		return
	}
	if st.selectStmt != nil {
		if !a.mustOK(AuthSelect, "", "", "") {
			return
		}
		a.selectBody(st.selectStmt, &authScope{trig: a.trigScope()}, w, false)
	}
	vals := &authSel{s: &SelectStmt{}}
	vsc := &authScope{sel: vals, trig: a.trigScope()}
	for _, row := range st.rows {
		for _, e := range row {
			a.expr(e, vsc, w)
		}
	}
	for _, sub := range vals.exprSubs {
		a.codegen(sub, true)
	}
	it := &authItem{name: t.name, tab: t}
	if st.alias != "" {
		it.name = st.alias
	}
	if u := st.upsert; u != nil {
		tsc := &authScope{items: []*authItem{it}}
		for _, c := range u.targetCols {
			a.column(ColumnExpr{Name: c}, tsc, "")
		}
		a.expr(u.targetWhere, tsc, w)
		if !u.doNothing {
			tsc.excl = true
			a.assignments(t, u.sets, tsc, w)
			a.expr(u.where, tsc, w)
		}
	}
	a.triggers(t, triggerInsert, nil, w)
	a.returning(st.returning, it, w)
}

// assignments is update.c's loop: each value resolved, then its column's
// UPDATE.
func (a *authPass) assignments(t *authTable, sets []assignment, sc *authScope, w *authWith) {
	for _, s := range sets {
		a.expr(s.expr, sc, w)
		if a.stopped() {
			return
		}
		col := s.col
		if i := t.colIndex(col); i >= 0 && i != t.ipk {
			col = t.cols[i]
		} else if i >= 0 || isRowidAliasName(col) && t.rowid {
			col = t.rowidName()
		} else {
			a.halt = true // C fails with "no such column"
			return
		}
		a.mustOK(AuthUpdate, t.name, col, t.db)
	}
}

func (a *authPass) returning(cols []SelectColumn, it *authItem, w *authWith) {
	if len(cols) == 0 {
		return
	}
	sc := &authScope{items: []*authItem{it}}
	a.keepReads = true
	a.columns(&SelectStmt{Columns: cols}, sc, w, true)
	a.keepReads = false
}

func (a *authPass) updateText() {
	p, err := a.parser()
	if err != nil {
		return
	}
	st, err := parseUpdateStmtTokens(p)
	if err != nil {
		return
	}
	a.update(st, nil)
}

func (a *authPass) update(st *updateStmt, w *authWith) {
	if len(st.ctes) > 0 {
		w = &authWith{defs: st.ctes, outer: w}
	}
	t := a.target(st.schema, st.table)
	if t == nil {
		return
	}
	it := &authItem{name: t.name, tab: t}
	if st.alias != "" {
		it.name = st.alias
	}
	sel := &authSel{s: &SelectStmt{}}
	sc := &authScope{sel: sel, items: []*authItem{it}, trig: a.trigScope()}
	if len(st.from) > 0 {
		// updateFromSelect: the columns first, then a SELECT over the target
		// and the FROM items that computes the new values.
		for _, s := range st.sets {
			col := s.col
			if i := t.colIndex(col); i >= 0 && i != t.ipk {
				col = t.cols[i]
			} else {
				col = t.rowidName()
			}
			if !a.mustOK(AuthUpdate, t.name, col, t.db) {
				return
			}
		}
		if !a.mustOK(AuthSelect, "", "", "") {
			return
		}
		from := a.fromItems(st.from, nil, w)
		sc.items = append([]*authItem{it}, from...)
		for _, s := range st.sets {
			a.expr(s.expr, sc, w)
		}
		a.expr(st.where, sc, w)
		if !it.used && !a.stopped() {
			a.mustOK(AuthRead, t.name, "", "")
		}
		a.codegenCore(&authSel{items: from, exprSubs: sel.exprSubs})
		return
	}
	a.assignments(t, st.sets, sc, w)
	a.expr(st.where, sc, w)
	var cols []string
	for _, s := range st.sets {
		cols = append(cols, s.col)
	}
	a.triggers(t, triggerUpdate, cols, w)
	for _, sub := range sel.exprSubs {
		a.codegen(sub, true)
	}
	a.returning(st.returning, it, w)
}

func (a *authPass) deleteText() {
	p, err := a.parser()
	if err != nil {
		return
	}
	st, err := parseDeleteStmtTokens(p)
	if err != nil {
		return
	}
	a.delete(st, nil)
}

func (a *authPass) delete(st *deleteStmt, w *authWith) {
	if len(st.ctes) > 0 {
		w = &authWith{defs: st.ctes, outer: w}
	}
	t := a.target(st.schema, st.table)
	if t == nil {
		return
	}
	// delete.c goes on after an IGNORE, only without its truncate shortcut.
	if a.check(AuthDelete, t.name, "", t.db) == AuthDeny {
		return
	}
	it := &authItem{name: t.name, tab: t}
	if st.alias != "" {
		it.name = st.alias
	}
	sel := &authSel{s: &SelectStmt{}}
	sc := &authScope{sel: sel, items: []*authItem{it}, trig: a.trigScope()}
	a.expr(st.where, sc, w)
	a.triggers(t, triggerDelete, nil, w)
	for _, sub := range sel.exprSubs {
		a.codegen(sub, true)
	}
	a.returning(st.returning, it, w)
}

// trigStack is the triggers being coded, which a trigger's own statements do
// not code again.
func (a *authPass) trigScope() *authTrigger {
	if len(a.trigs) == 0 {
		return nil
	}
	return a.trigs[len(a.trigs)-1]
}

// triggers codes the triggers a statement on t fires, as codeRowTrigger does:
// in the trigger's context, its WHEN and then its statements.
func (a *authPass) triggers(t *authTable, event triggerEventKind, cols []string, w *authWith) {
	if a.stopped() || t.db == "" {
		return
	}
	temp := t.db == "temp"
	if t.db != "main" && !temp {
		return
	}
	for _, tr := range a.db.triggers {
		if tr.event != event || tr.tableIsTemp != temp || tr.tableAttachName != "" || !equalFoldName(tr.table, t.name) {
			continue
		}
		if event == triggerUpdate && len(tr.updateCols) > 0 && !anyFold(tr.updateCols, cols) {
			continue
		}
		active := false
		for _, at := range a.trigs {
			if equalFoldName(at.name, tr.name) {
				active = true
			}
		}
		if active {
			continue
		}
		at := &authTrigger{name: tr.name, tab: t}
		a.trigs = append(a.trigs, at)
		saved := a.ctx
		a.ctx = tr.name
		a.expr(tr.when, &authScope{trig: at}, nil)
		for _, b := range tr.body {
			switch {
			case b.insert != nil:
				a.insert(b.insert, nil)
			case b.update != nil:
				a.update(b.update, nil)
			case b.delete != nil:
				a.delete(b.delete, nil)
			case b.sel != nil:
				if a.mustOK(AuthSelect, "", "", "") {
					a.selectBody(b.sel, &authScope{trig: at}, nil, false)
				}
			}
			if a.stopped() {
				break
			}
		}
		a.ctx = saved
		a.trigs = a.trigs[:len(a.trigs)-1]
		if a.stopped() {
			return
		}
	}
}

func anyFold(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if equalFoldName(x, y) {
				return true
			}
		}
	}
	return false
}

// ddlName is one "[schema.]name" in a DDL statement.
type ddlName struct {
	schema, name string
}

// ddlReader walks a DDL statement's leading tokens.
type ddlReader struct {
	toks []token
	i    int
}

func (r *ddlReader) kw(words ...string) bool {
	for _, w := range words {
		if r.i < len(r.toks) && r.toks[r.i].kind == tkIdent && r.toks[r.i].isKw(w) {
			r.i++
			return true
		}
	}
	return false
}

func (r *ddlReader) seq(words ...string) bool {
	at := r.i
	for _, w := range words {
		if !r.kw(w) {
			r.i = at
			return false
		}
	}
	return true
}

func (r *ddlReader) ident() (string, bool) {
	if r.i >= len(r.toks) {
		return "", false
	}
	t := r.toks[r.i]
	if t.kind == tkString {
		r.i++
		return t.str, true
	}
	if t.kind != tkIdent {
		return "", false
	}
	r.i++
	return identName(t), true
}

func identName(t token) string {
	if t.quoted && len(t.text) >= 2 {
		q := t.text[0]
		body := t.text[1 : len(t.text)-1]
		switch q {
		case '"':
			return strings.ReplaceAll(body, `""`, `"`)
		case '`':
			return strings.ReplaceAll(body, "``", "`")
		case '[':
			return body
		}
	}
	return t.text
}

func (r *ddlReader) name() (ddlName, bool) {
	first, ok := r.ident()
	if !ok {
		return ddlName{}, false
	}
	if r.i < len(r.toks) && r.toks[r.i].kind == tkPunct && r.toks[r.i].text == "." {
		r.i++
		second, ok := r.ident()
		if !ok {
			return ddlName{}, false
		}
		return ddlName{first, second}, true
	}
	return ddlName{"", first}, true
}

// dbOf is the database a DDL statement's object goes into.
func (a *authPass) dbOf(schema string, temp bool) (string, bool) {
	switch {
	case temp:
		if schema != "" && !equalFoldName(schema, "temp") {
			return "", false
		}
		return "temp", true
	case schema == "", equalFoldName(schema, "main"):
		return "main", true
	case equalFoldName(schema, "temp"):
		return "temp", true
	}
	for _, x := range a.extra {
		if equalFoldName(x.Name, schema) {
			return x.Name, true
		}
	}
	for _, at := range a.db.attached {
		if equalFoldName(at.name, schema) {
			return at.name, true
		}
	}
	return "", false
}

// schemaRowWritten is sqlite3EndTable's nested UPDATE of the new object's
// sqlite_schema row.
func (a *authPass) schemaRowWritten(db string) {
	m := schemaTableFor(db)
	for _, c := range schemaTableCols {
		if !a.mustOK(AuthUpdate, m, c, db) {
			return
		}
	}
	a.readSchema(m, "ROWID", db)
}

func (a *authPass) create(toks []token) {
	r := &ddlReader{toks: toks, i: 1}
	temp := r.kw("TEMP", "TEMPORARY")
	unique := r.kw("UNIQUE")
	switch {
	case !unique && r.kw("TABLE"):
		a.createTable(r, temp)
	case !unique && r.kw("VIEW"):
		a.createView(r, temp)
	case r.kw("INDEX"):
		a.createIndex(temp)
	case !unique && r.kw("TRIGGER"):
		a.createTrigger(r, temp)
	case !unique && !temp && r.seq("VIRTUAL", "TABLE"):
		a.createVirtual(r)
	}
}

func (a *authPass) createTable(r *ddlReader, temp bool) {
	ifNot := r.seq("IF", "NOT", "EXISTS")
	n, ok := r.name()
	if !ok {
		return
	}
	db, ok := a.dbOf(n.schema, temp)
	if !ok {
		a.halt = true
		return
	}
	code := AuthCreateTable
	if db == "temp" {
		code = AuthCreateTempTable
	}
	if !a.head(AuthInsert, schemaTableFor(db), "", db) || !a.head(code, n.name, "", db) {
		return
	}
	if a.lookupTable(db, n.name) != nil {
		if !ifNot {
			a.halt = true
		}
		return
	}
	if r.kw("AS") && r.i < len(r.toks) {
		p, err := a.parser()
		if err != nil {
			return
		}
		p.pos = r.i
		s, err := p.parseSelectStmt()
		if err != nil {
			return
		}
		as := a.resolveSelect(s, nil, nil, "", false)
		a.codegen(as, true)
	}
	a.schemaRowWritten(db)
	if !a.stopped() && strings.Contains(strings.ToUpper(a.src), "AUTOINCREMENT") && a.lookupTable(db, "sqlite_sequence") == nil {
		a.mustOK(AuthInsert, schemaTableFor(db), "", db)
		a.mustOK(code, "sqlite_sequence", "", db)
		a.schemaRowWritten(db)
	}
}

func (a *authPass) createView(r *ddlReader, temp bool) {
	ifNot := r.seq("IF", "NOT", "EXISTS")
	n, ok := r.name()
	if !ok {
		return
	}
	db, ok := a.dbOf(n.schema, temp)
	if !ok {
		a.halt = true
		return
	}
	code := AuthCreateView
	if db == "temp" {
		code = AuthCreateTempView
	}
	if !a.head(AuthInsert, schemaTableFor(db), "", db) || !a.head(code, n.name, "", db) {
		return
	}
	if a.lookupTable(db, n.name) != nil {
		if !ifNot {
			a.halt = true
		}
		return
	}
	a.schemaRowWritten(db)
}

func (a *authPass) createIndex(temp bool) {
	pc, err := parseCreateIndexStmt(a.src)
	if err != nil {
		return
	}
	schema := ""
	switch pc.scope {
	case scopeMain:
		schema = "main"
	case scopeTemp:
		schema = "temp"
	}
	t := a.lookupTable(schema, pc.table)
	if t == nil || t.view != nil {
		a.halt = true
		return
	}
	db := t.db
	if a.indexExists(db, pc.name) {
		a.halt = true
		return
	}
	code := AuthCreateIndex
	if db == "temp" {
		code = AuthCreateTempIndex
	}
	if !a.head(AuthInsert, schemaTableFor(db), "", db) || !a.head(code, pc.name, t.name, db) {
		return
	}
	sc := &authScope{items: []*authItem{{name: t.name, tab: t}}}
	a.expr(pc.where, sc, nil)
	for i, c := range pc.cols {
		if i < len(pc.exprs) && pc.exprs[i] != nil {
			a.expr(pc.exprs[i], sc, nil)
			continue
		}
		a.column(ColumnExpr{Name: c}, sc, "")
	}
	a.mustOK(AuthInsert, schemaTableFor(db), "", db)
	a.mustOK(AuthReindex, pc.name, "", db)
}

func (a *authPass) indexExists(db, name string) bool {
	for _, ix := range a.db.indexes {
		if equalFoldName(ix.name, name) && (ix.isTemp == (db == "temp")) {
			return true
		}
	}
	return false
}

func (a *authPass) createTrigger(r *ddlReader, temp bool) {
	pt, err := parseCreateTriggerStmt(a.src)
	if err != nil {
		return
	}
	schema := ""
	switch pt.tableScope {
	case scopeMain:
		schema = "main"
	case scopeTemp:
		schema = "temp"
	}
	t := a.lookupTable(schema, pt.table)
	if t == nil {
		a.halt = true
		return
	}
	db := "main"
	if pt.isTemp || t.db == "temp" {
		db = "temp"
	}
	if a.db.findTriggerMetaIn(scopeOfDB(db), pt.name) != nil {
		if !pt.ifNotExi {
			a.halt = true
		}
		return
	}
	code := AuthCreateTrigger
	if db == "temp" {
		code = AuthCreateTempTrigger
	}
	if !a.head(code, pt.name, t.name, db) || !a.head(AuthInsert, schemaTableFor(t.db), "", db) {
		return
	}
	a.mustOK(AuthInsert, schemaTableFor(db), "", db)
}

func (a *authPass) createVirtual(r *ddlReader) {
	ifNot := r.seq("IF", "NOT", "EXISTS")
	n, ok := r.name()
	if !ok || !r.kw("USING") {
		return
	}
	module, ok := r.ident()
	if !ok {
		return
	}
	db, ok := a.dbOf(n.schema, false)
	if !ok {
		a.halt = true
		return
	}
	if !a.head(AuthInsert, schemaTableFor(db), "", db) {
		return
	}
	if a.lookupTable(db, n.name) != nil {
		if !ifNot {
			a.halt = true
		}
		return
	}
	if !a.head(AuthCreateVtable, n.name, module, db) {
		return
	}
	a.schemaRowWritten(db)
}

func (a *authPass) drop(toks []token) {
	r := &ddlReader{toks: toks, i: 1}
	switch {
	case r.kw("TABLE"):
		a.dropTable(r, false)
	case r.kw("VIEW"):
		a.dropTable(r, true)
	case r.kw("INDEX"):
		a.dropIndex(r)
	case r.kw("TRIGGER"):
		a.dropTrigger(r)
	}
}

func (a *authPass) dropTable(r *ddlReader, isView bool) {
	ifExists := r.seq("IF", "EXISTS")
	n, ok := r.name()
	if !ok {
		return
	}
	t := a.lookupTable(n.schema, n.name)
	if t == nil || (t.view != nil) != isView {
		if t != nil || !ifExists {
			a.halt = true
		}
		return
	}
	db, m := t.db, schemaTableFor(t.db)
	if !a.head(AuthDelete, m, "", db) {
		return
	}
	code, arg2 := AuthDropTable, ""
	switch {
	case t.vtab != "":
		code, arg2 = AuthDropVtable, t.vtab
	case isView && db == "temp":
		code = AuthDropTempView
	case isView:
		code = AuthDropView
	case db == "temp":
		code = AuthDropTempTable
	}
	if !a.head(code, t.name, arg2, db) || !a.head(AuthDelete, t.name, "", db) {
		return
	}
	for _, tr := range a.db.triggers {
		if tr.tableAttachName == "" && tr.tableIsTemp == (db == "temp") && equalFoldName(tr.table, t.name) {
			a.dropTriggerCalls(tr.name, t.name, tr.isTemp, db)
		}
	}
	if tm := a.db.findTableMetaIn(scopeOfDB(db), t.name); tm != nil && tm.autoIncrement {
		a.mustOK(AuthDelete, "sqlite_sequence", "", db)
		a.readSchema("sqlite_sequence", "name", db)
	}
	a.mustOK(AuthDelete, m, "", db)
	a.readSchema(m, "tbl_name", db)
	a.readSchema(m, "type", db)
	if isView || t.vtab != "" {
		return
	}
	for i := 0; i < a.rootPages(t, db); i++ {
		a.mustOK(AuthUpdate, m, "rootpage", db)
		a.readSchema(m, "rootpage", db)
	}
}

func scopeOfDB(db string) schemaScope {
	if db == "temp" {
		return scopeTemp
	}
	return scopeMain
}

// rootPages is how many b-trees destroyTable frees: the table's and each
// index's.
func (a *authPass) rootPages(t *authTable, db string) int {
	n := 1
	tm := a.db.findTableMetaIn(scopeOfDB(db), t.name)
	if tm == nil {
		return n
	}
	for _, ix := range a.db.indexes {
		if ix.isTemp == tm.isTemp && equalFoldName(ix.table, tm.name) {
			n++
		}
	}
	return n
}

func (a *authPass) dropTriggerCalls(name, table string, temp bool, db string) {
	code := AuthDropTrigger
	if temp {
		code = AuthDropTempTrigger
	}
	m := schemaTableFor(db)
	if !a.head(code, name, table, db) || !a.head(AuthDelete, m, "", db) {
		return
	}
	a.mustOK(AuthDelete, m, "", db)
	a.readSchema(m, "name", db)
	a.readSchema(m, "type", db)
}

func (a *authPass) dropIndex(r *ddlReader) {
	ifExists := r.seq("IF", "EXISTS")
	n, ok := r.name()
	if !ok {
		return
	}
	var ix *indexMeta
	switch {
	case n.schema == "":
		ix = a.db.findIndexMetaIn(scopeAny, n.name)
	case equalFoldName(n.schema, "main"):
		ix = a.db.findIndexMetaIn(scopeMain, n.name)
	case equalFoldName(n.schema, "temp"):
		ix = a.db.findIndexMetaIn(scopeTemp, n.name)
	}
	if ix == nil {
		if !ifExists {
			a.halt = true
		}
		return
	}
	db := "main"
	code := AuthDropIndex
	if ix.isTemp {
		db, code = "temp", AuthDropTempIndex
	}
	m := schemaTableFor(db)
	if !a.head(AuthDelete, m, "", db) || !a.head(code, ix.name, ix.table, db) {
		return
	}
	a.mustOK(AuthDelete, m, "", db)
	a.readSchema(m, "name", db)
	a.readSchema(m, "type", db)
	a.mustOK(AuthUpdate, m, "rootpage", db)
	a.readSchema(m, "rootpage", db)
}

func (a *authPass) dropTrigger(r *ddlReader) {
	ifExists := r.seq("IF", "EXISTS")
	n, ok := r.name()
	if !ok {
		return
	}
	scope := scopeAny
	switch {
	case equalFoldName(n.schema, "main"):
		scope = scopeMain
	case equalFoldName(n.schema, "temp"):
		scope = scopeTemp
	}
	tr := a.db.findTriggerMetaIn(scope, n.name)
	if tr == nil {
		if !ifExists {
			a.halt = true
		}
		return
	}
	db := "main"
	if tr.isTemp {
		db = "temp"
	}
	a.dropTriggerCalls(tr.name, tr.table, tr.isTemp, db)
}

func (a *authPass) alter(toks []token) {
	r := &ddlReader{toks: toks, i: 1}
	if !r.kw("TABLE") {
		return
	}
	n, ok := r.name()
	if !ok {
		return
	}
	t := a.lookupTable(n.schema, n.name)
	if t == nil || t.view != nil {
		a.halt = true
		return
	}
	col := ""
	if r.kw("DROP") {
		r.kw("COLUMN")
		c, ok := r.ident()
		if !ok {
			return
		}
		if i := t.colIndex(c); i >= 0 {
			col = t.cols[i]
		} else {
			a.halt = true
			return
		}
	}
	a.head(AuthAlterTable, t.db, t.name, col)
}

func (a *authPass) pragma() {
	toks, err := lex(a.src)
	if err != nil {
		return
	}
	r := &ddlReader{toks: toks, i: 1}
	n, ok := r.name()
	if !ok {
		return
	}
	db := ""
	if n.schema != "" {
		d, ok := a.dbOf(n.schema, false)
		if !ok {
			a.halt = true
			return
		}
		db = d
	}
	value := ""
	if r.i < len(toks) && toks[r.i].kind == tkPunct && (toks[r.i].text == "=" || toks[r.i].text == "(") {
		r.i++
		neg := false
		if r.i < len(toks) && toks[r.i].kind == tkPunct && (toks[r.i].text == "-" || toks[r.i].text == "+") {
			neg = toks[r.i].text == "-"
			r.i++
		}
		if r.i < len(toks) {
			t := toks[r.i]
			switch t.kind {
			case tkString:
				value = t.str
			case tkIdent:
				value = identName(t)
			default:
				value = t.text
			}
			if neg {
				value = "-" + value
			}
		}
	}
	a.head(AuthPragma, n.name, value, db)
}

func (a *authPass) transaction(toks []token) {
	r := &ddlReader{toks: toks}
	switch {
	case r.kw("BEGIN"):
		a.head(AuthTransaction, "BEGIN", "", "")
	case r.kw("COMMIT", "END"):
		a.head(AuthTransaction, "COMMIT", "", "")
	case r.kw("SAVEPOINT"):
		if name, ok := r.ident(); ok {
			a.head(AuthSavepoint, "BEGIN", name, "")
		}
	case r.kw("RELEASE"):
		r.kw("SAVEPOINT")
		if name, ok := r.ident(); ok {
			a.head(AuthSavepoint, "RELEASE", name, "")
		}
	case r.kw("ROLLBACK"):
		r.kw("TRANSACTION")
		if !r.kw("TO") {
			a.head(AuthTransaction, "ROLLBACK", "", "")
			return
		}
		r.kw("SAVEPOINT")
		if name, ok := r.ident(); ok {
			a.head(AuthSavepoint, "ROLLBACK", name, "")
		}
	}
}

func (a *authPass) attachStmt() {
	toks, err := lex(a.src)
	if err != nil {
		return
	}
	i := 1
	if i < len(toks) && toks[i].kind == tkIdent && toks[i].isKw("DATABASE") {
		i++
	}
	file := ""
	if i+1 < len(toks) && toks[i].kind == tkString && toks[i+1].kind == tkIdent && toks[i+1].isKw("AS") {
		file = toks[i].str
	}
	a.head(AuthAttach, file, "", "")
}

func (a *authPass) detachStmt() {
	toks, err := lex(a.src)
	if err != nil {
		return
	}
	r := &ddlReader{toks: toks, i: 1}
	r.kw("DATABASE")
	if name, ok := r.ident(); ok {
		a.head(AuthDetach, name, "", "")
	}
}

// analyze checks each table ANALYZE would analyze. The statistics table it
// may create and read back are not modelled.
func (a *authPass) analyze(toks []token) {
	r := &ddlReader{toks: toks, i: 1}
	var tables []*tableMeta
	if n, ok := r.name(); ok {
		if n.schema == "" {
			if d, ok := a.dbOf(n.name, false); ok && a.lookupTable("", n.name) == nil {
				tables = a.dbTables(d)
			}
		}
		if tables == nil {
			if t := a.lookupTable(n.schema, n.name); t != nil {
				tables = append(tables, a.db.findTableMetaIn(scopeOfDB(t.db), t.name))
			} else if ix := a.db.findIndexMetaIn(scopeAny, n.name); ix != nil {
				tables = append(tables, a.db.findTableMetaIn(scopeOfDB(boolDB(ix.isTemp)), ix.table))
			}
		}
	} else {
		tables = a.dbTables("main")
	}
	for _, tm := range tables {
		if tm == nil || strings.HasPrefix(strings.ToLower(tm.name), "sqlite_") {
			continue
		}
		if !a.mustOK(AuthAnalyze, tm.name, "", boolDB(tm.isTemp)) {
			return
		}
	}
}

func boolDB(temp bool) string {
	if temp {
		return "temp"
	}
	return "main"
}

func (a *authPass) dbTables(db string) []*tableMeta {
	var out []*tableMeta
	for _, t := range a.db.tables {
		if t.isTemp == (db == "temp") {
			out = append(out, t)
		}
	}
	return out
}

// reindex checks each index REINDEX would rebuild.
func (a *authPass) reindex(toks []token) {
	r := &ddlReader{toks: toks, i: 1}
	n, named := r.name()
	for _, ix := range a.db.indexes {
		if named {
			db := boolDB(ix.isTemp)
			if n.schema != "" && !equalFoldName(n.schema, db) {
				continue
			}
			if !equalFoldName(ix.name, n.name) && !equalFoldName(ix.table, n.name) {
				continue
			}
		}
		if !a.mustOK(AuthReindex, ix.name, "", boolDB(ix.isTemp)) {
			return
		}
	}
}
