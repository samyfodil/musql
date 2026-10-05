// DROP TABLE/VIEW parsing and execution. A table's b-tree and indexes
// are only rebuilt at Close (writer.go's materialize), so removing
// tableMeta/indexMeta is all DROP needs to do. Views are handled similarly
// via CreateView and persist through OpenWrite/Close.
package engine

import (
	"fmt"
	"strings"
)

// dropQualifiedName parses "[schema.]name" off p (already positioned at the
// target name's first token), returning the bare name to look up, the exact
// text to report in a "no such X" error (schema-qualified, verbatim as
// typed, if a qualifier was given), and whether that qualifier names
// neither "main" nor "temp" -- the only two schemas C SQLite's DROP
// TABLE/VIEW/INDEX recognize.
//
// Unrecognized schema qualifiers are resolved as "object not found" rather
// than "unknown database" (unlike CREATE TABLE). Main and temp catalogs are
// distinct, so "DROP TABLE main.q" for a temp-only q is an error. Unqualified
// names resolve temp-first.
func dropQualifiedName(p *parser, errPrefix string) (bare, display string, scope schemaScope, unknownSchema bool, err error) {
	// objectNameToken, not a bare tkIdent check: a single-quoted STRING is a
	// name wherever SQLite's "nm" production appears, DROP included -- "DROP
	// TABLE '@abc'" is accepted by the reference (verified), and CREATE TABLE
	// here already accepts the same spelling, so refusing it only here made a
	// table creatable and not droppable. See objectNameToken (sql_parser.go).
	t := p.peek()
	first, ok := objectNameToken(t)
	if !ok {
		return "", "", scopeAny, false, fmt.Errorf("%s: expected name, got %q", errPrefix, p.tokenDesc(t))
	}
	p.next()
	if p.peekIsPunct(".") {
		p.next()
		t2 := p.peek()
		n2, ok := objectNameToken(t2)
		if !ok {
			return "", "", scopeAny, false, fmt.Errorf("%s: expected name after schema qualifier", errPrefix)
		}
		p.next()
		bare = n2
		display = first + "." + n2
		sc, ok := scopeOfQualifier(first)
		return bare, display, sc, !ok, nil
	}
	return first, first, scopeAny, false, nil
}

// expectDropTrailer requires nothing but an optional ";" and EOF after a DROP
// TABLE/VIEW/INDEX statement's name, mirroring rejectUnsupportedTableClauses'
// identical trailing check for CREATE TABLE (see that function's doc
// comment): "DROP TABLE t; SELECT 1" as a single Exec call is rejected
// outright rather than silently only dropping t and discarding the second
// statement.
func expectDropTrailer(p *parser, errPrefix string) error {
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return fmt.Errorf("%s: unexpected trailing input near %q (multi-statement Exec is not supported by this write path)", errPrefix, p.tokenDesc(p.peek()))
	}
	return nil
}

// canonicalReservedSchemaTable reports whether bare (case-insensitively)
// names one of the sqlite_-prefixed schema tables C SQLite refuses to
// DROP as either TABLE or VIEW (verified directly: "DROP VIEW sqlite_master"
// gives the identical error text as "DROP TABLE sqlite_master", and it wins
// even over IF EXISTS) under either of their two names apiece
// (sqlite_master/sqlite_schema, sqlite_temp_master/sqlite_temp_schema),
// returning the canonical name C SQLite's own error text always uses
// regardless of which alias or letter-case was typed.
func canonicalReservedSchemaTable(bare string) (canonical string, isReserved bool) {
	switch r33sFoldIdent(bare) {
	case "sqlite_master", "sqlite_schema":
		return "sqlite_master", true
	case "sqlite_temp_master", "sqlite_temp_schema":
		return "sqlite_temp_master", true
	}
	return "", false
}

// parsedDropTableOrView is parseDropTableOrViewStmt's result: enough for
// DropTable/DropView to look the name up and, if it's not found (or names
// the wrong kind of object), report the exact error C SQLite gives.
type parsedDropTableOrView struct {
	bare          string
	display       string
	ifExists      bool
	unknownSchema bool
	scope         schemaScope // catalog a "main."/"temp." qualifier selects
}

// parseDropTableOrViewStmt parses "DROP TABLE|VIEW [IF EXISTS]
// [schema.]name", shared by DropTable and DropView (identical grammar apart
// from the TABLE/VIEW keyword itself).
func parseDropTableOrViewStmt(sqlText, keyword string) (*parsedDropTableOrView, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	p := newParser(sqlText, toks)
	errPrefix := fmt.Sprintf("engine: DROP %s", keyword)
	if !p.consumeKeyword("DROP") {
		return nil, fmt.Errorf("%s: expected DROP, got %q", errPrefix, p.tokenDesc(p.peek()))
	}
	if !p.consumeKeyword(keyword) {
		return nil, fmt.Errorf("%s: expected %s, got %q", errPrefix, keyword, p.tokenDesc(p.peek()))
	}
	stmt := &parsedDropTableOrView{}
	if p.consumeKeyword("IF") {
		if !p.consumeKeyword("EXISTS") {
			return nil, fmt.Errorf("%s: expected EXISTS after IF", errPrefix)
		}
		stmt.ifExists = true
	}
	bare, display, scope, unknownSchema, nerr := dropQualifiedName(p, errPrefix)
	if nerr != nil {
		return nil, nerr
	}
	stmt.bare, stmt.display, stmt.scope, stmt.unknownSchema = bare, display, scope, unknownSchema
	if terr := expectDropTrailer(p, errPrefix); terr != nil {
		return nil, terr
	}
	return stmt, nil
}

// DropTable parses and executes "DROP TABLE [IF EXISTS] [schema.]name":
// removes the table's metadata AND every index (automatic or explicit)
// registered against it (removeTableAndIndexes below) -- the next catalog
// rewrite then simply never emits their sqlite_schema rows again, exactly like
// DropIndex's own single-index removal.
//
// Dropping a table some VIEW still references is allowed outright by real
// SQLite (the DROP itself never inspects other schema objects; the view
// only breaks lazily, the next time it's actually queried) -- moot here
// since this write path has no representation for views at all (see this
// file's own package doc comment), so that scenario cannot actually arise
// against a *DB this package built or opened.
func (db *DB) DropTable(sqlText string) error {
	stmt, err := parseDropTableOrViewStmt(sqlText, "TABLE")
	if err != nil {
		return err
	}
	if canonical, reserved := canonicalReservedSchemaTable(stmt.bare); reserved {
		return fmt.Errorf("engine: table %s may not be dropped", canonical)
	}
	if aerr := db.checkDropSchemaQualifier(stmt.display, stmt.unknownSchema); aerr != nil {
		return aerr
	}
	if !stmt.unknownSchema {
		if tbl := db.findTableMetaIn(stmt.scope, stmt.bare); tbl != nil {
			name := tbl.name
			if tableMayNotBeDropped(name) {
				return fmt.Errorf("engine: table %s may not be dropped", name)
			}
			// See table_load.go's package doc comment: tbl is about to be
			// REMOVED from db.tables below (removeTableAndIndexes), so
			// materialize's own backstop would never load it "for free" --
			// and fkBeforeDropTable, just below, reads tbl.rows directly to
			// apply this DROP's implicit "DELETE FROM parent" foreign-key
			// cascade.
			if err := db.ensureTableLoaded(tbl); err != nil {
				return err
			}
			// An fts4 "content=" table that took its COLUMN LIST from this one
			// would be left reading a table C SQLite has already stopped
			// re-reading -- see fts3_content.go's fts3ContentDependent for the
			// four verified behaviours and why declining here is what keeps the
			// two engines out of that state.
			if dep := db.fts3ContentDependent(name); dep != "" {
				return fmt.Errorf("engine: DROP TABLE %s: %s takes its columns from it (fts4 content=%s), and C SQLite keeps the columns that table already declared for the rest of the connection whatever %s comes back as -- not reproduced by this write path", stmt.display, dep, name, name)
			}
			// With foreign keys enforced, dropping a PARENT behaves like an
			// implicit "DELETE FROM parent": it fires the children's ON DELETE
			// actions and fails if a child would be orphaned. See
			// fkBeforeDropTable for the verified rules (including that the
			// parent's OWN delete triggers do NOT fire).
			if err := db.fkBeforeDropTable(tbl); err != nil {
				return err
			}
			if tbl.autoIncrement {
				if err := db.dropSequenceRows(tbl); err != nil {
					return err
				}
			}
			db.removeTableAndIndexes(tbl)
			if !tbl.isTemp {
				db.stat1 = db.stat1.forgetTable(tbl.name)
			}
			// C SQLite deletes a dropped table's sqlite_stat1 rows along
			// with it -- verified directly (analyze.test): stale stats for an
			// object that no longer exists never survive a DROP. The rows are
			// keyed by TABLE name, which also covers every index it had.
			return db.dropStat1Rows("tbl", name)
		}
		// A virtual table (vtab.go) is dropped with DROP TABLE too, exactly like
		// SQLite. It has no b-tree or indexes to cascade to -- just forget its
		// vtabMeta so Close no longer emits its sqlite_schema row.
		//
		// Scoped by stmt.scope, not the unscoped findVtabMeta: "DROP TABLE
		// temp.t1" must drop the TEMP t1, never a same-named MAIN one --
		// exactly the same catalog rule stmt.scope already applies to the
		// findTableMetaIn/findViewMetaIn lookups just above. Before this, a
		// same-named vtab pair across catalogs (now reachable now that fts3/
		// fts4/fts5's own temp-catalog CREATE is implemented) let an
		// unqualified or wrongly-qualified DROP silently take the wrong
		// catalog's table -- found while wiring fts5's temp support in
		// alongside fts3's.
		if vt := db.findVtabMetaIn(stmt.scope, stmt.bare); vt != nil {
			// ...with one exception: an fts3/fts4 table's data lives in
			// ordinary SHADOW tables created alongside it (vtab_fts3.go), and
			// C SQLite's DROP TABLE takes them with it -- leaving them
			// behind would make the name un-recreatable and the file's schema
			// disagree with what C SQLite writes.
			if fm, isFts3 := fts3ModuleOf(vt); isFts3 {
				// A dropped table's own automerge= resolution must not leak
				// into a DIFFERENT table later created under the same name:
				// C fts3's p->nAutoincrmerge lives on the Fts3Table
				// instance DROP TABLE disconnects (sqlite3_vtab xDisconnect),
				// so a table recreated with this name gets a FRESH one back
				// at 0xff "unknown" -- see engine/fts3_automerge.go's
				// fts3ReadAutoincrmerge, whose own cache would otherwise keep
				// answering the dropped table's last-resolved value forever
				// (verified live: recreating "t" after "automerge=4" without
				// this delete kept answering 4 for the brand new table, which
				// has no %_stat row at all yet).
				delete(db.fts3AutomergeCache, vt.name)
				// The "content=" table is the one exception to the five
				// unconditional drops -- see dropShadowTables. It is decided
				// from the ARGUMENTS alone, never from a re-parsed schema: a
				// content table that has itself been dropped must not turn
				// "<name>_content" back into a shadow name and take a user
				// table with it.
				fm.dropShadowTables(db, vt.name, fm.hasContentOption(vt.args))
			}
			if fm, isFts5 := fts5ModuleOf(vt); isFts5 {
				fm.dropShadowTables(db, vt.name)
			}
			if _, isRtree := vt.store.(*rtreeStore); isRtree {
				// rtree's xDestroy drops all three unconditionally
				// (rtree.c:1083-1085, "DROP TABLE '%q'.'%q_node';" and its
				// two siblings). Leaving them behind is not cosmetic: real
				// SQLite would go on listing them in sqlite_master, and a
				// later CREATE of the same name would collide with its own
				// leftovers.
				for _, suffix := range []string{"_node", "_rowid", "_parent"} {
					if tbl := db.findTableMetaIn(createScope(vt.isTemp), vt.name+suffix); tbl != nil {
						db.removeTableAndIndexes(tbl)
					}
				}
			}
			db.removeVtab(vt)
			return nil
		}
	}
	if stmt.ifExists {
		return nil
	}
	return fmt.Errorf("engine: no such table: %s", stmt.display)
}

// tableMayNotBeDropped is build.c:3497-3504's name rule: every "sqlite_" table
// but sqlite_stat* and sqlite_parameters -- sqlite_sequence among them.
func tableMayNotBeDropped(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "sqlite_") && !strings.HasPrefix(n[7:], "stat") && !strings.HasPrefix(n[7:], "parameters")
}

// dropSequenceRows is sqlite3CodeDropTable's "DELETE FROM %Q.sqlite_sequence
// WHERE name=%Q" (build.c:3416-3425), run before the table's b-tree is dropped.
// The column is untyped, so only a TEXT value equal byte for byte matches.
func (db *DB) dropSequenceRows(tbl *tableMeta) error {
	seq := db.findTableMetaIn(createScope(tbl.isTemp), sqliteSequenceTableName)
	if seq == nil {
		return fmt.Errorf("engine: no such table: %s.%s", map[bool]string{false: "main", true: "temp"}[tbl.isTemp], sqliteSequenceTableName)
	}
	if err := db.ensureTableLoaded(seq); err != nil {
		return err
	}
	for _, rowid := range seq.rows.sortedRowids() {
		if vals := seq.rows.row(rowid); len(vals) > 0 && vals[0].Typ == Text && string(vals[0].S) == tbl.name {
			seq.dropRow(rowid)
		}
	}
	return nil
}

// DropView parses and executes "DROP VIEW [IF EXISTS] [schema.]name": removes
// the view's metadata (view.go's viewMeta) so Close's materialize never emits
// its sqlite_schema row again -- exactly like DropTable's removal, but
// simpler (a view has no b-tree and no indexes of its own to cascade). If no
// view of that name is found, this either rejects name as naming a TABLE
// instead (C SQLite's own "wrong DDL verb" error -- verified directly:
// "DROP VIEW t" on an actual table errors "use DROP TABLE to delete table
// t") or reports "no such view" (verified directly against C SQLite's own
// identical wording for a name that resolves to neither a table nor a view).
func (db *DB) DropView(sqlText string) error {
	stmt, err := parseDropTableOrViewStmt(sqlText, "VIEW")
	if err != nil {
		return err
	}
	if canonical, reserved := canonicalReservedSchemaTable(stmt.bare); reserved {
		return fmt.Errorf("engine: table %s may not be dropped", canonical)
	}
	if aerr := db.checkDropSchemaQualifier(stmt.display, stmt.unknownSchema); aerr != nil {
		return aerr
	}
	if !stmt.unknownSchema {
		if v := db.findViewMetaIn(stmt.scope, stmt.bare); v != nil {
			db.removeView(v)
			return nil
		}
		if db.findTableMetaIn(stmt.scope, stmt.bare) != nil {
			return fmt.Errorf("engine: use DROP TABLE to delete table %s", stmt.bare)
		}
	}
	if stmt.ifExists {
		return nil
	}
	return fmt.Errorf("engine: no such view: %s", stmt.display)
}

// removeTableAndIndexes removes tbl from db.tables, every index registered
// against it (case-insensitively, by indexMeta.table) from db.indexes, and
// every TRIGGER registered against it (case-insensitively, by
// triggerMeta.table) from db.triggers -- verified directly against real
// SQLite: DROP TABLE cascades to a table's own triggers exactly like it
// already does to its indexes (a query against sqlite_schema for
// type='trigger' after dropping the table returns nothing) -- then bumps the
// schema cookie/change counter exactly like every other DDL mutation in this
// package (CreateTable/CreateIndex/DropIndex) does.
func (db *DB) removeTableAndIndexes(tbl *tableMeta) {
	for i, t := range db.tables {
		if t == tbl {
			db.tables = append(db.tables[:i], db.tables[i+1:]...)
			break
		}
	}
	// Matched by CATALOG as well as name: a temp and a main table may share a
	// name, and dropping one must not cascade into the other's indexes or
	// triggers (temp_schema.go).
	kept := db.indexes[:0]
	for _, ix := range db.indexes {
		if !equalFoldName(ix.table, tbl.name) || ix.isTemp != tbl.isTemp {
			kept = append(kept, ix)
		}
	}
	db.indexes = kept
	keptTr := db.triggers[:0]
	for _, tr := range db.triggers {
		// tbl is always a LOCAL table, so a trigger bound to an ATTACHed
		// database's table (tr.tableAttachName != "") is kept unconditionally
		// even when its bare name matches tbl's -- see
		// triggerMeta.tableAttachName's doc comment: trigger1.test 10.x has
		// main.t4, temp.t4 and an attachment's t4 all coexisting, each with
		// its own trigger, and dropping the LOCAL one must never touch the
		// other's.
		if tr.tableAttachName != "" || !equalFoldName(tr.table, tbl.name) || tr.tableIsTemp != tbl.isTemp {
			keptTr = append(keptTr, tr)
		}
	}
	db.triggers = keptTr
	db.bumpSchema(tbl.isTemp)
}
