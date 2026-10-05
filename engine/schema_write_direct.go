// What "PRAGMA writable_schema=ON" (pragma.go) is for: ordinary INSERT/UPDATE/
// DELETE directly against sqlite_master/sqlite_schema, which SQLite's test
// suite uses to build deliberately broken schemas.
//
// The flag selects no second engine: it only stops sqlite_schema counting as
// read-only (tabIsReadOnly, delete.c:103-107, over TF_Readonly set at
// build.c:2674), after which sqlite3IsReadOnly (delete.c:119-124) lets the
// statement reach the ordinary sqlite3Insert/sqlite3Update/sqlite3DeleteFrom
// codegen. So WHERE, SET and VALUES are compiled like any table's
// (OpSchemaWrite, vdbe_schema_write.go); the helpers here only decide where each
// produced value lands in the overlay row.
//
// C SQLite's behavior:
//
//   - With the flag OFF every such statement is "table sqlite_master may not
//     be modified"; DROP TABLE sqlite_master is "table sqlite_master may not
//     be dropped" even with it ON.
//   - The write hits the sqlite_master b-tree only. The connection keeps its
//     loaded schema: after "UPDATE sqlite_master SET sql='CREATE TABLE
//     t1(a,b,c)' WHERE name='t1'", "SELECT sql FROM sqlite_master" shows the
//     new text while "SELECT * FROM t1" and table_info still see two columns,
//     and integrity_check says "ok". A reopen sees three columns; an
//     unparseable row makes the reopen "malformed database schema (t1)".
//   - The schema cookie (schema_version) does not change.
//   - It is transactional: ROLLBACK and ROLLBACK TO undo it.
//   - Reads are not relaxed (writableSchemaResult); the flag survives
//     COMMIT/ROLLBACK.
//   - Column affinity follows TEXT/TEXT/TEXT/INT/TEXT; an arity mismatch is
//     "table sqlite_master has 5 columns but 4 values were supplied"; a column
//     list leaves the rest NULL.
//   - The flag also lifts the "reserved for internal use" check on CREATE
//     ("CREATE TABLE sqlite_foo(x)"); not implemented here, still declined.
//
// This engine has no separate cached schema: db.tables/views/vtabs/triggers/
// indexes are the loaded schema, and the catalog is rendered from them
// (segmentNaturalCatalog). A catalog write is an overlay of per-row column
// overrides (DB.wsEdits) on the rendered rows, so:
//
//   - "SELECT ... FROM sqlite_master" shows the edit;
//   - the live schema is untouched, so other statements run on the pre-write
//     schema, as in C;
//   - SnapshotPager seeds the snapshot's Schema() from the pre-overlay rows,
//     keeping resolveTable/table_info/integrity_check on the pre-write schema;
//   - commit writes the edited catalog (segment_ddl.go), so a reopen fails as
//     C's does.
//
// An override holds only assigned columns. An assigned rootpage is written
// through: C's catalog keeps reporting the assigned value while the
// connection runs on its cached schema.
//
// Declined:
//
//   - assigning ROWID/OID/_ROWID_: C would move the row, which the
//     per-object keying (wsCatalogKey) cannot represent. Reading rootpage or
//     rowid is served (each object keeps C's rowid as a catalog field;
//     catalogRootpages; schemaCatalogQueryGuard).
//   - sqlite_temp_master/sqlite_temp_schema: writableSchemaTarget routes only
//     unqualified or "main." spellings here. Temp objects merely existing is
//     fine (see wsCurrentCatalog and writableSchemaEditedRows).
//   - a catalog write while the schema holds an rtree table, whose shadow rows
//     this engine does not have, so the row set differs
//     (schemaHasPrivateStoreVtabRow).
//   - some DDL once an edit exists (writableSchemaDDLDecline).
//   - INSERT ... SELECT, RETURNING, an OR/UPSERT clause, and UPDATE ... FROM.
package engine

import (
	"errors"
	"fmt"
)

// wsCatalogKey identifies one catalog row across re-renders. A natural row is
// keyed by its object's name plus its catalog: main and temp are separate name
// spaces (a main and a temp "trial" can coexist, pragma.test 3.40), and
// without the temp field an edit on main's row would also hit a same-named
// temp row (in the render, or wsEditForObjectName for ADD COLUMN). An
// INSERTed row has no object and is keyed by its 1-based insertion ordinal
// (always main, as writableSchemaTarget routes only main spellings).
type wsCatalogKey struct {
	ins  int    // 0 for a natural row; else the insertion ordinal
	name string // the natural object's own name; "" for an inserted row
	temp bool   // which catalog the natural object belongs to; see doc comment above
}


// wsCatalogEdit is one writable_schema modification to that row: the columns
// the statement assigned (nil = leave the rendered value, which is how
// rootpage stays live), or the whole row's removal.
type wsCatalogEdit struct {
	deleted bool
	set     [5]*Value
	// rowid is an INSERTED row's sqlite_schema rowid, fixed when it was inserted:
	// OP_NewRowid on the schema b-tree gives max(rowid)+1 (insert.c), and the row
	// keeps it. 0 for a natural row, whose rowid is its object's own.
	rowid int64
}

// catalogRow is one row of the schema catalog, with the key that survives the
// next sync and the ROWID that row has in the catalog -- the number
// "WHERE rowid=2" has to match, gaps and all.
// wsNatural (below) holds the PRE-overlay list; wsCurrentCatalog applies the
// overlay to it.
type catalogRow struct {
	key   wsCatalogKey
	rowid int64
	vals  []Value
}

// wsCatalogColumnCount is sqlite_master's fixed arity -- see
// sqliteSchemaCatalogColumns (query.go), which owns the names/types.
const wsCatalogColumnCount = 5

// writableSchemaEditsActive reports whether this session has any recorded
// catalog edit -- i.e. whether the page-1 image and the live schema have
// parted ways.
func (db *DB) writableSchemaEditsActive() bool { return len(db.wsEdits) > 0 }

// applyWritableSchemaEdit returns row's values with this key's override
// applied, and whether the row survives at all. Called for every natural row a
// reader sees, and by wsCurrentCatalog to build the row set a catalog DML
// statement evaluates against, so the two cannot disagree.
func (db *DB) applyWritableSchemaEdit(key wsCatalogKey, vals []Value) ([]Value, bool) {
	e := db.wsEdits[key]
	if e == nil {
		return vals, true
	}
	if e.deleted {
		return nil, false
	}
	out := append([]Value(nil), vals...)
	for i, v := range e.set {
		if v != nil && i < len(out) {
			out[i] = *v
		}
	}
	return out, true
}

// writableSchemaInsertedRows is the tail appended after every natural row: the
// rows an INSERT INTO sqlite_master added, in insertion
// order, minus any a later DELETE removed.
func (db *DB) writableSchemaInsertedRows() []catalogRow {
	return db.writableSchemaInsertedRowsIn(false)
}

// writableSchemaInsertedRowsIn is writableSchemaInsertedRows for ONE catalog:
// an inserted row belongs to the one its statement named
// (writableSchemaTargetIsTemp), and the TEMP catalog is its own file.
func (db *DB) writableSchemaInsertedRowsIn(temp bool) []catalogRow {
	var out []catalogRow
	for i := 1; i <= db.wsInserted; i++ {
		key := wsCatalogKey{ins: i, temp: temp}
		e := db.wsEdits[key]
		if e == nil || e.deleted {
			continue
		}
		vals := make([]Value, wsCatalogColumnCount)
		for j := range vals {
			if e.set[j] != nil {
				vals[j] = *e.set[j]
			}
		}
		out = append(out, catalogRow{key: key, rowid: e.rowid, vals: vals})
	}
	return out
}

// wsCurrentCatalog is the catalog as it stands now: the natural rows (rendered
// fresh, so rootpages are current) with overrides applied, then the inserted
// tail, filtered to main's rows. Rowids are positions in the natural catalog.
//
// The filter is C's resolution: "sqlite_master" and "sqlite_temp_master" are
// distinct reserved names, one per database (sqliteInt.h:1246-1261), and
// sqlite3FindTable for an unqualified name probes temp's hash then main's
// (build.c:371-388); "sqlite_master" is never a key in temp's hash, so an
// unqualified write always reaches main's catalog, never a temp row.
func (db *DB) wsCurrentCatalog() ([]catalogRow, error) {
	// THE CATALOG IS THE SEGMENT FILE'S DIRECTORY, so its rows are rendered
	// from the live schema (wsCatalogWithOverlay's own wsEnsureNatural). See
	// segment_schema_write.go.
	return db.wsCatalogWithOverlay(), nil
}

// wsCatalogWithOverlay is wsCurrentCatalog's shared tail: db.wsNatural with every
// recorded edit applied, main's rows only, the inserted ones appended, and each
// row's catalog rowid attached.
func (db *DB) wsCatalogWithOverlay() []catalogRow {
	// The natural rows first, because nothing else fills them -- and a caller that reached here without them (the read pager, which
	// renders the catalog a scan sees) rendered the INSERTED rows alone, i.e. a
	// database with every other object missing.
	db.wsEnsureNatural()
	out := make([]catalogRow, 0, len(db.wsNatural))
	for _, r := range db.wsNatural {
		if r.key.temp {
			// TEMP row -- see this function's own doc comment. r.key.temp was
			// already classified once, when the row was rendered -- re-deriving
			// it a second time here from vals' own SQL text would risk
			// disagreeing with it.
			continue
		}
		vals, live := db.applyWritableSchemaEdit(r.key, r.vals)
		if !live {
			continue
		}
		// r.rowid comes along: it IS the row's identity (segmentNaturalCatalog).
		// Dropping it made every row's
		// rowid 0, so the scan's keyOf matched the FIRST row for all of them and
		// "DELETE FROM sqlite_master WHERE name='t2'" deleted t1.
		out = append(out, catalogRow{key: r.key, rowid: r.rowid, vals: vals})
	}
	// An INSERTed row is unconditionally main's (writableSchemaTarget only
	// ever routes an unqualified/"main."-qualified name here -- see
	// wsCatalogKey's own doc comment), never filtered.
	out = append(out, db.writableSchemaInsertedRows()...)
	// Each row keeps the position segmentNaturalCatalog gave it: zeroing it
	// would make every row unmatchable by the scan that feeds OpSchemaWrite
	// (schemaWriteState.keyOf).
	return out
}

// writableSchemaCleanRows is the PRE-overlay schema, as SchemaRow -- what
// SnapshotPager seeds the read snapshot's memoized Schema() with while an edit
// is outstanding, so table resolution keeps using the schema the live session
// still holds instead of re-deriving a deliberately-broken one from the edits.
// nil (no seeding, ordinary scan) when there is no edit.
func (db *DB) writableSchemaCleanRows() []SchemaRow {
	if !db.writableSchemaEditsActive() {
		return nil
	}
	// On the segment format the natural catalog's rootpage column holds the
	// PUBLIC number a reader sees (segmentNaturalCatalog), while table resolution
	// binds by ROUTING root: the pre-edit schema with those is segmentSchemaRows.
	return db.segmentSchemaRows()
}

// writableSchemaEditedRows is writableSchemaCleanRows with
// db.applyWritableSchemaEdit applied per row (dropping deleted ones) plus the
// inserted tail, classified by markTempSchemaRows; nil with no outstanding
// edit.
//
// A filtered catalog read (schemaCatalogRows, temp_catalog.go) is built from a
// SchemaRow list, and SnapshotPager seeds p.Schema() with the pre-edit rows so
// table resolution stays on the live schema. This list
// (ReadOnlyPager.schemaRowsEdited) is the filtered read's source instead, so it
// shows the edit while resolveTable/table_info/integrity_check keep the clean
// list.
func (db *DB) writableSchemaEditedRows() []SchemaRow {
	if !db.writableSchemaEditsActive() {
		return nil
	}
	db.wsEnsureNatural()
	rows := make([]SchemaRow, 0, len(db.wsNatural)+db.wsInserted)
	for _, r := range db.wsNatural {
		vals, live := db.applyWritableSchemaEdit(r.key, r.vals)
		if !live {
			continue
		}
		rows = append(rows, SchemaRow{
			Type:     valueText(vals[0]),
			Name:     valueText(vals[1]),
			TblName:  valueText(vals[2]),
			RootPage: uint32(valueInt(vals[3])),
			SQL:      valueText(vals[4]),
			// r.key.temp was already classified once, when the row was
			// rendered -- reusing it directly
			// here, rather than re-deriving from vals' own (possibly EDITED)
			// SQL text, is what keeps a main-scoped edit from ever being able
			// to flip a temp row's own classification: no edit this overlay
			// can create is ever temp-scoped (wsCurrentCatalog filters to
			// main before any UPDATE/DELETE/INSERT can match a row), so an
			// untouched temp row's r.key.temp is authoritative regardless of
			// what its vals presently read.
			Temp: r.key.temp,
		})
	}
	for _, r := range db.writableSchemaInsertedRows() {
		rows = append(rows, SchemaRow{
			Type:     valueText(r.vals[0]),
			Name:     valueText(r.vals[1]),
			TblName:  valueText(r.vals[2]),
			RootPage: uint32(valueInt(r.vals[3])),
			SQL:      valueText(r.vals[4]),
			// An INSERTed row is unconditionally main's -- see wsCatalogKey's
			// own doc comment -- never reclassified from its own text either.
			Temp: false,
		})
	}
	return rows
}

// schemaCatalogReadOnlyErr is C's refusal, verbatim, for a catalog write with
// the flag off. It is a type so OpSchemaWritePre (vdbe_schema_write.go) can
// recognize it without matching text: it is C's prepare-time refusal
// (sqlite3IsReadOnly, delete.c:121), so changes() keeps its previous value,
// unlike this overlay's own unsupported declines.
type schemaCatalogReadOnlyErr struct{ name string }

func (e schemaCatalogReadOnlyErr) Error() string {
	return fmt.Sprintf("engine: table %s may not be modified", e.name)
}

func errSchemaCatalogNotWritable(name string) error {
	return schemaCatalogReadOnlyErr{name: name}
}

// isSchemaCatalogNotWritable reports whether err is that refusal.
func isSchemaCatalogNotWritable(err error) bool {
	var e schemaCatalogReadOnlyErr
	return errors.As(err, &e)
}

// writableSchemaTarget reports whether a DML statement's target names the main
// schema catalog, and is the entry test for everything in this file. A
// "temp."-qualified target is deliberately NOT one: that names the TEMP
// catalog, which this engine keeps in the same b-tree (see the file comment).
func writableSchemaTarget(schema, table string) bool {
	if isTempSchemaCatalogName(table) || (isTempSchemaQualifier(schema) && isMainSchemaCatalogName(table)) {
		// The TEMP catalog, which is its own file (temp_store.go) and its own
		// key space here (wsCatalogKey.temp). Only an INSERT reaches it --
		// compileSchemaCatalogUpdate/Delete scan main's rows and decline it.
		return schema == "" || isTempSchemaQualifier(schema)
	}
	if schema != "" && !equalFoldName(schema, "main") {
		return false
	}
	return isMainSchemaCatalogName(table)
}

// writableSchemaTargetIsTemp reports which catalog a routed target names.
func writableSchemaTargetIsTemp(schema, table string) bool {
	return isTempSchemaCatalogName(table) || (isTempSchemaQualifier(schema) && isMainSchemaCatalogName(table))
}

// wsGuardedColumns are rowid/oid/_rowid_ -- the catalog's own rowid, under
// every alias C SQLite accepts for it. ASSIGNING to one of them is
// declined unconditionally (see writableSchemaPreflight's cols loop below):
// C SQLite would MOVE the row to the assigned key, a mechanism this
// key-stable overlay (wsCatalogKey) has no representation for at all. READING
// one, in a WHERE or a SET's right-hand side, is served.
var wsGuardedColumns = []string{"rowid", "oid", "_rowid_"}

// writableSchemaPreflight is the shared accept/decline decision every catalog
// DML statement passes through: the flag itself, then the shapes whose exact
// behavior this engine cannot reproduce. verb is only for error text.
func (db *DB) writableSchemaPreflight(verb, table string, cols []string) error {
	if !db.writableSchema {
		return errSchemaCatalogNotWritable(table)
	}
	for _, name := range cols {
		for _, g := range wsGuardedColumns {
			if equalFoldName(name, g) {
				return db.wsGuardedColumnDecline(verb, table, g)
			}
		}
	}
	// Only the rowid assignment above is declined; reading rootpage/rowid
	// is served. An rtree vtab means the catalog's row set differs from C's
	// (its shadow rows have no row here), which would move rows under a
	// per-row override. A same-named temp object is not a reason to
	// decline (wsCurrentCatalog, writableSchemaEditedRows).
	for _, vt := range db.vtabs {
		if _, mod, _, _, perr := parseCreateVirtualTableStmt(vt.sql); perr == nil && vtabPrivateStoreModules[r33sFoldIdent(mod)] {
			return fmt.Errorf("engine: unsupported: %s %s while the schema holds a %q virtual table (C SQLite's catalog carries one row per shadow table and this engine's carries none, so the row an edit would land on is not the same row)", verb, table, mod)
		}
	}
	return nil
}

// wsGuardedColumnDecline words the rowid/oid/_rowid_ assignment decline. Only
// an assignment TARGET reaches it: READING those columns, and rootpage, is
// reproducible now (see writableSchemaPreflight).
func (db *DB) wsGuardedColumnDecline(verb, table, col string) error {
	return fmt.Errorf("engine: unsupported: %s %s ASSIGNING %s (C SQLite would MOVE the row to the newly-assigned key, which this overlay's per-object keying -- wsCatalogKey -- has no representation for at all)", verb, table, col)
}

// writableSchemaDDLDecline is the guard the DDL route (OpDdl, via
// ddlKind.guards) consults once a catalog edit exists. It is narrow: C runs
// ordinary DDL after an edit and just grows the catalog an uncorrupted row
// (misc5.test runs 600 lines of schema building after one whole-catalog
// UPDATE). Declined:
//
//   - DDL inside an open transaction, unless a later ROLLBACK's reload could
//     be reproduced (wsRollbackReloadWouldSucceed). A ROLLBACK that undoes a
//     schema change makes C reload the schema, which silently drops whatever
//     no longer parses: "BEGIN; CREATE TABLE t2(y); ROLLBACK" makes the next
//     "SELECT * FROM t1" "no such table: t1" (misc1.test 23.1, table.test
//     5.2.2), while "BEGIN; INSERT; ROLLBACK" does not. COMMIT, ANALYZE and
//     REINDEX never reload.
//   - ALTER TABLE ADD COLUMN, unless the target table carries the only
//     outstanding edit and it is sql-only (addColumnUnderWritableSchemaEdit,
//     alter_write.go; addColOffset splice, alter.c:406-424). C reloads the
//     whole catalog after every ALTER (renameReloadSchema with a NULL zWhere,
//     alter.c:111, 115-116), absorbing other tables' edits too: with t1 edited
//     to "CREATE TABLE t1(a,b,c)", "ALTER TABLE t2 ADD COLUMN y" makes t1's
//     phantom c live, and with t1 edited to 'zzz' it makes t1 vanish.
//   - RENAME TO is served when every outstanding edit is reloadable: the
//     whole-catalog reload is performed first (reloadSchemaForALTER,
//     schema_reload_image.go) and the ALTER runs over the edited definitions.
//     It still declines for edits the reload cannot represent (type/name/
//     tbl_name/rootpage, inserted rows, virtual tables). RENAME COLUMN, DROP
//     COLUMN and DROP CONSTRAINT stay declined while any edit exists.
//   - CREATE or DROP of an object whose name carries an edit: DROP deletes C's
//     row and the corruption with it, where the overlay would resurrect the
//     edit onto the re-created object.
//
// An unreadable target name is declined too.
//
// pragma.test 3.20 ("ALTER TABLE t1 RENAME TO t1x" with t1 edited to add NOT
// NULL and t1a edited to UNIQUE, over data violating both) stays declined:
// after it, C's integrity_check reports "non-unique entry in index t1a" and
// "NULL value in t1x.a" (pragma.c:1975, 2147), per-row checks this engine's
// integrity_check does not implement (integrityCheckViolations), and adding
// them would widen the schema-hash-order truncation it cannot reproduce
// (pragma.c:1765, 2167). TestWritableSchemaReloadBoundaryStaysDeclined.
func (db *DB) writableSchemaDDLDecline(kw string, toks []token) error {
	if !db.writableSchemaEditsActive() {
		return nil
	}
	if db.inTransaction() && !db.wsRollbackReloadWouldSucceed() && !db.wsEditsAllFromThisTxn() {
		return fmt.Errorf("engine: unsupported: %s inside a transaction after a direct sqlite_master write (a ROLLBACK of a schema change makes C SQLite RELOAD the schema, which over a deliberately-broken catalog drops whatever no longer parses -- this engine's loaded schema is its live one and cannot be reloaded from a corrupted catalog in general; see wsVanishReloadPlan for the one shape it CAN predict, which is not this session's)", kw)
	}
	// Past here: no transaction, or every outstanding edit can be reloaded
	// on ROLLBACK, so the remaining checks are those outside a transaction.
	// Creating a temp object after an edit is fine: writableSchemaEditedRows
	// keeps filtered catalog reads showing the edit, and wsCatalogKey's temp
	// field keeps same-named temp objects apart.
	if kw == "ALTER" {
		name, form, ok := alterTableNameAndForm(toks)
		if !ok {
			return fmt.Errorf("engine: unsupported: ALTER after a direct sqlite_master write (this statement's target table or form could not be identified, and an edit keyed by the wrong object is a silent wrong answer)")
		}
		if form != "ADD" {
			// RENAME's cascade touches rows beyond the altered one, so it is served
			// by performing C's whole-catalog reload first (alter.c:281,
			// renameReloadSchema at alter.c:111; reloadSchemaForALTER), after which
			// no edit is outstanding. ADD COLUMN keeps its own narrower mechanism.
			//
			// Only RENAME TO, over sql-only edits: alter.c validates against the
			// stale schema (alter.c:143, 154-161) and reloads after (:281), while
			// this reloads first. They agree only when validation sees the same
			// objects: RENAME TO checks names, which an sql-only edit cannot change
			// (reloadCatalogTextAgreesWithRow), while RENAME COLUMN and DROP COLUMN
			// take positions from the stale table (alter.c:637, 2272).
			if alterIsRenameTo(toks) && !db.wsEditsDeleteRows() {
				snap := db.captureSnapshot()
				if db.reloadSchemaForALTER() {
					// A RENAME TO that then fails must leave the stale schema,
					// as alter.c's does; opDdl restores this on error.
					db.wsAlterUndo = snap
					return nil
				}
			}
			return fmt.Errorf("engine: unsupported: ALTER TABLE %s after a direct sqlite_master write (see writableSchemaDDLDecline's doc comment for exactly what is and is not reproducible here, and schema_reload_image.go for what the reload it needs can and cannot represent)", name)
		}
		// len(db.wsEdits) is >=1 here (writableSchemaEditsActive() gated
		// entry above) -- ==1 is what says THIS is the only edit the
		// whole-catalog reload C SQLite runs after any ALTER TABLE would
		// have to absorb (see this function's doc comment for the verified
		// evidence: a SECOND table's own edit, even an unrelated one, gets
		// silently reloaded -- or vanished, if it no longer parses -- as a
		// side effect this write path's single-target
		// addColumnUnderWritableSchemaEdit does not reproduce).
		if len(db.wsEdits) != 1 {
			return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN while more than one table's sqlite_master row carries an outstanding direct write (see writableSchemaDDLDecline's doc comment)", name)
		}
		// name has no schema qualifier by this point (alterTableNameAndForm
		// strips one off), so the best available scope guess is the ordinary
		// unqualified (temp-shadows-main) lookup -- see wsEditForObjectName's
		// own doc comment for why an imprecise guess here can only ever
		// UNDER-match, never mis-attribute: addColumn's own real call site
		// (alter_write.go) re-asks with the precisely-resolved tbl.isTemp
		// before ever applying an edit.
		isTemp := false
		if t := db.findTableMeta(name); t != nil {
			isTemp = t.isTemp
		}
		edit := db.wsEditForObjectName(name, isTemp)
		if edit == nil {
			return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN while a DIFFERENT table's sqlite_master row carries the session's own outstanding direct write (see writableSchemaDDLDecline's doc comment)", name)
		}
		if wsEditIsSQLOnly(edit) {
			return nil // addColumnUnderWritableSchemaEdit (alter_write.go) handles this; it may still decline the specific shape for its own documented reasons
		}
		return fmt.Errorf("engine: unsupported: ALTER TABLE %s ADD COLUMN after a direct sqlite_master write to that table's own row (see writableSchemaDDLDecline's doc comment for exactly what is and is not reproducible here)", name)
	}
	name, ok := ddlObjectName(toks)
	if !ok {
		return fmt.Errorf("engine: unsupported: %s after a direct sqlite_master write (this statement's target object could not be identified, and an edit keyed by the wrong object is a silent wrong answer)", kw)
	}
	// Case-INSENSITIVELY: an object name is, so "DROP TABLE T1" targets the
	// row an edit keyed "t1" is on.
	for key := range db.wsEdits {
		if key.ins == 0 && equalFoldName(key.name, name) {
			return fmt.Errorf("engine: unsupported: %s %s after a direct sqlite_master write to that same object's row (C SQLite's DROP removes the edited row outright and a later CREATE gets a clean one; an overlay keyed by object name would put the edit back)", kw, name)
		}
	}
	return nil
}

// alterTableNameAndForm reads an "ALTER TABLE [schema.]name FORM ..."
// statement's own target table name and its FORM ("RENAME"/"ADD"/"DROP")
// off the token stream -- writableSchemaDDLDecline's per-table, per-form
// gate needs both, where ddlObjectName (shared with CREATE/DROP, which have
// no "form" concept) only ever needed the name. Mirrors ddlObjectName's own
// lead-keyword skip and schema-qualifier handling; reported ok=false for
// anything it does not recognize, the same "decline rather than guess"
// contract every other DDL-token-stream reader in this file follows.
func alterTableNameAndForm(toks []token) (name, form string, ok bool) {
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && !toks[i].quoted && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("ALTER") || !kw("TABLE") {
		return "", "", false
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return "", "", false
	}
	name = toks[i].text
	i++
	if i+1 < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." && isNameToken(toks[i+1]) {
		name = toks[i+1].text
		i += 2
	}
	if i >= len(toks) || toks[i].kind != tkIdent || toks[i].quoted {
		return "", "", false
	}
	return name, toks[i].upper(), true
}

// alterIsRenameTo reports whether toks is "ALTER TABLE <name> RENAME TO ...",
// as opposed to RENAME [COLUMN] x TO y. A table itself named "rename" reads as
// not RENAME TO, which only declines.
func alterIsRenameTo(toks []token) bool {
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind == tkIdent && !toks[i].quoted && toks[i].upper() == "RENAME" {
			return toks[i+1].kind == tkIdent && !toks[i+1].quoted && toks[i+1].upper() == "TO"
		}
	}
	return false
}

// wsEditsDeleteRows reports whether any outstanding edit removed a catalog row.
// A deleted row makes the stale and reloaded schemas name different objects,
// so validating against one is not validating against the other.
func (db *DB) wsEditsDeleteRows() bool {
	for _, edit := range db.wsEdits {
		if edit.deleted {
			return true
		}
	}
	return false
}

// wsEditForObjectName returns the outstanding edit on the natural catalog row
// named name (case-insensitively) in catalog temp, or nil. temp matters:
// every natural edit is main-scoped, so an ALTER on a same-named temp table
// must not get main's edit spliced in (addColumnUnderWritableSchemaEdit).
// addColumn passes tbl.isTemp; writableSchemaDDLDecline only has a token
// name and uses findTableMeta's temp-shadows-main resolution, which can only
// under-match.
func (db *DB) wsEditForObjectName(name string, temp bool) *wsCatalogEdit {
	for key, e := range db.wsEdits {
		if key.ins == 0 && key.temp == temp && equalFoldName(key.name, name) {
			return e
		}
	}
	return nil
}

// wsEditIsSQLOnly reports whether e (which may be nil) touched ONLY its
// row's sql column -- the one shape addColumnUnderWritableSchemaEdit
// (alter_write.go) can reproduce; any other edit (type/name/tbl_name/
// rootpage touched too, or the row deleted outright) is a materially
// different, out-of-scope case.
func wsEditIsSQLOnly(e *wsCatalogEdit) bool {
	if e == nil || e.deleted {
		return false
	}
	for i, v := range e.set {
		if i != 4 && v != nil {
			return false
		}
	}
	return e.set[4] != nil && e.set[4].Typ == Text
}

// ddlObjectName is the object a CREATE/DROP/ALTER statement names, read off its
// token stream: the first identifier after the object-kind keywords and any
// IF [NOT] EXISTS, with a "schema." qualifier dropped. Reported ok=false for
// anything it does not recognize, which writableSchemaDDLDecline treats as a
// reason to decline rather than to guess.
func ddlObjectName(toks []token) (string, bool) {
	lead := map[string]bool{
		"CREATE": true, "DROP": true, "ALTER": true, "TEMP": true,
		"TEMPORARY": true, "UNIQUE": true, "VIRTUAL": true, "TABLE": true,
		"INDEX": true, "VIEW": true, "TRIGGER": true, "IF": true,
		"NOT": true, "EXISTS": true,
	}
	i := 0
	// A QUOTED identifier is never one of those keywords, however it is
	// spelled -- "CREATE TABLE \"table\"(x)" names a table called table.
	for i < len(toks) && toks[i].kind == tkIdent && !toks[i].quoted && lead[toks[i].upper()] {
		i++
	}
	if i >= len(toks) {
		return "", false
	}
	// A STRING LITERAL is a legal object name here, and real schemas carry
	// them: "CREATE TABLE IF NOT EXISTS 'bfts_idx_data'(...)" is expert1.test
	// 7.6's own text. objectNameToken is the same acceptance the parser
	// applies (sqlite3NameFromToken over a TK_STRING, parse.y's nm).
	name, ok := objectNameToken(toks[i])
	if !ok {
		return "", false
	}
	if i+2 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." {
		if qualified, qok := objectNameToken(toks[i+2]); qok {
			name = qualified
		}
	}
	return name, true
}

// writableSchemaReload implements the one shape of "PRAGMA writable_schema=
// RESET" reproducible here (narrowing writableSchemaResetDecline, pragma.go):
// edits that rewrote a table's sql only in its tail (the WITHOUT ROWID/STRICT
// clause after the column list), with no other row's type/name/tbl_name/
// rootpage changed.
//
// C's reload (sqlite3ResetAllSchemasOfConnection, build.c:650, from
// pragma.c:1182) re-derives every object from sqlite_master (sqlite3InitOne,
// prepare.c:290ff). Doing that in general here would need re-reading rows by
// rootpage and re-deriving automatic indexes mid-session, which this write
// path has no primitive for.
//
// With a byte-identical column list, rows (affinity is applied at write time)
// and automatic indexes (buildAutoIndexes and finalizeWithoutRowidPK do not
// read NotNull or Aff) are unaffected. Everything else (cols, checks, pkIndex,
// autoIncrement) is re-derived from the new text with OpenWrite's functions,
// because STRICT changes an ANY column's affinity (strictColAffinity) and
// adds NOT NULL to a non-rowid PRIMARY KEY (sqlite3EndTable,
// build.c:2686-2707). strict2.test:
//
//	CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT);
//	INSERT INTO t1 VALUES(1,2),('three','four'),(x'5555','six'),(NULL,'eight');
//	PRAGMA writable_schema=ON;
//	UPDATE sqlite_schema SET sql=(sql||'STRICT') WHERE name='t1';
//	PRAGMA writable_schema=RESET;
//	INSERT INTO t1(x) VALUES('nine');  -- NOT NULL constraint failed: t1.id
//
// (C's integrity_check(t1) then also reports "NULL value in t1.id", which
// this engine's integrity_check does not implement.)
//
// Returns true and clears db.wsEdits only when every outstanding edit fits;
// false leaves everything untouched for the caller's decline.
func (db *DB) writableSchemaReload() bool {
	if db.wsInserted != 0 {
		return false // an INSERTed catalog row would need to become a real, live object -- OpenWrite's full path, not this one
	}
	type pendingReload struct {
		tbl           *tableMeta
		sql           string
		cols          []columnInfo
		checks        []checkConstraint
		strict        bool
		pkIndex       *indexMeta
		autoIncrement bool
	}
	var pending []pendingReload
	for key, edit := range db.wsEdits {
		if edit.deleted || key.ins != 0 {
			return false // a removed row, or one this session inserted -- neither is a plain sql-text edit on an existing table
		}
		for i, v := range edit.set {
			if i != 4 && v != nil {
				return false // type/name/tbl_name/rootpage edited -- out of scope
			}
		}
		if edit.set[4] == nil || edit.set[4].Typ != Text {
			return false // no sql edit at all, or one that isn't a TEXT value -- not a table definition
		}
		// key.temp is load-bearing here exactly as it is in
		// wsEditForObjectName above (see that function's own doc comment):
		// every edit db.wsEdits can ever hold is main-scoped
		// (wsCurrentCatalog filters to main before any UPDATE/DELETE/INSERT
		// can match a row), so key.temp is always false in practice -- but a
		// bare-name lookup that ignores it can still resolve to a SAME-NAMED
		// TEMP table's tableMeta when one precedes the intended MAIN table in
		// db.tables, splicing the main edit's sql/cols/checks/strict onto the
		// wrong catalog's object entirely (confirmed reachable and corrupting
		// by TestWritableSchemaReloadCrossCatalogNameCollisionSafe).
		var tbl *tableMeta
		for _, t := range db.tables {
			if equalFoldName(t.name, key.name) && t.isTemp == key.temp {
				tbl = t
				break
			}
		}
		if tbl == nil {
			return false // the edited row isn't an ordinary table this session has loaded (an index/view/trigger row, or a vtab's)
		}
		newSQL := string(edit.set[4].S)

		// The column-definition list itself -- everything through the
		// top-level closing paren -- must be byte-IDENTICAL; only the tail
		// (WITHOUT ROWID/STRICT) may differ. See this function's own doc
		// comment for exactly what that license does and does not justify.
		oldEnd, oldOK := tableColListEnd(tbl.sql)
		newEnd, newOK := tableColListEnd(newSQL)
		if !oldOK || !newOK || tbl.sql[:oldEnd] != newSQL[:newEnd] {
			return false
		}
		newWithoutRowid, newStrict, tailErr := parseTableTailClauses(newSQL)
		if tailErr != nil || newWithoutRowid != tbl.withoutRowid {
			return false // doesn't parse, or WITHOUT ROWID itself changed -- a storage-representation change this overlay cannot apply
		}

		// Re-run the SAME derivation OpenWrite uses for a table row (see the
		// doc comment above for why this cannot be shortcut).
		cols, checks, specs, perr := parseCreateTableColumnsAndAutoIndexes(newSQL, newWithoutRowid)
		if perr != nil {
			return false
		}
		checks, perr = finalizeCheckConstraints(tbl.name, cols, checks, newWithoutRowid)
		if perr != nil {
			return false
		}
		if len(cols) != len(tbl.cols) {
			// The byte-identical column-list text above should make this
			// impossible; a defensive decline rather than risk a stored-row
			// width mismatch if it somehow isn't.
			return false
		}
		ipk := -1
		for i, c := range cols {
			if c.IsRowidAlias {
				ipk = i
				break
			}
		}
		if ipk != tbl.ipkIndex {
			return false // the rowid-alias column moved -- existing rows' stored shape assumes the OLD position
		}
		_, autoIdxBySpec, perr := buildAutoIndexes(tbl.name, cols, specs)
		if perr != nil {
			return false
		}
		pkIndex, perr := finalizeWithoutRowidPK(tbl.name, newWithoutRowid, cols, specs, autoIdxBySpec)
		if perr != nil {
			return false
		}
		autoIncrement, perr := finalizeAutoIncrement(tbl.name, newSQL, newWithoutRowid, cols, ipk)
		if perr != nil {
			return false
		}
		pending = append(pending, pendingReload{
			tbl: tbl, sql: newSQL, cols: cols, checks: checks, strict: newStrict,
			pkIndex: pkIndex, autoIncrement: autoIncrement,
		})
	}
	for _, p := range pending {
		p.tbl.sql = p.sql
		p.tbl.cols = p.cols
		p.tbl.checks = p.checks
		p.tbl.strict = p.strict
		p.tbl.pkIndex = p.pkIndex
		p.tbl.autoIncrement = p.autoIncrement
		// withoutRowid and ipkIndex are re-verified equal to their existing
		// values above (a storage-representation change is out of scope), so
		// they are left as they were rather than reassigned. db.indexes is
		// untouched for the same reason the doc comment gives: its automatic
		// entries for this table are unaffected by strict.
	}
	db.wsEdits = nil
	return true
}

// tableColListEnd returns the byte offset immediately past a CREATE TABLE
// statement's top-level column-definition list -- i.e. right after the
// closing ")" a WITHOUT ROWID/STRICT tail clause would follow -- via the same
// top-level-paren scan parseTableTailClauses (schema_write.go) performs.
// Factored out separately, rather than having writableSchemaReload reuse that
// function directly, because the caller needs the BYTE OFFSET to compare two
// CREATE TABLE texts' column lists for byte-identity, where
// parseTableTailClauses reports only the two booleans past it.
func tableColListEnd(sqlText string) (int, bool) {
	toks, err := lex(sqlText)
	if err != nil {
		return 0, false
	}
	depth := 0
	seenOpen := false
	for _, t := range toks {
		switch {
		case t.kind == tkPunct && t.text == "(":
			depth++
			seenOpen = true
		case t.kind == tkPunct && t.text == ")":
			depth--
			if seenOpen && depth == 0 {
				return t.End, true
			}
		}
	}
	return 0, false
}

// noteSchemaChange records that a DDL statement succeeded, for
// rollbackTxn/rollbackToSavepoint (DB.txSchemaChanged). Called only from
// opDdl's CREATE/DROP/ALTER cases around each Create*/Drop*/AlterTable call.
// The mutators are all-or-nothing, so err==nil matches build.c's
// unconditional DBFLAG_SchemaChange sites (build.c:588/885/2961/4407,
// trigger.c:768, vdbe.c:4265/7141).
func (db *DB) noteSchemaChange(err error) error {
	if err == nil {
		db.txSchemaChanged = true
	}
	return err
}

// sqlWellFormedStatement reports whether sqlText lexes and balances its
// parentheses, a coarse stand-in for "sqlite3Prepare would accept some
// statement" (sqlite3InitCallback, prepare.c:96, 145, ignores the row's
// type). It separates a syntax error (no object gets built, whatever the
// kind) from text that parses as something this write path cannot re-derive
// (wsVanishReloadPlan declines). misc1.test 23.1 and table.test 5.2.2 are
// unbalanced under any CREATE grammar.
func sqlWellFormedStatement(sqlText string) bool {
	toks, err := lex(sqlText)
	if err != nil {
		return false
	}
	depth := 0
	for _, t := range toks {
		if t.kind != tkPunct {
			continue
		}
		switch t.text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// wsVanishReloadPlan returns the live objects that would vanish under a real
// reload of every outstanding edit (see reloadSchemaFromEdits). ok only when
// every entry is a natural, non-deleted, sql-only edit (wsEditIsSQLOnly) whose
// text is not even well-formed (sqlWellFormedStatement): corruptSchema
// (prepare.c:22-53) records the error but loading continues
// (prepare.c:390-406), so that object is simply not rebuilt. Any other edit
// gives ok=false and nothing applies.
func (db *DB) wsVanishReloadPlan() (names []string, ok bool) {
	for key, edit := range db.wsEdits {
		if key.ins != 0 || edit.deleted || !wsEditIsSQLOnly(edit) {
			return nil, false
		}
		if sqlWellFormedStatement(string(edit.set[4].S)) {
			return nil, false
		}
		names = append(names, key.name)
	}
	return names, len(names) > 0
}

// wsEditsAllFromThisTxn reports whether every outstanding catalog edit was
// made inside the current transaction. Then a ROLLBACK needs no reload:
// restoreSnapshot restores db.wsEdits to its BEGIN state (txn.go), which is the
// schema already running, as sqlite3RollbackAll's reload (main.c:1509) would
// re-derive. expert1.test 7.6.
func (db *DB) wsEditsAllFromThisTxn() bool {
	if db.txSnapshot == nil {
		// A DRIVER-held transaction runs no engine BEGIN at all
		// (DB.heldTransaction): its ROLLBACK discards this whole session and
		// the next statement re-reads the schema off the file, which IS the
		// reload, so every edit this session holds is one of the transaction's.
		return db.heldTransaction
	}
	for key := range db.wsEdits {
		if _, had := db.txSnapshot.wsEdits[key]; had {
			return false
		}
	}
	return true
}

// wsRollbackReloadWouldSucceed reports, without side effects, whether
// reloadSchemaFromEdits would succeed with db.wsEdits as it stands, so
// writableSchemaDDLDecline can allow DDL inside a transaction only when a
// later ROLLBACK could reproduce C's reload. It checks wsVanishReloadPlan
// alone, not writableSchemaReload's tail-only shape (that would need a
// side-effect-free half). An edit made after this check but before the
// ROLLBACK makes reloadSchemaFromEdits apply nothing, never a wrong answer.
func (db *DB) wsRollbackReloadWouldSucceed() bool {
	_, ok := db.wsVanishReloadPlan()
	return ok
}

// wsReloadVanishObject removes the object named name from whichever of
// db.tables/indexes/views/triggers holds it, as sqlite3ResetOneSchema ->
// sqlite3SchemaClear (build.c:626, 640) drops an object whose sql no longer
// parses, so later statements say "no such table".
//
// db.wsEdits keeps the entry, so writableSchemaDDLDecline still refuses to
// reuse the name: C would then hold two physical rows with that name, which
// the one-row-per-name key cannot represent. The vanished row also disappears
// from this engine's sqlite_master (the render is tied to the live objects),
// where C's row survives; a known narrow gap.
//
// Not removeTableAndIndexes: that bumps the schema cookie, which a reload does
// not.
func (db *DB) wsReloadVanishObject(name string) {
	for i, t := range db.tables {
		if equalFoldName(t.name, name) {
			db.tables = append(db.tables[:i], db.tables[i+1:]...)
			// A table's own indexes/triggers cannot resolve their owning
			// table once it is gone -- cascade exactly like
			// removeTableAndIndexes does, minus that function's cookie bump
			// (see this function's own doc comment for why it does not apply
			// to a reload).
			kept := db.indexes[:0]
			for _, ix := range db.indexes {
				if !equalFoldName(ix.table, name) || ix.isTemp != t.isTemp {
					kept = append(kept, ix)
				}
			}
			db.indexes = kept
			keptTr := db.triggers[:0]
			for _, tr := range db.triggers {
				if tr.tableAttachName != "" || !equalFoldName(tr.table, name) || tr.tableIsTemp != t.isTemp {
					keptTr = append(keptTr, tr)
				}
			}
			db.triggers = keptTr
			return
		}
	}
	for i, ix := range db.indexes {
		if equalFoldName(ix.name, name) {
			db.indexes = append(db.indexes[:i], db.indexes[i+1:]...)
			return
		}
	}
	for i, v := range db.views {
		if equalFoldName(v.name, name) {
			db.views = append(db.views[:i], db.views[i+1:]...)
			return
		}
	}
	for i, tr := range db.triggers {
		if equalFoldName(tr.name, name) {
			db.triggers = append(db.triggers[:i], db.triggers[i+1:]...)
			return
		}
	}
	// Not currently found under any kind -- already gone (e.g. a cascade from
	// an earlier name in this same plan already removed it): a harmless no-op.
}

// reloadSchemaFromEdits attempts a schema reload from the outstanding edits,
// mirroring sqlite3ResetAllSchemasOfConnection (build.c:650) and sqlite3InitOne
// (prepare.c:199; sqlite3InitCallback, prepare.c:96/145). Two shapes, each
// all-or-nothing over db.wsEdits:
//
//  1. writableSchemaReload's tail-only table edit (the only shape that turns
//     a table into a different but live one);
//  2. wsVanishReloadPlan: every edited object simply vanishes.
//
// Reports whether either succeeded. Callers treat false as "leave state as
// is", never an error, so it is safe from rollbackTxn/rollbackToSavepoint (C's
// reload never fails outright, prepare.c:390-406) and
// writableSchemaResetDecline.
func (db *DB) reloadSchemaFromEdits() bool {
	if db.writableSchemaReload() {
		return true
	}
	names, ok := db.wsVanishReloadPlan()
	if !ok {
		return false
	}
	for _, name := range names {
		db.wsReloadVanishObject(name)
	}
	return true
}

// ---- the three statement kinds ----

// wsPendingUpdate is one row an UPDATE selected, with the values its SET list
// produced -- one per ASSIGNMENT, in statement order, affinity already applied.
// OpSchemaWrite (vdbe_schema_write.go) builds it out of the register block its
// scan filled.
type wsPendingUpdate struct {
	key  wsCatalogKey
	vals []Value
}

// wsUpdateSetIndexes maps each of stmt's assignments onto the sqlite_master
// column it writes, reporting C SQLite's own prepare-time error
// (update.c:500) for a name the catalog does not have.
func wsUpdateSetIndexes(stmt *updateStmt) ([]int, error) {
	names := make([]string, len(stmt.sets))
	for i, a := range stmt.sets {
		names[i] = a.col
	}
	return wsResolveCatalogColumns(stmt.table, names)
}

// wsResolveCatalogColumns maps names onto sqlite_master's own five columns.
// nil names (an INSERT with no column list) resolves to nil, which is that
// statement's "every column, in order".
func wsResolveCatalogColumns(table string, names []string) ([]int, error) {
	if names == nil {
		return nil, nil
	}
	cols := sqliteSchemaCatalogColumns()
	out := make([]int, len(names))
	for i, name := range names {
		idx := -1
		for j, c := range cols {
			if equalFoldName(c.Name, name) {
				idx = j
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("engine: table %s has no column named %s", table, name)
		}
		out[i] = idx
	}
	return out, nil
}

// wsCheckInsertArity is C SQLite's own prepare-time arity check
// (insert.c:1249-1252), over EVERY tuple: it is decided before any value is
// coded, so a two-row VALUES whose second tuple is short inserts neither.
func wsCheckInsertArity(stmt *insertStmt) error {
	want := wsCatalogColumnCount
	if stmt.cols != nil {
		want = len(stmt.cols)
	}
	for _, row := range stmt.rows {
		if len(row) != want {
			// C SQLite's own wording, verified: "table sqlite_master has 5
			// columns but 4 values were supplied".
			return fmt.Errorf("engine: table %s has %d columns but %d values were supplied", stmt.table, wsCatalogColumnCount, len(row))
		}
	}
	return nil
}

// wsSetValuesFromRegisters turns one row's SET registers into the values the
// overlay stores. The right-hand sides were already evaluated, once per row,
// inside the scan that codes them (update.c:955), and raw holds their results
// in STATEMENT order. All that is left is writeApplySetListPre's slot mapping
// -- which keeps the LAST-WINS rule for a repeated target -- and the catalog's
// own declared TEXT/TEXT/TEXT/INT/TEXT affinities, read back per assignment
// out of the slot it wrote.
func wsSetValuesFromRegisters(raw []Value, setIdx []int) ([]Value, error) {
	cols := sqliteSchemaCatalogColumns()
	newVals := make([]Value, wsCatalogColumnCount)
	if err := writeApplySetListPre(newVals, raw, setIdx); err != nil {
		return nil, err
	}
	out := make([]Value, len(setIdx))
	for j, slot := range setIdx {
		out[j] = applyAffinityToValue(newVals[slot], cols[slot].Aff)
	}
	return out, nil
}

// wsApplyCatalogUpdates is the apply half the UPDATE route ends in: assignment j of
// every selected row lands on the column setIdx[j] of that row's overlay
// entry, in statement order, so a repeated target's LAST assignment wins.
// Reports the statement's affected-row count.
func (db *DB) wsApplyCatalogUpdates(pending []wsPendingUpdate, setIdx []int) int {
	for _, pu := range pending {
		edit := db.wsEditFor(pu.key)
		for j := range pu.vals {
			stored := pu.vals[j]
			edit.set[setIdx[j]] = &stored
		}
	}
	db.wsNoteCatalogWrite(len(pending))
	return len(pending)
}

// wsApplyCatalogDeletes is the apply half the DELETE route ends in: doom every
// selected row in the overlay and record what that costs the session.
// Reports the statement's affected-row count.
func (db *DB) wsApplyCatalogDeletes(doomed []wsCatalogKey) int {
	for _, key := range doomed {
		db.wsEditFor(key).deleted = true
	}
	db.wsNoteCatalogWrite(len(doomed))
	return len(doomed)
}

// wsInsertRowFromValues turns one already-evaluated VALUES tuple into the
// five-wide catalog row the overlay stores: each value into the column its
// position (or the statement's own column list) names, with the catalog's
// declared TEXT/TEXT/TEXT/INT/TEXT affinity applied. Every column the
// statement did not name is SQL NULL, exactly as an ordinary INSERT with no
// DEFAULT would leave it (verified: "INSERT INTO sqlite_master(type,name)
// VALUES('x','y')" reads back x|y|NULL|NULL|NULL).
//
// rowVals is the register block OpSchemaWrite's scan filled
// (vdbe_schema_write.go); this function only places those values, so the
// column mapping lives in exactly one place.
func wsInsertRowFromValues(rowVals []Value, cols []string, colIdx []int) []Value {
	catCols := sqliteSchemaCatalogColumns()
	vals := make([]Value, wsCatalogColumnCount)
	for i, rv := range rowVals {
		target := i
		if cols != nil {
			target = colIdx[i]
		}
		vals[target] = applyAffinityToValue(rv, catCols[target].Aff)
	}
	return vals
}

// wsApplyCatalogInserts is the apply half the INSERT route ends in: every built row
// becomes a new overlay entry, keyed by its 1-based insertion ordinal.
// Reports the statement's affected-row count.
func (db *DB) wsApplyCatalogInserts(built [][]Value, temp bool) int {
	// Each row's ROWID, fixed now: max over the catalog as it stands, plus one per
	// row. Without it every inserted row scanned as rowid 0, so the scan's
	// rowid->key map sent "DELETE FROM sqlite_master WHERE name='y'" to the FIRST
	// inserted row, and it deleted t9 (TestWritableSchemaWriteMatchesCSQLite).
	var top int64
	for _, r := range db.wsCatalogWithOverlay() {
		top = max(top, r.rowid)
	}
	for _, vals := range built {
		db.wsInserted++
		top++
		edit := db.wsEditFor(wsCatalogKey{ins: db.wsInserted, temp: temp})
		edit.rowid = top
		for i := range vals {
			stored := vals[i]
			edit.set[i] = &stored
		}
	}
	db.wsNoteCatalogWrite(len(built))
	return len(built)
}

// errWritableSchemaUnsupported is the one decline writable_schema has on the segment
// format, shared by the two places that can reach it first.
var errWritableSchemaUnsupported = errors.New("writing sqlite_schema directly is not supported on the " +
	"segment format: its catalog is the segment file's directory, not an editable table -- use " +
	"ordinary DDL (ALTER/DROP/CREATE), which rewrites it")

// wsEditFor returns key's edit record, creating it on first write.
func (db *DB) wsEditFor(key wsCatalogKey) *wsCatalogEdit {
	if db.wsEdits == nil {
		db.wsEdits = map[wsCatalogKey]*wsCatalogEdit{}
	}
	e := db.wsEdits[key]
	if e == nil {
		e = &wsCatalogEdit{}
		db.wsEdits[key] = e
	}
	return e
}

// wsNoteCatalogWrite records the statement's change count.
func (db *DB) wsNoteCatalogWrite(n int) {
	db.setChanges(int64(n))
}

func wsSetsContainSubquery(sets []assignment) bool {
	for _, a := range sets {
		if containsSubquery(a.expr) {
			return true
		}
	}
	return false
}

func wsRowsContainSubquery(rows [][]Expr) bool {
	for _, row := range rows {
		for _, e := range row {
			if containsSubquery(e) {
				return true
			}
		}
	}
	return false
}

// cloneWritableSchemaEdits deep-copies the overlay for txSnapshot: a catalog
// write is ordinary transactional DML in C SQLite (verified: ROLLBACK and
// ROLLBACK TO a savepoint both undo one), so it has to be saved and restored
// exactly like a row.
func cloneWritableSchemaEdits(m map[wsCatalogKey]*wsCatalogEdit) map[wsCatalogKey]*wsCatalogEdit {
	if m == nil {
		return nil
	}
	out := make(map[wsCatalogKey]*wsCatalogEdit, len(m))
	for k, e := range m {
		cp := &wsCatalogEdit{deleted: e.deleted}
		for i, v := range e.set {
			if v != nil {
				val := *v
				if v.S != nil {
					val.S = append([]byte(nil), v.S...)
				}
				cp.set[i] = &val
			}
		}
		out[k] = cp
	}
	return out
}
