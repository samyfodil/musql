// This file implements CREATE VIEW and the read-path machinery a stored view
// needs: parsing "CREATE [TEMP|TEMPORARY] VIEW [IF NOT EXISTS] [schema.]name
// [(col-list)] AS <select-stmt>", registering it (viewMeta/DB.views; the
// catalog load recovers it), and resolving a FROM item naming a view into the
// same derived-table row source a parenthesized subquery gets -- a view runs
// as an inline derived table, never through a separate path. DROP VIEW is in
// drop_write.go.
//
// CREATE VIEW does not inspect the SELECT: an explicit column list's count and
// a missing table are both reported at query time, as in C, even for "SELECT *
// FROM <table created later>". See viewOutputRows.
package engine

import (
	"fmt"
	"strings"
)

// viewMeta is what this writer needs to remember about a view -- whether
// created this session (CreateView) or recovered from an existing database
// (OpenWrite) -- in order to resolve a FROM-item reference to it and, at
// commit, re-emit its verbatim CREATE VIEW text into sqlite_schema (a view has
// no b-tree of its own: rootpage is always 0, exactly like C SQLite).
type viewMeta struct {
	// schemaSeq is this object's CREATION rank within its database: the order
	// its sqlite_schema row must appear in, which C SQLite gives by simply
	// appending each new object's row to the catalog b-tree. materialize
	// (writer.go) sorts every row it emits by this, so a schema this engine
	// rebuilds from scratch on every flush still reports its objects in the
	// order they were created rather than grouped by kind. Assigned by
	// DB.nextSchemaSeq at CREATE time, recovered from the row's own position at
	// OpenWrite, and carried unchanged through ALTER TABLE (which C SQLite
	// likewise applies in place, leaving the row's rowid alone).
	schemaSeq uint64

	isTemp     bool // TEMP catalog rather than main; see temp_schema.go
	name       string
	sql        string // verbatim CREATE VIEW text, stored in sqlite_schema at Close
	selectStmt *SelectStmt
	colNames   []string // explicit "(col, ...)" rename list; nil if none was given
}

// findViewMeta returns the viewMeta for name (case-insensitive), or nil.
func (db *DB) findViewMeta(name string) *viewMeta {
	return db.findViewMetaIn(scopeAny, name)
}

// viewModifyError returns "cannot modify %s because it is a view" (verified
// directly against C SQLite's own wording) if name (case-insensitive)
// names a view registered against db, or nil otherwise. INSERT/UPDATE/DELETE
// call this once their own db.findTableMeta lookup comes back nil, so they
// can distinguish a genuinely missing table from a read-only view that
// exists but isn't writable through that verb -- views have no INSTEAD OF
// trigger support in this write path (out of scope; see this package's
// AGENTS.md), so every write verb against a view is rejected outright.
func (db *DB) viewModifyError(name string) error {
	if db.findViewMeta(name) != nil {
		return fmt.Errorf("engine: cannot modify %s because it is a view", name)
	}
	return nil
}

// skipCollateAndLikely is sqlite3ExprSkipCollateAndLikely: it peels the two
// node kinds SQLite treats as pure annotations when naming a TABLE's column
// from a select-list item (sqlite3ColumnsFromExprList) -- an explicit COLLATE,
// and the three no-op optimizer hints likely()/unlikely()/likelihood(), whose
// EP_Unlikely flag marks them. So "CREATE VIEW v AS SELECT g COLLATE nocase
// FROM t2" has a column named "g", and so does "SELECT likely(g)" -- while the
// very same expressions written as a top-level result set keep their verbatim
// source text (verified against 3.53.3; see colNameMode.subqueryCols).
func skipCollateAndLikely(e Expr) Expr {
	for {
		switch x := e.(type) {
		case CollateExpr:
			e = x.X
		case FuncExpr:
			// likelihood(X,P) takes two arguments; all three peel to X, and
			// only a plain call does (a window/FILTER spelling is a different
			// node in SQLite, and never carries EP_Unlikely).
			if len(x.Args) == 0 || x.Star || x.Distinct || x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 {
				return e
			}
			switch r33sFoldIdent(x.Name) {
			case "likely", "unlikely", "likelihood":
				e = x.Args[0]
			default:
				return e
			}
		default:
			return e
		}
	}
}

// applyViewColumnNames rewrites cols' names with sqlite3ColumnsFromExprList's
// POST-resolution answer for the view body sub -- the rule a view's stored
// column list is built under (see viewOutputRows). A body this cannot expand
// leaves every name exactly as it was.
func (p *ReadOnlyPager) applyViewColumnNames(sub *SelectStmt, cols []columnInfo) {
	if sub == nil || len(cols) == 0 {
		return
	}
	var scopes []tableScope
	if len(sub.From) > 0 {
		jts, _, err := p.resolveFrom(sub.From, nil)
		if err != nil {
			return
		}
		scopes = buildScopes(jts)
	}
	outCols, err := expandSelectList(sub.Columns, scopes, defaultColNameMode)
	if err != nil || len(outCols) != len(cols) {
		return
	}
	// sqlite3ColumnsFromExprList names and uniquifies in one pass (the ":N"
	// loop, select.c:2295), so a view's column list never holds a repeat.
	// Renaming over already-uniquified names would put the duplicate back
	// and make the duplicate-name decline refuse views C answers:
	//
	//	CREATE VIEW v1 AS SELECT 1 AS a, 2 AS a;  SELECT * FROM v1
	//	  C: a|a:1 = 1|2
	//
	// or "CREATE VIEW v3 AS SELECT * FROM d1 JOIN d2 ON ..." over tables
	// sharing column names. r32mUniqueColumnNames' !ok case (a name still
	// colliding where C would randomize its counter) leaves names plain, and
	// the declines still refuse those.
	names := make([]string, len(outCols))
	for i, oc := range outCols {
		names[i] = oc.name
	}
	names, _ = r32mUniqueColumnNames(names)
	for i := range cols {
		cols[i].Name = names[i]
	}
}

// parsedCreateView is parseCreateViewStmt's result.
type parsedCreateView struct {
	isTemp     bool // TEMP catalog rather than main; see temp_schema.go
	name       string
	ifNotExi   bool
	colNames   []string // nil if no explicit "(col, ...)" list was given
	selectStmt *SelectStmt
}

// parseCreateViewStmt parses "CREATE [TEMP|TEMPORARY] VIEW [IF NOT EXISTS]
// [schema.]name [(col-name [, col-name ...])] AS <select-stmt>". This is the
// SAME parser CreateView (write path) and the read path's schema-row lookup
// (viewDefFromSchemaRow) both use, so a view's in-session definition and its
// materialized-then-reread definition can never disagree.
func parseCreateViewStmt(sqlText string) (*parsedCreateView, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	p := newParser(sqlText, toks)
	// A view's SELECT is SCHEMA text, never a statement the caller wrote:
	// sqlite3EndTable re-parses the stored CREATE VIEW through OP_ParseSchema
	// even on the connection that ran it, so db->init.busy is set every time
	// this body is typed. That is sqlite3MultiValues' condition (b); see
	// valuesFold (sql_ast.go) for the measurement that pins it.
	p.schemaBody = true
	if !p.consumeKeyword("CREATE") {
		return nil, fmt.Errorf("engine: CREATE VIEW: expected CREATE, got %q", p.tokenDesc(p.peek()))
	}
	isTemp := p.consumeKeyword("TEMP") || p.consumeKeyword("TEMPORARY")
	if !p.consumeKeyword("VIEW") {
		return nil, fmt.Errorf("engine: CREATE VIEW: expected VIEW, got %q", p.tokenDesc(p.peek()))
	}
	stmt := &parsedCreateView{}
	if p.consumeKeyword("IF") {
		if !p.consumeKeyword("NOT") || !p.consumeKeyword("EXISTS") {
			return nil, fmt.Errorf("engine: CREATE VIEW: expected NOT EXISTS after IF")
		}
		stmt.ifNotExi = true
	}

	t := p.peek()
	if t.kind != tkIdent {
		return nil, fmt.Errorf("engine: CREATE VIEW: expected view name, got %q", p.tokenDesc(t))
	}
	p.next()
	stmt.name = t.text
	if p.peekIsPunct(".") {
		p.next()
		t2 := p.peek()
		if t2.kind != tkIdent {
			return nil, fmt.Errorf("engine: CREATE VIEW: expected view name after schema qualifier")
		}
		p.next()
		// See parseCreateTableNamePrefix's identical check (schema_write.go):
		// only "main"/"temp" are recognized (they select one of this engine's
		// two catalogs, temp_schema.go); anything else names an ATTACHed
		// database, out of scope for this single-file write path.
		scope, ok := scopeOfQualifier(t.text)
		if !ok {
			return nil, fmt.Errorf("engine: CREATE VIEW: unknown database %s", t.text)
		}
		if isTemp && scope != scopeTemp {
			// "CREATE TEMP VIEW main.vv AS ..." is rejected outright by real
			// SQLite, verified directly -- exactly like a TEMP TABLE's.
			return nil, fmt.Errorf("engine: temporary table name must be unqualified")
		}
		isTemp = scope == scopeTemp
		stmt.name = t2.text
	}
	stmt.isTemp = isTemp

	if p.peekIsPunct("(") {
		p.next()
		for {
			ct := p.peek()
			if ct.kind != tkIdent {
				return nil, fmt.Errorf("engine: CREATE VIEW: expected column name, got %q", p.tokenDesc(ct))
			}
			p.next()
			stmt.colNames = append(stmt.colNames, ct.text)
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}

	if !p.consumeKeyword("AS") {
		return nil, fmt.Errorf("engine: CREATE VIEW: expected AS, got %q", p.tokenDesc(p.peek()))
	}
	// sqlite3CreateView (build.c:2990) never resolves a COLLATE name; it
	// only snapshots the Select (sqlite3SelectDup). Validation happens when
	// the body is first resolved (view_collate.go), so parseCollate's eager
	// rejection is suppressed for this parse: "CREATE VIEW v AS SELECT a
	// COLLATE bogus FROM t" succeeds and fails only on use.
	p.allowUnknownCollation = true
	sel, err := p.parseSelectStmt()
	if err != nil {
		return nil, err
	}
	stmt.selectStmt = sel
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("engine: CREATE VIEW: unexpected trailing input near %q (multi-statement Exec is not supported by this write path)", p.tokenDesc(p.peek()))
	}
	return stmt, nil
}

// CreateView parses and registers a single "CREATE VIEW ..." statement. A
// view's SELECT is deliberately NOT validated against the current schema at
// all here (no table-existence check, no column-count check against an
// explicit column list) -- verified directly against C SQLite: both
// checks fire only once the view is actually queried (see viewOutputRows),
// even when the SELECT names a table that doesn't exist yet. Name-conflict
// checks mirror C SQLite's own verified behavior: IF NOT EXISTS silently
// no-ops against an existing VIEW or TABLE of the same name (checked in that
// order), but does NOT suppress a conflict with an existing INDEX name --
// "there is already an index named %s" is always a hard error, regardless of
// IF NOT EXISTS.
func (db *DB) CreateView(sqlText string) error {
	stmt, err := parseCreateViewStmt(sqlText)
	if err != nil {
		return err
	}
	if err := db.checkReservedObjectName(stmt.name); err != nil {
		return err
	}
	// Collisions are scoped to the catalog this view is created in: a TEMP
	// view may shadow a main table of the same name (view.test 14.x).
	scope := createScope(stmt.isTemp)
	if db.findViewMetaIn(scope, stmt.name) != nil {
		if stmt.ifNotExi {
			return nil
		}
		return fmt.Errorf("engine: view %s already exists", stmt.name)
	}
	if db.findTableMetaIn(scope, stmt.name) != nil {
		if stmt.ifNotExi {
			return nil
		}
		return fmt.Errorf("engine: table %s already exists", stmt.name)
	}
	if db.findIndexMetaIn(scope, stmt.name) != nil {
		return fmt.Errorf("engine: there is already an index named %s", stmt.name)
	}
	// A view's SELECT may carry no bound PARAMETER anywhere -- a view has no
	// call site to bind one at. Verified directly against C SQLite:
	// "CREATE VIEW v AS SELECT a FROM t1 WHERE b=?" (and the ?1 / :n
	// spellings, and one nested inside a subquery) all report "parameters are
	// not allowed in views" (view.test).
	if selectHasParam(stmt.selectStmt) {
		return fmt.Errorf("engine: parameters are not allowed in views")
	}
	// Nor may it reference another database. This engine has one schema, so
	// any qualifier that is not the local one is such a reference -- real
	// SQLite reports "view v13 cannot reference objects in database two"
	// (view.test). A TEMP view is not checked at all: the fixer runs with
	// bTemp set for iDb==1 and skips every source (attach.c:495, :547).
	if schema := selectForeignSchema(stmt.selectStmt, localSchemaOr(db.localSchema), stmt.isTemp); schema != "" && !stmt.isTemp {
		return fmt.Errorf("engine: view %s cannot reference objects in database %s", stmt.name, schema)
	}

	db.views = append(db.views, &viewMeta{
		schemaSeq:  db.nextSchemaSeq(stmt.isTemp),
		isTemp:     stmt.isTemp,
		name:       stmt.name,
		sql:        storedSchemaSQL("VIEW", sqlText, stmt.isTemp),
		selectStmt: stmt.selectStmt,
		colNames:   stmt.colNames,
	})
	db.bumpSchema(stmt.isTemp)
	return nil
}

// removeView removes v from db.views, mirroring removeTableAndIndexes
// (drop_write.go): DropView calls this once it finds v, and Close's
// materialize then simply never emits its sqlite_schema row again.
func (db *DB) removeView(v *viewMeta) {
	for i, x := range db.views {
		if x == v {
			db.views = append(db.views[:i], db.views[i+1:]...)
			break
		}
	}
	// DROP VIEW cascades to the view's own INSTEAD OF triggers, exactly like
	// DROP TABLE cascades to a table's triggers (removeTableAndIndexes,
	// drop_write.go) -- verified directly against C SQLite: after DROP VIEW
	// its INSTEAD OF triggers no longer appear in sqlite_schema, and a freshly
	// recreated same-named view is once again read-only.
	keptTr := db.triggers[:0]
	for _, tr := range db.triggers {
		// v is always a LOCAL view, so a trigger bound to an ATTACHed
		// database's table (triggerMeta.tableAttachName) is kept
		// unconditionally even when its bare name matches v's -- same
		// reasoning as removeTableAndIndexes' identical guard
		// (drop_write.go): a same-named object in a different catalog must
		// never cascade into this one's DROP.
		if tr.tableAttachName != "" || !equalFoldName(tr.table, v.name) || tr.tableIsTemp != v.isTemp {
			keptTr = append(keptTr, tr)
		}
	}
	db.triggers = keptTr
	db.bumpSchema(v.isTemp)
}

// ---- read-path: resolving a FROM-item that names a view ----

// viewDefFromSchemaRow parses a sqlite_schema row's stored CREATE VIEW text
// (found by resolveViewByName) into the SelectStmt/column-rename-list shape
// join.go's resolveFrom needs -- the read-side counterpart of
// parseCreateViewStmt, used identically whether the schema comes from an
// in-flight write session's SnapshotPager image or an already-Close'd,
// reopened file: both go through the exact same ReadOnlyPager.Schema() path.
func viewDefFromSchemaRow(sr SchemaRow, localSchema string) (*parsedCreateView, error) {
	pcv, err := parseCreateViewStmt(sr.SQL)
	if err != nil {
		return nil, fmt.Errorf("engine: view %s: %w", sr.Name, err)
	}
	// The CATALOG the row came from is authoritative, not whether the stored
	// text still spells TEMP (normalizeSchemaSQL may have dropped a qualifier).
	// Only trusted_schema.go's TEMP exemption reads this back.
	pcv.isTemp = sr.Temp
	if !sr.Temp {
		// A MAIN view's body resolves in the MAIN schema, even where a TEMP
		// object of the same name would shadow it for an ordinary unqualified
		// reference -- verified directly against mattn/go-sqlite3: with a main
		// t1 and a temp t1 both holding a row, "CREATE VIEW v1 AS SELECT *
		// FROM t1" reads MAIN's while "CREATE TEMP VIEW v2 AS SELECT * FROM
		// t1" reads temp's (e_dropview.test). Pinning the body's own FROM
		// items to main here is what makes that hold for every later
		// reference; the stored SQL text is untouched.
		qualifyUnqualifiedFromItems(pcv.selectStmt, localSchemaOr(localSchema), nil)
	}
	return pcv, nil
}

// qualifyUnqualifiedFromItems marks every base-table FROM item in sel -- and
// in every subquery, compound arm and CTE body it contains -- as belonging to
// schema, skipping any name bound by a WITH clause in scope (a CTE reference is
// not a schema object at all, and resolveFrom's CTE branch is itself gated on
// the item being unqualified). cteNames carries the names bound by enclosing
// WITH clauses.
//
// schema is the OWNING pager's own schema name, not the literal "main": a view
// stored in an ATTACHed database binds its unqualified references within THAT
// database (cross_db.go), so pinning them to "main" would send them to the
// wrong file.
func qualifyUnqualifiedFromItems(sel *SelectStmt, schema string, cteNames map[string]bool) {
	if sel == nil {
		return
	}
	if len(sel.CTEs) > 0 {
		next := make(map[string]bool, len(cteNames)+len(sel.CTEs))
		for k := range cteNames {
			next[k] = true
		}
		for _, cte := range sel.CTEs {
			next[r33sFoldIdent(cte.Name)] = true
		}
		cteNames = next
	}
	for _, cte := range sel.CTEs {
		qualifyUnqualifiedFromItems(cte.Select, schema, cteNames)
	}
	for i := range sel.From {
		it := &sel.From[i]
		if it.Subquery != nil {
			qualifyUnqualifiedFromItems(it.Subquery, schema, cteNames)
			continue
		}
		if it.Schema == "" && it.Table != "" && !cteNames[r33sFoldIdent(it.Table)] {
			it.Schema = schema
		}
	}
	for _, arm := range sel.Compound {
		qualifyUnqualifiedFromItems(arm.Stmt, schema, cteNames)
	}
}

// resolveViewByName scans p's schema for a view named name (case-
// insensitive). ok is false when no such view exists -- not itself an error;
// callers fall back to their own "no such table" error in that case. A
// non-nil err means the view's OWN stored SQL text fails to parse, which
// (since this write path is what wrote it in the first place) is an
// internal-consistency problem surfaced rather than silently ignored.
func (p *ReadOnlyPager) resolveViewByName(name string) (pcv *parsedCreateView, ok bool, err error) {
	return p.resolveViewByNameIn(scopeAny, name)
}

// resolveViewByNameIn is resolveViewByName scoped to one catalog: scopeAny
// prefers a TEMP view over a main one of the same name (a "CREATE TEMP VIEW
// t1" shadows a main t1 for every unqualified reference, view.test 14.x),
// while scopeMain/scopeTemp match only their own. See temp_schema.go.
func (p *ReadOnlyPager) resolveViewByNameIn(scope schemaScope, name string) (pcv *parsedCreateView, ok bool, err error) {
	rows, err := p.Schema()
	if err != nil {
		return nil, false, err
	}
	var main *SchemaRow
	for i := range rows {
		if rows[i].Type != "view" || !equalFoldName(rows[i].Name, name) || !scope.accepts(rows[i].Temp) {
			continue
		}
		if rows[i].Temp {
			main = &rows[i]
			break
		}
		if main == nil {
			main = &rows[i]
		}
	}
	if main == nil {
		return nil, false, nil
	}
	local := ""
	if p != nil {
		local = p.localSchema // a nil pager is reachable from the write path
	}
	pcv, err = viewDefFromSchemaRow(*main, local)
	if err != nil {
		return nil, false, err
	}
	return pcv, true, nil
}

// rewriteViewNoSuchTable adds "main." to an unqualified "no such table: X"
// error: C reports a table missing from within a view's SQL (its FROM or a
// nested subquery) qualified ("no such table: main.x"), unlike the same miss
// at top level. Applied once at viewOutputRows; a no-op on an already
// prefixed message, so nested views get one prefix.
func rewriteViewNoSuchTable(err error) error {
	if err == nil {
		return nil
	}
	const prefix = "engine: no such table: "
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) {
		return err
	}
	rest := msg[len(prefix):]
	if len(rest) >= 5 && equalFoldName(rest[:5], "main.") {
		return err
	}
	return fmt.Errorf("%smain.%s", prefix, rest)
}

// viewOutputRows executes pcv's stored SELECT (via resolveDerivedRows --
// exactly the same call a parenthesized subquery FROM-item's derived-table
// resolution uses) and applies pcv's explicit column-rename list, if any.
// This is where both of C SQLite's QUERY-TIME-only view checks actually
// live (see this file's package doc comment): a "no such table"/"no such
// column"/etc. error surfacing from anywhere within the SELECT is passed
// through rewriteViewNoSuchTable, and a column-count mismatch against an
// explicit "(col, ...)" list is reported as "expected %d columns for '%s'
// but got %d" -- verified directly against C SQLite's own wording (note
// the argument order: the EXPLICIT list's length first, the SELECT's actual
// column count second).
func (p *ReadOnlyPager) viewOutputRows(viewName string, pcv *parsedCreateView, params []Value) ([]columnInfo, [][]Value, error) {
	// The deferred half of parseCreateViewStmt's own permissiveness: an
	// unrecognized COLLATE name anywhere in this view's body is retained,
	// unresolved, until the view is genuinely referenced -- which this call is
	// (C SQLite's build.c:3115-3183 sqlite3ViewGetColumnNames, the FIRST
	// TIME anything needs this view's Table object). See view_collate.go.
	if name := firstUnknownViewCollation(pcv.selectStmt); name != "" {
		return nil, nil, errUnknownViewCollation(name)
	}
	// A view body is one of the two DDL-origin sites "PRAGMA trusted_schema=OFF"
	// reaches; see trusted_schema.go. A no-op with the ON default.
	if err := p.checkTrustedSchemaSelect(pcv.isTemp, pcv.selectStmt); err != nil {
		return nil, nil, err
	}
	if err := p.declineIfCompoundAffinityUnreproducible(pcv.selectStmt); err != nil {
		return nil, nil, err
	}
	// sqlite3SelectExpand does both halves of this for a view body
	// (select.c:5991):
	//
	//	if( pParse->pWith && (p->selFlags & SF_View) ){
	//	  if( p->pWith==0 ){ p->pWith = <empty With>; }   /* synthesised */
	//	  p->pWith->bView = 1;
	//	}
	//	sqlite3WithPush(pParse, p->pWith, 0);
	//
	// bView is a barrier: searchWith (select.c:5627) stops at it, so a view
	// body cannot see the enclosing query's CTEs (hideCTEScopes). Then the
	// body's own WITH is pushed, so "CREATE VIEW v3 AS WITH t1(p,q) AS
	// (SELECT 9,9) SELECT * FROM t1" reads the CTE (columns p, q).
	defer p.hideCTEScopes()()
	defer p.pushCTEScope(pcv.selectStmt.CTEs)()
	cols, rows, err := p.resolveDerivedRows(pcv.selectStmt, params)
	if err != nil {
		return nil, nil, rewriteViewNoSuchTable(err)
	}
	// A VIEW's column list is built by sqlite3ResultSetOfSelect AFTER its
	// expressions are resolved, unlike an inline derived table's -- so it also
	// peels likely()/unlikely()/likelihood() and resolves a rowid alias to its
	// INTEGER PRIMARY KEY column's name. resolveDerivedRows gave the inline
	// answer; re-derive the names under the resolved rule. Verified against
	// 3.53.3 over t9(g TEXT, h INT) and k(id INTEGER PRIMARY KEY):
	//
	//	CREATE VIEW w AS SELECT unlikely(h) FROM t9; SELECT * FROM w -> "h"
	//	SELECT * FROM (SELECT unlikely(h) FROM t9)                   -> "unlikely(h)"
	p.applyViewColumnNames(pcv.selectStmt, cols)
	if pcv.colNames != nil {
		if len(pcv.colNames) != len(cols) {
			return nil, nil, fmt.Errorf("engine: expected %d columns for '%s' but got %d", len(pcv.colNames), viewName, len(cols))
		}
		renamed := make([]columnInfo, len(cols))
		copy(renamed, cols)
		for i, n := range pcv.colNames {
			renamed[i].Name = n
		}
		// An explicit "(col, ...)" list is NOT exempt from the ":N"
		// uniquifier, contrary to what this file used to record.
		// sqlite3ViewGetColumnNames (build.c:3175) hands that very list to
		// sqlite3ColumnsFromExprList, so "CREATE VIEW v(x,x) AS SELECT 1,2"
		// is accepted -- as recorded -- but its columns come out x and "x:1",
		// and 3.53.3 answers 1|2 for "SELECT * FROM v". Left verbatim here,
		// both columns were named x, so the star expansion bound both to the
		// first one and answered 1|1: a wrong VALUE, not just a wrong name.
		if uniqNames, uniqOK := r32mUniqueColumnNames(pcv.colNames); uniqOK {
			for i := range renamed {
				renamed[i].Name = uniqNames[i]
			}
		}
		cols = renamed
		return cols, rows, nil
	}
	// No explicit column-name list: the view's columns are named exactly like
	// an ordinary derived table's (query.go's errIfDuplicateOutputNames,
	// which this mirrors) -- a case-insensitive duplicate name among them is
	// a shape C SQLite disambiguates with a ":N" suffix that this engine
	// does not reproduce (see that function's doc comment for the identical,
	// already-accepted policy for a parenthesized/derived FROM). Declined
	// cleanly here too, rather than silently exposing two same-named output
	// columns. An explicit "(col, ...)" rename list is exempt (verified
	// directly: "CREATE VIEW v(x,x) AS ..." is accepted by C SQLite with
	// no error at all), which is why this check only runs in this branch.
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		lname := r33sFoldIdent(c.Name)
		if seen[lname] {
			return nil, nil, fmt.Errorf("engine: unsupported: duplicate result-column name %q in view %s (SQLite's ':N' disambiguation is not reproduced)", c.Name, viewName)
		}
		seen[lname] = true
	}
	return cols, rows, nil
}

// declineIfCompoundAffinityUnreproducible guards SQLite ticket 57c47526
// (unionall.test): a compound view used as a derived table whose arms
// disagree on a column's affinity gets one folded affinity
// (sqlite3SubqueryColumnTypes), reproduced by compoundOutputAffinities and
// applied by derivedColumnAffinities. Only when that cannot be computed (an
// arm expression it cannot classify, like a scalar subquery) and the arms'
// first-arm affinities actually disagree is the statement declined.
func (p *ReadOnlyPager) declineIfCompoundAffinityUnreproducible(sub *SelectStmt) error {
	if sub == nil || len(sub.Compound) == 0 {
		return nil
	}
	if _, ok := p.compoundOutputAffinities(sub); ok {
		return nil // faithfully reproducible -- derivedColumnAffinities applies it
	}
	firstAffs, ok := p.armOutputAffinities(sub)
	if !ok {
		return nil // can't analyze -- not itself an error; real resolution below succeeds or fails on its own merits
	}
	for _, arm := range sub.Compound {
		if arm.Stmt == nil {
			continue
		}
		armAffs, ok := p.armOutputAffinities(arm.Stmt)
		if !ok || len(armAffs) != len(firstAffs) {
			continue
		}
		for i, a := range armAffs {
			if a != firstAffs[i] {
				return fmt.Errorf("engine: unsupported: compound view arms disagree on column %d's type affinity", i+1)
			}
		}
	}
	return nil
}

// armOutputAffinities returns one select-core's own output-column
// affinities (schema-only: resolves its FROM into scopes, expands its select
// list, and reads exprAffinity per column) -- exactly the per-arm
// computation join.go's derivedColumnAffinities already does for a
// compound's FIRST arm alone; this exposes the identical computation for an
// ARBITRARY arm, so declineIfCompoundAffinityUnreproducible can compare every arm
// against the first. ok is false for any shape it can't analyze (matching
// derivedColumnAffinities' own "never error, just decline to analyze"
// contract).
func (p *ReadOnlyPager) armOutputAffinities(core *SelectStmt) ([]affinity, bool) {
	if core == nil {
		return nil, false
	}
	var scopes []tableScope
	if len(core.From) > 0 {
		jts, _, err := p.resolveFrom(core.From, nil)
		if err != nil {
			return nil, false
		}
		scopes = buildScopes(jts)
	}
	outCols, err := expandSelectList(core.Columns, scopes, defaultColNameMode)
	if err != nil {
		return nil, false
	}
	ctx := &evalCtx{tables: scopes}
	affs := make([]affinity, len(outCols))
	for i, oc := range outCols {
		affs[i] = exprAffinity(ctx, oc.expr)
	}
	return affs, true
}

// viewBodyFactsKey identifies one view's body for ReadOnlyPager.viewBodyFacts:
// its catalog (a temp view shadows a same-named main one) plus its folded
// name. countViewReference's key omits the catalog, since conflating two views
// only shifts when a count threshold fires; this cache holds column and row
// facts, where conflating them would answer one view's query with the other's
// data.
type viewBodyFactsKey struct {
	temp bool
	name string
}

// viewBodyFactsEntry is what resolveViewRowsGuarded memoizes into
// ReadOnlyPager.viewBodyFacts the first time it fully resolves a given view
// within the current cache's lifetime -- see that field's own doc comment.
type viewBodyFactsEntry struct {
	cols []columnInfo
	rows [][]Value
}

// resolveViewRowsGuarded is resolveFrom's entry point for a FROM item naming a
// view: it pushes viewName onto p.viewExpansion, calls viewOutputRows, and pops
// it, so a view referencing itself through any chain is "view %s is circularly
// defined" rather than unbounded recursion. A repeated non-recursive reference
// (a self-join) pushes and pops in turn.
//
// item is the *FromItem this resolution is for (every caller has one). Two
// caches, checked in order:
//
//  1. viewItemFacts (item's pointer): this exact occurrence was already
//     resolved by another helper asking about the same FromItem. Skips
//     countViewReference, since it is not a new occurrence.
//  2. viewBodyFacts (view name): another occurrence of the same view was
//     resolved. Still counted (select.c's nTabRef++ fires per FROM item) but
//     skips the re-resolution.
//
// A miss on both populates them after a real resolution.
func (p *ReadOnlyPager) resolveViewRowsGuarded(item *FromItem, viewName string, pcv *parsedCreateView, params []Value) ([]columnInfo, [][]Value, error) {
	for _, seen := range p.viewExpansion {
		if equalFoldName(seen, viewName) {
			return nil, nil, fmt.Errorf("engine: view %s is circularly defined", viewName)
		}
	}
	if item != nil {
		if cached, ok := p.viewItemFacts[item]; ok {
			cols := append([]columnInfo(nil), cached.cols...)
			rows := append([][]Value(nil), cached.rows...)
			return cols, rows, nil
		}
	}
	if err := p.countViewReference(viewName); err != nil {
		return nil, nil, err
	}
	key := viewBodyFactsKey{temp: pcv.isTemp, name: r33sFoldIdent(viewName)}
	if cached, ok := p.viewBodyFacts[key]; ok {
		cols := append([]columnInfo(nil), cached.cols...)
		rows := append([][]Value(nil), cached.rows...)
		if item != nil {
			if p.viewItemFacts == nil {
				p.viewItemFacts = map[*FromItem]viewBodyFactsEntry{}
			}
			p.viewItemFacts[item] = viewBodyFactsEntry{cols: cols, rows: rows}
		}
		return cols, rows, nil
	}
	p.viewExpansion = append(p.viewExpansion, viewName)
	cols, rows, err := p.viewOutputRows(viewName, pcv, params)
	p.viewExpansion = p.viewExpansion[:len(p.viewExpansion)-1]
	if err == nil {
		entry := viewBodyFactsEntry{cols: cols, rows: rows}
		if p.viewBodyFacts == nil {
			p.viewBodyFacts = map[viewBodyFactsKey]viewBodyFactsEntry{}
		}
		p.viewBodyFacts[key] = entry
		if item != nil {
			if p.viewItemFacts == nil {
				p.viewItemFacts = map[*FromItem]viewBodyFactsEntry{}
			}
			p.viewItemFacts[item] = entry
		}
	}
	return cols, rows, err
}

// maxViewReferences is C SQLite's own cap on how many times one view may be
// expanded while resolving a single statement, and 65535 is its exact value --
// view3.test asserts the message verbatim, so this is the oracle's number, not
// a chosen one.
const maxViewReferences = 65535

// countViewReference records one expansion of viewName and rejects the
// statement once it exceeds maxViewReferences. This is the fan-out guard,
// distinct from the cycle stack: view3.test's doubling chain has no cycle but
// expands v1 2^16 times, hanging the compiler. C answers `too many references
// to "v1": max 65535`, reproduced verbatim.
func (p *ReadOnlyPager) countViewReference(viewName string) error {
	if p.viewRefCount == nil {
		p.viewRefCount = map[string]int{}
	}
	key := r33sFoldIdent(viewName)
	p.viewRefCount[key]++
	if p.viewRefCount[key] > maxViewReferences {
		return fmt.Errorf("engine: too many references to %q: max %d", viewName, maxViewReferences)
	}
	return nil
}

// fromItemIsDerived reports whether FROM item it is a derived table for
// checkDerivedJoinSupported's purposes: a parenthesized subquery, an in-scope
// CTE, or a view -- all desugar into the same row source and carry the same
// USING/NATURAL-in-a-join-group risk. A schema-read error counts as not
// derived; this is a pre-check, and resolveFrom raises the real error.
func (p *ReadOnlyPager) fromItemIsDerived(it FromItem) bool {
	if it.Subquery != nil {
		return true
	}
	// A materialized nested group's own opaque stand-in (FromItem.
	// NestedGroupSpan's own doc comment) is architecturally a derived table
	// (parse.y's own SF_NestedFrom rebuild wraps it via the SAME
	// sqlite3SrcListAppendFromTerm a plain derived-table FROM item uses --
	// see that field's doc comment for the citation), so it carries the
	// identical rebuilt-column-list naming risk checkDerivedJoinSupported
	// exists to guard against -- conservatively true regardless of what its
	// own nested span actually contains (this call site has no schema handle
	// to recurse into it with, and this fix's own verified scope never
	// exercised a derived table nested inside one).
	if it.NestedGroupSpan != nil {
		return true
	}
	if it.Table == "" {
		return false
	}
	if _, ok := p.lookupCTE(it.Table); ok {
		return true
	}
	// For a cross-database read (attachedReaders set), an item may resolve to a
	// view in an ATTACHED database rather than this primary; consult the pager
	// that actually owns it so a foreign view is still recognized as derived
	// (and thus subject to the NATURAL/USING-with-derived decline in
	// checkDerivedJoinSupported). itemOwner never errors for a resolvable item;
	// a routing error here is treated conservatively as "not derived" and the
	// real error surfaces at resolveFrom.
	rp := p
	if len(p.attachedReaders) > 0 {
		if owner, err := p.itemOwner(it); err == nil {
			rp = owner
		}
	}
	_, ok, err := rp.resolveViewByNameIn(fromItemScope(it), it.Table)
	return err == nil && ok
}

// selectForeignSchema returns the first FROM-clause schema qualifier in sel
// (or any nested subquery/compound arm) that is not local, or "" when every
// reference is local. Used only by CreateView -- see its "cannot reference
// objects in database" check.
func selectForeignSchema(sel *SelectStmt, local string, allowTemp bool) string {
	if sel == nil {
		return ""
	}
	for _, f := range sel.From {
		if f.Schema != "" && !equalFoldName(f.Schema, local) && !(allowTemp && isTempSchemaQualifier(f.Schema)) {
			return f.Schema
		}
		if s := selectForeignSchema(f.Subquery, local, allowTemp); s != "" {
			return s
		}
		if s := exprForeignSchema(f.On, local, allowTemp); s != "" {
			return s
		}
	}
	// A qualifier reached only through an EXPRESSION subquery counts too:
	// "CREATE TRIGGER r5 AFTER INSERT ON t5 BEGIN SELECT 'no-op' ||
	// (SELECT * FROM temp.t6); END" is "trigger r5 cannot reference objects
	// in database temp" in C SQLite, exactly like the plain
	// "... FROM temp.t6" form (attach.test 5.4-5.7, verified directly);
	// walking only the FROM items let the subquery forms through.
	for _, c := range sel.Columns {
		if s := exprForeignSchema(c.Expr, local, allowTemp); s != "" {
			return s
		}
	}
	for _, e := range []Expr{sel.Where, sel.Having} {
		if s := exprForeignSchema(e, local, allowTemp); s != "" {
			return s
		}
	}
	for _, e := range sel.GroupBy {
		if s := exprForeignSchema(e, local, allowTemp); s != "" {
			return s
		}
	}
	for _, ob := range sel.OrderBy {
		if s := exprForeignSchema(ob.Expr, local, allowTemp); s != "" {
			return s
		}
	}
	for _, arm := range sel.Compound {
		if s := selectForeignSchema(arm.Stmt, local, allowTemp); s != "" {
			return s
		}
	}
	for _, cte := range sel.CTEs {
		if s := selectForeignSchema(cte.Select, local, allowTemp); s != "" {
			return s
		}
	}
	return ""
}

// exprForeignSchema returns the first schema qualifier naming a database other
// than local that e's tree reaches through a subquery (see
// selectForeignSchema, which this pairs with).
func exprForeignSchema(e Expr, local string, allowTemp bool) string {
	switch x := e.(type) {
	case nil:
		return ""
	case SubqueryExpr:
		return selectForeignSchema(x.Stmt, local, allowTemp)
	case ExistsExpr:
		return selectForeignSchema(x.Stmt, local, allowTemp)
	case UnaryExpr:
		return exprForeignSchema(x.X, local, allowTemp)
	case BinaryExpr:
		if s := exprForeignSchema(x.L, local, allowTemp); s != "" {
			return s
		}
		return exprForeignSchema(x.R, local, allowTemp)
	case IsNullExpr:
		return exprForeignSchema(x.X, local, allowTemp)
	case CollateExpr:
		return exprForeignSchema(x.X, local, allowTemp)
	case CastExpr:
		return exprForeignSchema(x.X, local, allowTemp)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if s := exprForeignSchema(a, local, allowTemp); s != "" {
				return s
			}
		}
		return ""
	case BetweenExpr:
		for _, a := range []Expr{x.X, x.Lo, x.Hi} {
			if s := exprForeignSchema(a, local, allowTemp); s != "" {
				return s
			}
		}
		return ""
	case LikeExpr:
		for _, a := range []Expr{x.X, x.Pattern, x.Escape} {
			if s := exprForeignSchema(a, local, allowTemp); s != "" {
				return s
			}
		}
		return ""
	case GlobExpr:
		if s := exprForeignSchema(x.X, local, allowTemp); s != "" {
			return s
		}
		return exprForeignSchema(x.Pattern, local, allowTemp)
	case InExpr:
		if s := selectForeignSchema(x.Sub, local, allowTemp); s != "" {
			return s
		}
		if s := exprForeignSchema(x.X, local, allowTemp); s != "" {
			return s
		}
		for _, a := range x.List {
			if s := exprForeignSchema(a, local, allowTemp); s != "" {
				return s
			}
		}
		return ""
	case CaseExpr:
		for _, a := range []Expr{x.Base, x.Else} {
			if a != nil {
				if s := exprForeignSchema(a, local, allowTemp); s != "" {
					return s
				}
			}
		}
		for _, w := range x.Whens {
			if s := exprForeignSchema(w.When, local, allowTemp); s != "" {
				return s
			}
			if s := exprForeignSchema(w.Then, local, allowTemp); s != "" {
				return s
			}
		}
		return ""
	default:
		return ""
	}
}

// selectHasParam reports whether sel (or anything nested in it) contains a
// bound-parameter placeholder. CreateView uses it directly rather than
// SelectStmt.Params, which the view's own sub-parse does not populate.
func selectHasParam(sel *SelectStmt) bool {
	if sel == nil {
		return false
	}
	for _, c := range sel.Columns {
		if exprHasParam(c.Expr) {
			return true
		}
	}
	for _, f := range sel.From {
		if exprHasParam(f.On) || selectHasParam(f.Subquery) {
			return true
		}
		for _, a := range f.TableFuncArgs {
			if exprHasParam(a) {
				return true
			}
		}
	}
	if exprHasParam(sel.Where) || exprHasParam(sel.Having) {
		return true
	}
	for _, g := range sel.GroupBy {
		if exprHasParam(g) {
			return true
		}
	}
	for _, o := range sel.OrderBy {
		if exprHasParam(o.Expr) {
			return true
		}
	}
	if sel.LimitParam != nil || sel.OffsetParam != nil {
		return true
	}
	for _, arm := range sel.Compound {
		if selectHasParam(arm.Stmt) {
			return true
		}
	}
	for _, cte := range sel.CTEs {
		if selectHasParam(cte.Select) {
			return true
		}
	}
	return false
}

// exprHasParam reports whether e contains a ParamExpr anywhere, descending
// into every sub-expression kind a view body can hold -- including a nested
// SELECT, which is where view.test's own case puts it.
func exprHasParam(e Expr) bool {
	switch x := e.(type) {
	case nil:
		return false
	case ParamExpr:
		return true
	case UnaryExpr:
		return exprHasParam(x.X)
	case BinaryExpr:
		return exprHasParam(x.L) || exprHasParam(x.R)
	case CollateExpr:
		return exprHasParam(x.X)
	case CastExpr:
		return exprHasParam(x.X)
	case IsNullExpr:
		return exprHasParam(x.X)
	case LikeExpr:
		return exprHasParam(x.X) || exprHasParam(x.Pattern) || exprHasParam(x.Escape)
	case GlobExpr:
		return exprHasParam(x.X) || exprHasParam(x.Pattern)
	case MatchExpr:
		return exprHasParam(x.X) || exprHasParam(x.Pattern)
	case BetweenExpr:
		return exprHasParam(x.X) || exprHasParam(x.Lo) || exprHasParam(x.Hi)
	case InExpr:
		if exprHasParam(x.X) || selectHasParam(x.Sub) {
			return true
		}
		for _, a := range x.List {
			if exprHasParam(a) {
				return true
			}
		}
	case FuncExpr:
		for _, a := range x.Args {
			if exprHasParam(a) {
				return true
			}
		}
		for _, ob := range x.orderByExprs() {
			if exprHasParam(ob) {
				return true
			}
		}
		return exprHasParam(x.Filter)
	case CaseExpr:
		if exprHasParam(x.Base) || exprHasParam(x.Else) {
			return true
		}
		for _, w := range x.Whens {
			if exprHasParam(w.When) || exprHasParam(w.Then) {
				return true
			}
		}
	case SubqueryExpr:
		return selectHasParam(x.Stmt)
	case ExistsExpr:
		return selectHasParam(x.Stmt)
	case RowExpr:
		for _, v := range x.Elems {
			if exprHasParam(v) {
				return true
			}
		}
	}
	return false
}
