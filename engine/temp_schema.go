// This file implements the TEMP/main name-resolution rules: objects live in two
// schemas, "main" and "temp". Their storage is temp_store.go's: the TEMP
// database is its own file, as C's db->aDb[1] is, removed when the connection
// closes.
//
// A TEMP object's stored CREATE text keeps its TEMP keyword (normalizeSchemaSQL
// preserves it), and an index or trigger on a TEMP table belongs to temp
// whether or not it was spelled TEMP: after "CREATE TEMP TABLE q(a); CREATE
// INDEX qi2 ON q(a); CREATE TRIGGER trm AFTER INSERT ON q ...",
// sqlite_temp_master lists all three. A temp trigger also gets the keyword
// written in (withTempKeyword), so only an index is classified by its table
// alone. markTempSchemaRows is the one place that classification lives; every
// lookup then resolves through a schemaScope.
//
// The rules:
//
//   - an unqualified name searches temp first, then main (SELECT, DROP and
//     ALTER all pick the temp one);
//   - "main.X" resolves only in main and "temp.X" only in temp;
//   - "CREATE TEMP <object> main.name" is "temporary table name must be
//     unqualified", while "CREATE TABLE temp.q2(a)" makes a temp table;
//   - a new index or trigger lands in its table's catalog, so "CREATE INDEX
//     temp.qi ON q(a)" is valid exactly when q is temp, and "CREATE TEMP
//     TRIGGER trig1 AFTER INSERT ON main.t4" is a temp trigger on a main table
//     that ignores a same-named temp t4 (trigger1.test 10.x);
//   - a main view's body resolves in main (viewDefFromSchemaRow), while an
//     unqualified trigger's ON clause resolves temp-first unless the trigger
//     name said "main." (triggerTargetScope).
//
// temp_catalog.go materializes an empty temp catalog for a connection that
// never opened a temp database.
package engine

import (
	"fmt"
	"strings"
)

// schemaScope is the catalog a written name must resolve in.
type schemaScope uint8

const (
	// scopeAny is an UNQUALIFIED name: temp first, then main.
	scopeAny schemaScope = iota
	scopeMain
	scopeTemp
)

// scopeOfQualifier maps a written schema qualifier to the catalog it names.
// ok is false for anything but ""/"main"/"temp" -- a database this engine has
// no concept of, which each caller turns into its own "unknown database" or
// "no such table" error (they differ; see dropQualifiedName).
func scopeOfQualifier(q string) (s schemaScope, ok bool) {
	switch {
	case q == "":
		return scopeAny, true
	case equalFoldName(q, "main"):
		return scopeMain, true
	case equalFoldName(q, "temp"):
		return scopeTemp, true
	}
	return scopeAny, false
}

// accepts reports whether an object in the given catalog satisfies this scope.
func (s schemaScope) accepts(isTemp bool) bool {
	switch s {
	case scopeMain:
		return !isTemp
	case scopeTemp:
		return isTemp
	}
	return true
}

// isTempCreateSQL reports whether sqlText (a schema row's verbatim stored
// CREATE statement) is itself a "CREATE TEMP ..."/"CREATE TEMPORARY ..."
// statement. This engine stores the original source text (schema_write.go/
// trigger.go/view.go) with the TEMP keyword intact, which is the on-disk
// marker the temp catalog is recovered from. An empty/malformed sqlText (an
// automatic sqlite_autoindex_* row, whose SQL column is NULL) reports false --
// such a row is classified by its table instead (markTempSchemaRows).
func isTempCreateSQL(sqlText string) bool {
	f := strings.Fields(sqlText)
	return len(f) >= 2 && equalFoldName(f[0], "CREATE") &&
		(equalFoldName(f[1], "TEMP") || equalFoldName(f[1], "TEMPORARY"))
}

// markTempSchemaRows fills in SchemaRow.Temp for a freshly read catalog. A
// table or view is classified by its own stored text; an index or trigger
// follows the table its tbl_name names.
//
// When tbl_name exists in both catalogs, creation order decides, as it did
// when the CREATE ran: unqualified names resolve temp first, so the dependent
// is temp iff a temp table of that name already existed:
//
//	CREATE TABLE dup(a); CREATE TEMP TABLE dup(b); CREATE INDEX di ON dup(b);
//	  -- sqlite_temp_master lists di, sqlite_master does not
//	CREATE TABLE dup(a); CREATE INDEX di ON dup(a); CREATE TEMP TABLE dup(b);
//	  -- sqlite_master lists di, sqlite_temp_master does not
//
// Rows are in creation order (segmentSchemaRows sorts by schemaSeq) and a
// dependent never precedes its table, so one forward pass remembering temp
// table names seen so far is exact. A view counts as an owner too: an INSTEAD
// OF trigger on a temp view is temp.
func markTempSchemaRows(rows []SchemaRow) {
	tempOwners := make(map[string]bool)
	for i := range rows {
		if rows[i].Type == "table" || rows[i].Type == "view" {
			rows[i].Temp = isTempCreateSQL(rows[i].SQL)
			if rows[i].Temp {
				tempOwners[r33sFoldIdent(rows[i].Name)] = true
			}
			continue
		}
		rows[i].Temp = isTempCreateSQL(rows[i].SQL) || tempOwners[r33sFoldIdent(rows[i].TblName)]
	}
}

// findSchemaTableRow returns the sqlite_schema row of the TABLE named name that
// satisfies scope, or nil. An unqualified lookup (scopeAny) prefers a temp row
// over a main one whatever their b-tree order; a scoped one matches only its
// own catalog.
func findSchemaTableRow(rows []SchemaRow, scope schemaScope, name string) *SchemaRow {
	var main *SchemaRow
	for i := range rows {
		if rows[i].Type != "table" || !equalFoldName(rows[i].Name, name) {
			continue
		}
		if !scope.accepts(rows[i].Temp) {
			continue
		}
		if rows[i].Temp {
			return &rows[i]
		}
		if main == nil {
			main = &rows[i]
		}
	}
	return main
}

// scopeCacheKey keys a per-scope memo entry for name (see
// ReadOnlyPager.resolvedTables): lower-cased, since resolution is
// case-insensitive, and scope-prefixed, since the same name can now name a
// different object in each catalog.
func scopeCacheKey(scope schemaScope, name string) string {
	return string(rune('0'+scope)) + r33sFoldIdent(name)
}

// withTempKeyword makes sure a temp object's stored CREATE text carries TEMP,
// this engine's marker for the temp catalog. "CREATE TABLE temp.q(a)" and
// "CREATE VIEW temp.v AS ..." do not spell it, and normalizeSchemaSQL drops
// the qualifier, so without this they would reopen under main. Tables, views
// and triggers take it; an index cannot, because C refuses to open a database
// whose schema holds "CREATE TEMP INDEX ...", while it reads a "CREATE TEMP
// TRIGGER" row fine. Text this cannot confidently re-render is returned
// unchanged.
func withTempKeyword(sqlText string) string {
	if isTempCreateSQL(sqlText) {
		return sqlText
	}
	toks, err := lex(sqlText)
	if err != nil || len(toks) < 2 || toks[0].kind != tkIdent || toks[0].upper() != "CREATE" {
		return sqlText
	}
	return sqlText[:toks[1].Start] + "TEMP " + sqlText[toks[1].Start:]
}

// fromItemScope is the catalog a FROM item's written "main."/"temp." qualifier
// selects. An unrecognized qualifier maps to scopeAny, which is harmless here:
// validateSelectSchemaQualifiers (schema_qualifier.go) has already rejected the
// whole statement before any FROM item is resolved.
func fromItemScope(it FromItem) schemaScope {
	s, _ := scopeOfQualifier(it.Schema)
	return s
}

// writeTargetScope is the catalog an INSERT/UPDATE/DELETE target's own written
// qualifier selects. checkWriteSchemaQualifier has already rejected anything
// but ""/"main"/"temp" (or this session's own localSchema) by the time this
// runs, so an unrecognized name safely maps to scopeAny.
func writeTargetScope(q string) schemaScope {
	s, _ := scopeOfQualifier(q)
	return s
}

// declineFileScopedTempPragma rejects a "temp."-qualified PRAGMA whose answer
// comes from the temp database file rather than a schema object, where this
// engine has no exact answer to give. The object-scoped pragmas (table_info,
// index_list, foreign_key_check, ...) honour the qualifier through the
// matching catalog, as C does. Connection-level pragmas ignore it.
//
// page_count/freelist_count are declined on the query path
// (queryPragmaStmt); an Exec discards the row, so execPragma accepts them --
// see its "r35aEmptyTempPragmaName" carve-out.
func declineFileScopedTempPragma(qualifier, name string) error {
	if !isTempSchemaQualifier(qualifier) {
		return nil
	}
	switch r33sFoldIdent(name) {
	case "table_info", "table_xinfo", "index_list", "index_info", "index_xinfo", "foreign_key_list", "foreign_key_check":
		return nil
	case "pragma_list":
		// Not per-database at ALL: pragma.c's PragTyp_PRAGMA_LIST walks the
		// compile-time aPragmaName[] array and never looks at iDb, so the
		// qualifier only makes sqlite3Pragma open the temp database
		// (pragma.c:457) before answering the identical list.
		return nil
	case "full_column_names", "short_column_names":
		// CONNECTION-level, like foreign_keys below and for the same structural
		// reason: PragTyp_FLAG reads and writes db->flags, which has one copy
		// per connection and none per database. So "PRAGMA temp.short_column_names"
		// reports the same bit the bare form does, and setting it through either
		// spelling moves the one flag.
		return nil
	case "user_version", "application_id", "schema_version":
		// The TEMP database's own header holds these (temp_store.go), and both
		// halves are servable now: the GETTER reads that file's header and the
		// SETTER writes it, opening the database exactly as C SQLite does
		// (verified against 3.53.3: "PRAGMA temp.user_version=7" takes
		// "PRAGMA temp.page_count" from 0 to 1, reads back 7, and leaves
		// "PRAGMA main.user_version" at 0).
		return nil
	case "database_list":
		// CONNECTION-wide, and the qualifier is IGNORED: pragma.c's
		// PragTyp_DATABASE_LIST walks db->aDb with no iDb filter at all
		// (pragma.c:1439), so "PRAGMA temp.database_list" reports the same list
		// the bare form does -- verified against 3.53.3.
		return nil
	case "wal_autocheckpoint":
		// CONNECTION-level: pragma.c's PragTyp_WAL_AUTOCHECKPOINT reads and
		// writes db->xWalCallback's own page count, one per connection, and
		// never looks at iDb -- so "PRAGMA temp.wal_autocheckpoint" answers the
		// same 1000 the bare form does (verified directly).
		return nil
	case "foreign_keys", "recursive_triggers", "trusted_schema", "ignore_check_constraints":
		// Connection-level, not per-database: "PRAGMA main.foreign_keys" and
		// "PRAGMA temp.foreign_keys" both answer the one flag. The same holds
		// for recursive_triggers, trusted_schema and ignore_check_constraints
		// -- "PRAGMA temp.ignore_check_constraints=OFF" clears the flag a bare
		// "= ON" set.
		return nil
	case "locking_mode":
		// Per-database, but a temp database's answer is a CONSTANT rather than
		// state: C SQLite's temp database is a tempFile pager, which
		// sqlite3PagerLockingMode() refuses to change and which starts out
		// exclusive. Verified on a brand-new file-backed connection: "PRAGMA
		// temp.locking_mode" answers "exclusive" while main answers "normal",
		// and "PRAGMA temp.locking_mode=normal" answers "exclusive" too. There
		// is no header to read and nothing to share with main, so this one is
		// answerable -- see execLockingMode.
		return nil
	case "journal_mode":
		// Per-database and REAL state, but not from any header: C SQLite
		// keeps the temp database's journal mode on the connection exactly as it
		// keeps main's, so this engine can track it too -- see
		// tempJournalModeResult (pragma.go) for the rules and their evidence.
		return nil
	case "integrity_check", "quick_check":
		// Per-database in C, where the qualifier picks a separate file to walk;
		// here too, through that database's own pager (temp_store.go). The
		// CHECK-constraint half is scoped by the qualifier
		// (integrityCheckViolations): with a violating row in main.t1 and
		// another in temp.tt, the bare form reports both and each qualified
		// form only its own. With nothing violating, temp answers "ok" like
		// main, including the "(N)" form and quoted qualifiers.
		return nil
	case "cache_size":
		// Per-database in C (temp starts at 0, main at -2000, and each setter
		// moves only its own), but a cache size is a page-cache hint that never
		// changes results -- why the unqualified form is in
		// execPragmaSafeNoop. So the qualifier picks between two no-ops. The
		// getter stays declined, as for main: no value is tracked.
		return nil
	}
	return fmt.Errorf("engine: unsupported: PRAGMA temp.%s reads this engine's single database header, which the temp catalog shares with main -- C SQLite keeps a separate one per database", name)
}

// ---- "has the TEMP database been opened yet" (DB.tempDatabaseOpened) ----

// r35aNoteTempDatabaseOpen folds one top-level statement into
// DB.tempDatabaseOpened. Called from mainReadTxnOf and resolveWalIndexRead
// (pragma.go) -- between them, every top-level statement, write or read, passes
// through one of the two.
func (db *DB) r35aNoteTempDatabaseOpen(sqlText string) {
	if db == nil {
		return
	}
	// ...and, in the same pass, whether the temp database has ever held an
	// object, which its page_count turns on (r35aEmptyTempPragmaResult).
	// Reading it off the schema before each statement catches every route
	// (a "temp." CREATE, an index or trigger landing in temp) with one rule.
	//
	// A temp object cannot exist unless the statement that created it opened
	// the temp database, so its presence proves the database is open --
	// which r35aStatementOpensTempDatabase alone cannot see, since it is
	// driven from the read paths and CREATE TEMP TABLE reaches neither.
	// Gated on !tempDatabaseOpened so sessions that never touch temp do not
	// walk the schema per statement.
	if !db.tempDatabaseOpened && db.holdsAnyTempObject() {
		db.tempDatabaseOpened = true
	}
	if db.tempDatabaseOpened && !db.tempDatabaseHeldObject && db.holdsAnyTempObject() {
		db.tempDatabaseHeldObject = true
	}
	if db.tempDatabaseOpened {
		return
	}
	if r35aStatementOpensTempDatabase(sqlText) {
		db.tempDatabaseOpened = true
	}
}

// r35aEmptyTempPragmaResult answers "temp."-qualified pragmas whose value is
// knowable while the temp database has never held an object -- e.g. "PRAGMA
// temp.page_count" on a connection that only used main (pragma.test 14.2).
//
// C creates the temp database lazily and writes nothing until an object needs
// a page, so its page count is 0 and its freelist empty:
//
//	fresh connection                      page_count 0  freelist 0
//	after CREATE TABLE + rows on MAIN     0             0
//	after "SELECT * FROM sqlite_temp_master" (an OPENER)  0   0
//	after PRAGMA integrity_check          0             0
//	after ORDER BY / GROUP BY / DISTINCT  0             0
//	after CREATE TEMP VIEW                1             0
//	after CREATE TEMP TABLE               2             0
//	after CREATE TEMP TABLE + DROP        2             1   (it does NOT shrink)
//
// Past the first temp object it keeps declining.
func (p *ReadOnlyPager) r35aEmptyTempPragmaResult(stmt *PragmaStmt) (cols []string, rows [][]Value, ok bool) {
	if !r35aEmptyTempPragmaName(stmt.Name) || !isTempSchemaQualifier(stmt.Schema) || stmt.HasValue ||
		!p.writeSession.r35aTempDatabaseNeverHeldObject() {
		return nil, nil, false
	}
	return []string{stmt.Name}, [][]Value{{{Typ: Int, I: 0}}}, true
}

// r35aEmptyTempPragmaName is the set r35aEmptyTempPragmaResult answers: the
// header values a temp database that has never been written to reports, every
// one of them 0. user_version/application_id/schema_version join page_count and
// freelist_count because a header that does not exist yet reads as zeroes in
// C SQLite too -- verified on a fresh connection, all five answer 0.
func r35aEmptyTempPragmaName(name string) bool {
	switch r33sFoldIdent(name) {
	case "page_count", "freelist_count", "user_version", "application_id", "schema_version":
		return true
	}
	return false
}

// r35aTempDatabaseNeverHeldObject reports whether the TEMP database has never
// held an object this session -- so nothing has ever been written into it and
// its page count is still 0. A nil DB (a read-only pager with no write session
// behind it) cannot answer, and says so.
func (db *DB) r35aTempDatabaseNeverHeldObject() bool {
	return db != nil && !db.tempDatabaseHeldObject && !db.holdsAnyTempObject()
}

// StatementOpensTempDatabase is r35aStatementOpensTempDatabase for the
// driver's read path: a pure SELECT runs on a snapshot with no write session,
// so the driver records it on the Conn (Conn.tempOpened) for "PRAGMA
// database_list". That makes an over-approximation a visible wrong answer
// rather than a decline, so the table below is exact, not merely
// conservative, and compat-harness/r35a_tempdb_open_probe_test.go keeps it so.
func StatementOpensTempDatabase(sqlText string) bool {
	return r35aStatementOpensTempDatabase(sqlText)
}

// r35aStatementOpensTempDatabase reports whether preparing sqlText makes C
// open its temp database (sqlite3OpenTempDatabase, build.c:5323), from its two
// call sites:
//
//   - sqlite3CodeVerifySchemaAtToplevel for iDb==1 (build.c:5366): any
//     statement naming the temp schema -- a TEMP/TEMPORARY keyword, a "temp."
//     qualifier, a temp catalog name, or an unqualified name resolving to a
//     temp object (which an earlier opener must have created);
//   - pragma.c:457, "if( iDb==1 && sqlite3OpenTempDatabase(pParse) )": any
//     "temp."-qualified pragma.
//
// Plus PRAGMA integrity_check / quick_check, which call
// sqlite3CodeVerifySchema for every database (pragma.c:1741) -- but not
// "PRAGMA main.integrity_check", which skips i!=iDb first.
//
// PRAGMA database_list shows the temp row exactly when it is open, which is
// how the table is checked:
//
//	opens it   CREATE TEMP TABLE (and after DROP TABLE of it); CREATE TABLE
//	           temp.q; SELECT ... FROM sqlite_temp_master / sqlite_temp_schema;
//	           PRAGMA temp.<anything>; PRAGMA integrity_check; PRAGMA
//	           quick_check; PRAGMA integrity_check(t)
//	leaves it  CREATE/INSERT/SELECT on main; ORDER BY, GROUP BY and DISTINCT
//	 shut      (a sorter's transient temp use is not the temp DATABASE);
//	           REINDEX; ANALYZE; VACUUM; ATTACH; SAVEPOINT; a view; a trigger;
//	           PRAGMA temp_store (get AND set); PRAGMA main.integrity_check;
//	           PRAGMA table_list / optimize / schema_version / writable_schema /
//	           foreign_key_check
func r35aStatementOpensTempDatabase(sqlText string) bool {
	return stmtTextFlagsFor(sqlText).opensTempDatabase
}

// r35aStatementOpensTempDatabaseUncached is the rule itself; the memo above is
// the only thing that calls it. See stmtTextFlags.
func r35aStatementOpensTempDatabaseUncached(sqlText string) bool {
	// Every spelling below contains "temp" or "check" as a substring, so this
	// rejects the overwhelming majority of statements without a second lex --
	// which matters, because this runs on the statement path for every
	// statement of every session until the temp database is opened.
	if !r35aContainsFold(sqlText, "temp") && !r35aContainsFold(sqlText, "check") {
		return false
	}
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil {
		// Text this engine cannot even lex says nothing about what real
		// SQLite's parser did with it; count it as an opener rather than
		// assume it never reached code generation.
		return true
	}
	// ATTACH/DETACH name a schema without generating code that VERIFIES one:
	// both are run-time function calls (attachFunc/detachFunc, attach.c), so
	// sqlite3CodeVerifySchemaAtToplevel never sees iDb==1 and the temp database
	// stays shut. Measured against 3.53.3 through detachFunc's own tell: its
	// lookup SKIPS a database whose pBt is 0 (attach.c:310-311), so a FIRST
	// "DETACH temp" answers "no such database: temp" -- which it could not if
	// the statement itself had just opened it -- while the same statement after
	// a CREATE TEMP TABLE answers "cannot detach database temp". ATTACH was
	// already on the "leaves it shut" list above and this is its other half.
	if len(toks) > 0 && toks[0].kind == tkIdent && !toks[0].quoted {
		switch r33sFoldIdent(toks[0].text) {
		case "attach", "detach":
			return false
		}
	}
	for _, t := range toks {
		if t.kind != tkIdent || t.quoted {
			// A QUOTED spelling is an ordinary object name -- "CREATE TABLE
			// \"temp\"(x)" names a main table -- and never a keyword or a
			// schema qualifier.
			continue
		}
		switch r33sFoldIdent(t.text) {
		case "temp", "temporary", "sqlite_temp_master", "sqlite_temp_schema",
			"integrity_check", "quick_check":
			return true
		}
	}
	return false
}

// r35aContainsFold is an ASCII-case-insensitive substring test that allocates
// nothing -- sub must already be lower-case. strings.Contains over a lowered
// copy would allocate one string per statement on the hot path.
func r35aContainsFold(s, sub string) bool {
	if len(sub) == 0 || len(s) < len(sub) {
		return len(sub) == 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		j := 0
		for ; j < len(sub); j++ {
			if asciiLowerByte(s[i+j]) != sub[j] {
				break
			}
		}
		if j == len(sub) {
			return true
		}
	}
	return false
}

// r35aTempStoreValue is pragma.c's getTempStore(): the value is read from its
// FIRST CHARACTER, '0'..'2' being that digit, and otherwise "file" is 1,
// "memory" is 2 and anything else (including "3" and "0.5", which stops at its
// leading '0') is 0.
func r35aTempStoreValue(text string) uint8 {
	z := strings.TrimSpace(text)
	if z == "" {
		return 0
	}
	if z[0] >= '0' && z[0] <= '2' {
		return z[0] - '0'
	}
	switch r33sFoldIdent(z) {
	case "file":
		return 1
	case "memory":
		return 2
	}
	return 0
}

// containsKeyword reports whether sqlText contains kw as a whole identifier
// token (case-insensitive).
func containsKeyword(sqlText, kw string) bool {
	toks, err := lex(sqlText)
	if err != nil {
		return strings.Contains(strings.ToUpper(sqlText), kw)
	}
	for _, t := range toks {
		if t.kind == tkIdent && !t.quoted && t.upper() == kw {
			return true
		}
	}
	return false
}

// createScope is the lookup scope a CREATE statement's own collision checks
// use: an object being created in one catalog collides only with that
// catalog's own objects.
func createScope(isTemp bool) schemaScope {
	if isTemp {
		return scopeTemp
	}
	return scopeMain
}

// storedSchemaSQL is normalizeSchemaSQL plus the temp catalog's on-disk marker
// (withTempKeyword) -- what a TABLE's or VIEW's sqlite_schema row stores.
func storedSchemaSQL(kind, sqlText string, isTemp bool) string {
	out := normalizeSchemaSQL(kind, sqlText)
	if isTemp {
		out = withTempKeyword(out)
	}
	return out
}

// withTempKeywordIf is withTempKeyword gated on isTemp.
func withTempKeywordIf(sqlText string, isTemp bool) string {
	if !isTemp {
		return sqlText
	}
	return withTempKeyword(sqlText)
}

// findTableMetaIn / findViewMetaIn / findIndexMetaIn / findTriggerMetaIn are
// the write-side counterparts of resolveTableIn: an unqualified lookup
// (scopeAny) searches the TEMP catalog first and then main, a scoped one only
// its own. Each unscoped find* (schema_write.go, view.go, index_write.go,
// trigger.go) is exactly the scopeAny case.
func (db *DB) findTableMetaIn(scope schemaScope, name string) *tableMeta {
	var main *tableMeta
	for _, t := range db.tables {
		if !equalFoldName(t.name, name) || !scope.accepts(t.isTemp) {
			continue
		}
		if t.isTemp {
			return t
		}
		if main == nil {
			main = t
		}
	}
	return main
}

func (db *DB) findViewMetaIn(scope schemaScope, name string) *viewMeta {
	var main *viewMeta
	for _, v := range db.views {
		if !equalFoldName(v.name, name) || !scope.accepts(v.isTemp) {
			continue
		}
		if v.isTemp {
			return v
		}
		if main == nil {
			main = v
		}
	}
	return main
}

func (db *DB) findIndexMetaIn(scope schemaScope, name string) *indexMeta {
	var main *indexMeta
	for _, ix := range db.indexes {
		if !equalFoldName(ix.name, name) || !scope.accepts(ix.isTemp) {
			continue
		}
		if ix.isTemp {
			return ix
		}
		if main == nil {
			main = ix
		}
	}
	return main
}

func (db *DB) findTriggerMetaIn(scope schemaScope, name string) *triggerMeta {
	var main *triggerMeta
	for _, tr := range db.triggers {
		if !equalFoldName(tr.name, name) || !scope.accepts(tr.isTemp) {
			continue
		}
		if tr.isTemp {
			return tr
		}
		if main == nil {
			main = tr
		}
	}
	return main
}

// findVtabMetaIn is findVtabMeta restricted to scope. CREATE VIRTUAL TABLE's
// collision checks and DROP/INSERT/UPDATE/DELETE dispatch use it with the
// same scope their table/view lookup computed, so a "temp."-qualified
// statement cannot reach a same-named main vtab. Other callers (fk.go, read
// paths) use the unscoped findVtabMeta.
func (db *DB) findVtabMetaIn(scope schemaScope, name string) *vtabMeta {
	var main *vtabMeta
	for _, v := range db.vtabs {
		if !equalFoldName(v.name, name) || !scope.accepts(v.isTemp) {
			continue
		}
		if v.isTemp {
			return v
		}
		if main == nil {
			main = v
		}
	}
	return main
}

// hasTempObject reports whether this write session holds any TEMP-catalog
// object.
func (db *DB) hasTempObject() bool {
	for _, t := range db.tables {
		if t.isTemp {
			return true
		}
	}
	for _, v := range db.views {
		if v.isTemp {
			return true
		}
	}
	for _, ix := range db.indexes {
		if ix.isTemp {
			return true
		}
	}
	for _, tr := range db.triggers {
		if tr.isTemp {
			return true
		}
	}
	for _, vt := range db.vtabs {
		if vt.isTemp {
			return true
		}
	}
	return false
}

// DropTempObjects removes every TEMP-catalog object -- table, view, index,
// trigger and virtual table -- from this session's schema, and frees the
// b-trees they own, so the next Close commits a file holding only the main
// schema. It reports whether anything was actually removed.
//
// "PRAGMA temp_store" is what reaches it (pragma.go): a change of temp storage
// discards the temp database, exactly as invalidateTempStorage does
// (pragma.c:189-206). A connection ending is the driver's own business -- it
// removes the temp FILE (driver's Conn.dropTempObjects).
func (db *DB) DropTempObjects() bool {
	before := len(db.tables) + len(db.views) + len(db.indexes) + len(db.triggers) + len(db.vtabs)
	// The objects' pages go with the DATABASE they live in: dropping every temp
	// object empties the TEMP database, which is its own file (temp_store.go).
	// C SQLite does exactly this -- invalidateTempStorage closes aDb[1]'s
	// Btree and its temp file goes with it (pragma.c:189-206) -- and it is why
	// nothing in MAIN's file has to be freed here at all.
	if err := db.emptyTempDatabase(); err != nil {
		db.deferErr(err)
	}
	tables := db.tables[:0]
	for _, t := range db.tables {
		if !t.isTemp {
			tables = append(tables, t)
		}
	}
	db.tables = tables
	views := db.views[:0]
	for _, v := range db.views {
		if !v.isTemp {
			views = append(views, v)
		}
	}
	db.views = views
	indexes := db.indexes[:0]
	for _, ix := range db.indexes {
		if !ix.isTemp {
			indexes = append(indexes, ix)
		}
	}
	db.indexes = indexes
	triggers := db.triggers[:0]
	for _, tr := range db.triggers {
		if !tr.isTemp {
			triggers = append(triggers, tr)
		}
	}
	db.triggers = triggers
	vtabs := db.vtabs[:0]
	for _, vt := range db.vtabs {
		if !vt.isTemp {
			vtabs = append(vtabs, vt)
		}
	}
	db.vtabs = vtabs
	changed := before != len(db.tables)+len(db.views)+len(db.indexes)+len(db.triggers)+len(db.vtabs)
	if changed {
		db.bumpSchema(true)
	}
	return changed
}
