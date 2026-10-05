// This file implements VACUUM and REINDEX.
//
// REINDEX is a no-op here: indexes store no entries on this format (their
// answers derive from the rows), so there is nothing to rebuild. Its parsing
// must still accept exactly C's forms, since accepting one C rejects would be
// a wrong answer:
//   - bare "REINDEX";
//   - "REINDEX <collation>" for BINARY/NOCASE/RTRIM (a bare name is tried as a
//     collation first, then as an object);
//   - "REINDEX <table-index-view-or-vtab>" existing in either catalog;
//   - "REINDEX <schema>.<object>" existing in that schema only.
//
// Otherwise: "unable to identify the object to be reindexed", or "unknown
// database <name>" for an unknown qualifier.
//
// VACUUM's one observable effect is renumbering rowids: a table's rowids
// become 1..N (ascending old rowid, signed) iff it is a rowid table with no
// INTEGER PRIMARY KEY and no index of any kind. Any index makes C keep the
// rowids. Since that means there is never an index to fix up, rekeying the row
// store is the whole operation.
//
// "VACUUM INTO 'file'" is a separate statement that does not renumber rowids
// (execVacuumInto).
//
// Declined: VACUUM inside a transaction (C: "cannot VACUUM from within a
// transaction"); "VACUUM <name>" for anything but main/temp is "unknown
// database <name>", as in C. "VACUUM temp" is accepted as a no-op.
package engine

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// isVacuumReindexSchemaName reports whether name is one of the two schema
// names VACUUM and a qualified REINDEX accept -- "main" and "temp" are always
// valid schema names in C SQLite regardless of ATTACH/TEMP state (the same
// rule ANALYZE's isAnalyzeSchemaName encodes).
func isVacuumReindexSchemaName(name string) bool {
	return equalFoldName(name, "main") || equalFoldName(name, "temp")
}

// isReindexCollationName reports whether name (case-insensitively) is one of
// C SQLite's three built-in collating sequences. A bare "REINDEX <name>"
// resolves name as a collation before trying it as a table/index, and these
// are the only collations the oracle (and this engine) has, so any other bare
// name must resolve to an existing object or be rejected.
func isReindexCollationName(name string) bool {
	switch strings.ToUpper(name) {
	case "BINARY", "NOCASE", "RTRIM":
		return true
	}
	return false
}

// reindexTargetExistsIn reports whether name (case-insensitively) is an
// existing table, index, view or virtual table in scope's catalog -- what C's
// REINDEX resolves through sqlite3FindTable/FindIndex (a collation is handled
// separately, for a bare name only). A view or vtab is accepted and has
// nothing to rebuild. scope is strict: with a main m1 and a temp tt1, "REINDEX
// main.tt1" and "REINDEX temp.m1" are rejected.
func (db *DB) reindexTargetExistsIn(scope schemaScope, name string) bool {
	return db.findTableMetaIn(scope, name) != nil ||
		db.findIndexMetaIn(scope, name) != nil ||
		db.findViewMetaIn(scope, name) != nil ||
		// A VIRTUAL table too: sqlite3Reindex looks the name up with
		// sqlite3FindTable, which finds one, and reindexTable then loops over
		// its (empty) index list -- so "REINDEX <vtab>" is a silent no-op
		// rather than "unable to identify the object to be reindexed".
		db.findVtabMetaIn(scope, name) != nil
}

// execReindex parses and "executes" (validates, then does nothing -- see this
// file's package doc comment) a single REINDEX statement.
func (db *DB) execReindex(sqlText string) error {
	toks, err := lex(sqlText)
	if err != nil {
		return err
	}
	p := newParser(sqlText, toks)
	if !p.consumeKeyword("REINDEX") {
		return fmt.Errorf("engine: REINDEX: expected REINDEX, got %q", p.tokenDesc(p.peek()))
	}

	if p.peek().kind == tkIdent {
		first := p.next().text
		if p.peekIsPunct(".") {
			p.next()
			t := p.peek()
			if t.kind != tkIdent {
				return fmt.Errorf("engine: REINDEX: expected name after %q., got %q", first, p.tokenDesc(t))
			}
			p.next()
			second := t.text
			if err := expectDropTrailer(p, "engine: REINDEX"); err != nil {
				return err
			}
			// A schema-qualified name must resolve to an object IN THAT
			// SCHEMA -- scopeOfQualifier claims exactly main and temp, the
			// same two names isVacuumReindexSchemaName accepts for VACUUM,
			// and rejects everything else as an unknown database. The
			// resolution is scoped rather than catalog-blind: C SQLite
			// rejects "REINDEX main.<temp object>" and "REINDEX temp.<main
			// object>" (see reindexTargetExistsIn), so resolving either
			// against the whole schema would ACCEPT a statement C SQLite
			// rejects.
			scope, ok := scopeOfQualifier(first)
			if !ok {
				return fmt.Errorf("engine: unknown database %s", first)
			}
			if !db.reindexTargetExistsIn(scope, second) {
				return fmt.Errorf("engine: unable to identify the object to be reindexed")
			}
			return nil
		}
		if err := expectDropTrailer(p, "engine: REINDEX"); err != nil {
			return err
		}
		// A bare name is resolved as a collation FIRST, then as an object in
		// EITHER catalog (scopeAny) -- either way REINDEX is a no-op here, so
		// accepting on either match is exactly right; a name that is neither
		// is what C SQLite rejects. The collation fallback is bare-name-only
		// on purpose: "REINDEX NOCASE" is accepted but "REINDEX main.NOCASE"
		// is "unable to identify the object to be reindexed" (verified
		// directly), which is why the qualified branch above never consults it.
		if isReindexCollationName(first) || db.reindexTargetExistsIn(scopeAny, first) {
			return nil
		}
		return fmt.Errorf("engine: unable to identify the object to be reindexed")
	}

	if err := expectDropTrailer(p, "engine: REINDEX"); err != nil {
		return err
	}
	// Bare "REINDEX": rebuild every index -- a no-op here.
	return nil
}

// execVacuum parses and executes a single VACUUM statement: it validates the
// form (declining VACUUM INTO, an in-transaction VACUUM, and an unknown schema
// qualifier -- see this file's package doc comment), then renumbers the rowids
// of every eligible main-schema table exactly as C SQLite's VACUUM does.
func (db *DB) execVacuum(sqlText string) error {
	// VACUUM renumbers every page in the file; the incremental commit path's
	// page numbers (from the ORIGINAL file) would be meaningless afterward.
	toks, err := lex(sqlText)
	if err != nil {
		return err
	}
	p := newParser(sqlText, toks)
	if !p.consumeKeyword("VACUUM") {
		return fmt.Errorf("engine: VACUUM: expected VACUUM, got %q", p.tokenDesc(p.peek()))
	}

	// Optional schema-name qualifier. "INTO" is itself a bare identifier to the
	// lexer, so it must not be mistaken for the schema name.
	var schema string
	if p.peek().kind == tkIdent && !equalFoldName(p.peek().text, "INTO") {
		schema = p.next().text
	}
	// "VACUUM [schema] INTO <expr>" writes a separate database copy -- see
	// execVacuumInto, which owns every rule that form has of its own.
	if p.peek().kind == tkIdent && equalFoldName(p.peek().text, "INTO") {
		p.next() // INTO
		return db.execVacuumInto(p, schema)
	}
	if err := expectDropTrailer(p, "engine: VACUUM"); err != nil {
		return err
	}

	// C SQLite rejects VACUUM inside an open transaction. The predicate is
	// inTransaction() rather than txActive alone because a transaction a DRIVER
	// is holding open (driver's BeginTx, and its SQL-"BEGIN" path, which
	// keeps one engine session from BEGIN to COMMIT without the engine ever
	// seeing a BEGIN statement) is just as much an open transaction to real
	// SQLite: probed through the driver, "BEGIN; VACUUM" is
	// SQLITE_ERROR on 3.53.3 and used to be ACCEPTED here -- an accept where
	// the oracle refuses, which is the one direction that is scored wrong.
	if db.inTransaction() {
		return fmt.Errorf("engine: cannot VACUUM from within a transaction")
	}
	if schema != "" && !isVacuumReindexSchemaName(schema) {
		// A schema-name that is neither main nor temp is still legal if it
		// names a live ATTACH binding: "R-36598-60500 Attached databases can
		// be vacuumed by appending the appropriate schema-name to the VACUUM
		// statement" -- verified directly (e_vacuum.test#2.1.7, vacuum5.test):
		// "VACUUM aux" shrinks aux's OWN file and bumps aux's OWN
		// schema_version by one, while main's file size and schema_version
		// are untouched. See execVacuumAttached.
		if ad := db.attachedNamed(schema); ad != nil {
			return db.execVacuumAttached(ad)
		}
		return fmt.Errorf("engine: unknown database %s", schema)
	}
	// "VACUUM temp": the temp catalog shares the main file (temp_schema.go),
	// which a bare "VACUUM"/"VACUUM main" already renumbers wholesale, so there
	// is nothing separate to do here.
	if equalFoldName(schema, "temp") {
		return nil
	}
	// VACUUM is where C applies a "PRAGMA auto_vacuum = MODE" it had silently
	// ignored, in both directions:
	//
	//	CREATE TABLE t(a); INSERT...; PRAGMA auto_vacuum=1  -> 0, page_count 2
	//	                                            VACUUM  -> 1, page_count 3
	//	PRAGMA auto_vacuum=2; CREATE TABLE t(a); ...=0      -> 2
	//	                                            VACUUM  -> 0, page_count 2
	//
	// See DB.autoVacuumPendingPlus1.
	if db.autoVacuumPendingPlus1 != 0 {
		db.autoVacuum = db.autoVacuumPendingPlus1 - 1
		db.autoVacuumPendingPlus1 = 0
	}
	// On this format VACUUM is the file rewrite: C re-lays the database from
	// its logical content (vacuum.c:209-212), and Session.rewriteFile does
	// the same -- one segment per table, no delta. The statement records the
	// request and the next commit performs it (in autocommit, its own), which
	// keeps a rename from happening under a live read.
	//
	// Rowids are renumbered first (vacuumRenumberTable), since a rewrite
	// keeps rows under their existing keys: "DELETE FROM t4 WHERE x='y';
	// VACUUM; SELECT rowid, x FROM t4" must answer rowid 2 (e_vacuum.test#1).
	if rerr := db.vacuumRenumberMainSchema(); rerr != nil {
		return rerr
	}
	db.vacuumRenumberSchemaRowids()
	// ...and a VACUUM is where a "PRAGMA page_size=N" on an existing
	// database takes effect (vacuum.c:209-212, db->nextPagesize); the page
	// size is a catalog field the rewrite writes.
	db.pageSize = db.vacuumIntoPageSize()
	db.segmentVacuumPending = true
	// The schema cookie moves by one (vacuum.c:359, aCopy's
	// "BTREE_SCHEMA_VERSION, 1"); the page path's build writes it itself.
	db.bumpSchema(false)
	return nil
}

func (db *DB) execVacuumAttached(ad *attachedDB) error {
	// forWrite=true: a "VACUUM <attached>" always genuinely compacts the
	// attachment's own file -- see attachedWriteSessionFor's doc comment.
	w, err := db.attachedWriteSessionFor(ad.name, true)
	if err != nil {
		return err
	}
	// "VACUUM <attached>" always genuinely compacts and renumbers that
	// database's own file -- see attachedDB.wroteData's own doc comment.
	ad.wroteData = true
	if w.autoVacuumPendingPlus1 != 0 {
		w.autoVacuum = w.autoVacuumPendingPlus1 - 1
		w.autoVacuumPendingPlus1 = 0
	}
	// What execVacuum does for main: renumber, then let the Close below perform
	// the rewrite.
	if err := w.vacuumRenumberMainSchema(); err != nil {
		return err
	}
	w.vacuumRenumberSchemaRowids()
	w.segmentVacuumPending = true
	w.schemaCookie++ // vacuum.c:359, once -- C moves it by one, not two
	w.schemaGen++

	// Committed through the Session that owns w, like every attached write
	// (closeWrite): w.Close alone is the SQLite-format commit, which on a
	// segment session takes the lock ladder on a file it never opened.
	if cerr := ad.closeWrite(); cerr != nil {
		return fmt.Errorf("engine: VACUUM %s: %w", ad.name, cerr)
	}
	// The attachment's READ pager is a snapshot from before this write;
	// reopen it from the just-flushed file, the same way
	// rollbackTxnAttachedWrites does after discarding a session.
	rp, oerr := openAttachedRead(ad.path)
	if oerr != nil {
		return fmt.Errorf("engine: VACUUM %s: reopening after vacuum: %w", ad.name, oerr)
	}
	rp.SetLocalSchema(ad.name)
	if ad.pager != nil {
		ad.pager.Close()
	}
	ad.pager = rp
	db.refreshAttachedReaders()
	return nil
}

// execVacuumInto runs "VACUUM [schema] INTO '<file>'": it writes a compacted
// copy of this database to <file> and leaves the source alone. The parser is
// positioned just past INTO.
//
// VACUUM INTO does not renumber table rowids:
//
//	CREATE TABLE r(a); INSERT INTO r VALUES('x'),('y'),('z'),('w')
//	DELETE FROM r WHERE a IN ('y','z')      -> rowids 1,4
//	VACUUM INTO 'copy.db'; SELECT rowid FROM copy.r  -> 1,4   (PRESERVED)
//	VACUUM;                SELECT rowid FROM r       -> 1,2   (RENUMBERED)
//
// The catalog's rowids are renumbered, since C rebuilds the copy by re-running
// each CREATE (vacuumIntoContents). The copy is what a file rewrite writes
// (segmentFileContents), sent to the target. Its schema cookie is the
// source's plus one (vacuum.c aCopy[]) and the source's does not move.
//
// Errors, in C's order (the qualifier resolves at prepare time, then the
// transaction check, then the target):
//
//	VACUUM nosuch INTO ...        "unknown database nosuch"
//	BEGIN; VACUUM INTO <anything> "cannot VACUUM from within a transaction"
//	                              (including a target that is NULL, or a file
//	                              that already exists)
//	target is a non-empty database   "output file already exists"
//	target is a non-empty non-database "file is not a database"
//	target is a ZERO-LENGTH file  accepted, and overwritten
//	target is this database        "output file already exists" (it exists)
//	target is NULL or a number    "non-text filename"
//	a relative path               resolved against the process directory
//
// ':memory:' and '' are a real no-op: VACUUM INTO commits into a private
// btree (vacuum.c:226, 382) that is never copied back (the copy is gated on
// pOut==0, vacuum.c:378-379) and is closed at end_of_vacuum (vacuum.c:417).
// db->flags, nChange and nTotalChange are saved and restored (vacuum.c:193,
// 195, 401-403) and lastRowid is untouched, so nothing is observable.
//
// Declined:
//
//   - "VACUUM temp INTO 'f'": C reports success and writes no file.
//   - a deferred "PRAGMA auto_vacuum = MODE": C applies it to the copy only;
//     this engine applies a pending mode at VACUUM only. (A pending page_size
//     is honoured; see notePageSizeRequest.)
func (db *DB) execVacuumInto(p *parser, schema string) error {
	target, terr := p.parseExpr()
	if terr != nil {
		return terr
	}
	if err := expectDropTrailer(p, "engine: VACUUM INTO"); err != nil {
		return err
	}
	// Prepare-time in C SQLite, so it wins over everything below.
	if schema != "" && !isVacuumReindexSchemaName(schema) {
		return fmt.Errorf("engine: unknown database %s", schema)
	}
	if db.inTransaction() { // see execVacuum for why it is not txActive alone
		return fmt.Errorf("engine: cannot VACUUM from within a transaction")
	}
	if equalFoldName(schema, "temp") {
		return fmt.Errorf("%w: VACUUM temp INTO (C SQLite reports success and writes no file at all, even with a populated temp table present)", errVDBEUnsupported)
	}
	// Resolve the target before evaluating it, under the zeroed NameContext
	// sqlite3Vacuum gives it (vacuum.c:128, "    if( pInto &&
	// sqlite3ResolveSelfReference(pParse,0,0,pInto,0)==0 ){"), so an aggregate
	// or window call is "misuse of ... function": "VACUUM INTO min('mn.db')"
	// must not write mn.db. See requireZeroedNameContext.
	//
	// Below the temp check, as in C (the resolve is inside "if( iDb!=1 )",
	// vacuum.c:126). One ordering difference stands: C's transaction check
	// is a runtime one in OP_Vacuum, so "BEGIN; VACUUM INTO min('x.db')" is
	// the misuse error there and the transaction error here. Both reject.
	if err := requireZeroedNameContext(target); err != nil {
		return err
	}
	// vacuum.c:177-183: the target is evaluated and must have type
	// SQLITE_TEXT -- a strict type check, so a number is refused -- after the
	// transaction and schema checks. pager is needed because the target may
	// be a scalar subquery ("VACUUM INTO (SELECT name FROM t2)"). It is
	// compiled (selectExprValue), as C codes it into a register for
	// OP_Vacuum (vacuum.c:130, 132).
	pager, perr := db.SnapshotPager()
	if perr != nil {
		return perr
	}
	tv, everr := pager.selectExprValue(target)
	if everr != nil {
		return everr
	}
	if tv.Typ != Text {
		return fmt.Errorf("engine: non-text filename")
	}
	path := string(tv.S)
	if path == "" || path == ":memory:" {
		// A private, discarded copy (see the function comment), so the other
		// target rules do not apply. Case-sensitive on purpose: btree.c:2556
		// recognizes ":memory:" with a plain strcmp, so ":MEMORY:" is an
		// ordinary file name and C writes a real file by that name.
		return nil
	}
	if err := vacuumIntoTargetUsable(path); err != nil {
		return err
	}

	// ON THE SEGMENT FORMAT the copy is a segment file, written from the same
	// contents a rewrite writes (segmentFileContents) -- which is what VACUUM
	// INTO means: this database's logical content, re-laid, somewhere else.
	w, werr := newSegFileWriter(filepath.Dir(path))
	if werr != nil {
		return werr
	}
	defer w.discard()
	tables, cat, berr := db.vacuumIntoContents(w)
	if berr != nil {
		return berr
	}
	// ...but the copy's header fields are the COPY's, not this database's. A
	// deferred page_size and auto_vacuum request sizes it (vacuum.c:281-282,
	// :290 -- the WAL guard at :275 applies only when pOut==0), and it is a
	// fresh file in the default rollback mode: vacuum.c never carries the
	// journal mode across, so C's copy of a wal database answers "delete".
	cat.PageSize = db.vacuumIntoPageSize()
	cat.AutoVacuumPlus1 = uint32(db.vacuumIntoAutoVacuum()) + 1
	cat.JournalWAL = 0
	// "Add one to the old schema cookie" (vacuum.c:359), so a connection that
	// knew the source re-reads the copy's schema.
	cat.SchemaVersion++
	// Counter 1, pages 1 for the same reason CreateFile uses them: a state, not
	// a page count (segment_ddl.go). The copy is a database of its own, so it
	// starts at the beginning of its own history rather than inheriting this
	// one's.
	if werr := w.finish(path, tables, cat, 1, 1); werr != nil {
		return werr
	}
	return syncSegFile(path)
}

// vacuumIntoContents is the copy's contents: this database's, with the catalog
// renumbered the way VACUUM renumbers it (vacuumRenumberSchemaRowids), because
// C builds the copy by the same rebuild (vacuum.c:297-337) -- after a non-tail
// DROP, "SELECT rowid FROM sqlite_master" on C's copy starts again from 1. This
// database's own numbering is put back: VACUUM INTO leaves the source alone.
func (db *DB) vacuumIntoContents(w *segFileWriter) ([]ConvertedTable, ConvertedCatalog, error) {
	var seqs []*uint64
	for _, t := range db.tables {
		seqs = append(seqs, &t.schemaSeq)
	}
	for _, ix := range db.indexes {
		seqs = append(seqs, &ix.schemaSeq)
	}
	for _, v := range db.views {
		seqs = append(seqs, &v.schemaSeq)
	}
	for _, g := range db.triggers {
		seqs = append(seqs, &g.schemaSeq)
	}
	for _, vt := range db.vtabs {
		seqs = append(seqs, &vt.schemaSeq)
	}
	old := make([]uint64, len(seqs))
	for i, p := range seqs {
		old[i] = *p
	}
	tables := slices.Clone(db.tables) // the renumbering may drop sqlite_sequence
	defer func() {
		db.tables = tables
		for i, p := range seqs {
			*p = old[i]
		}
	}()
	db.vacuumRenumberSchemaRowids()
	return db.segmentFileContents(w)
}

// vacuumIntoAutoVacuum is the copy's auto_vacuum mode: a deferred
// "PRAGMA auto_vacuum = MODE" request applies to the COPY and leaves this
// database on its old one, exactly as vacuum.c:290 reads it
// ("db->nextAutovac>=0 ? db->nextAutovac : sqlite3BtreeGetAutoVacuum(pMain)").
func (db *DB) vacuumIntoAutoVacuum() int {
	if db.autoVacuumPendingPlus1 != 0 {
		return db.autoVacuumPendingPlus1 - 1
	}
	return db.autoVacuum
}

// vacuumIntoTargetUsable applies C SQLite's two target-file refusals: a
// target that already holds a database is "output file already exists", and
// one that holds anything else is "file is not a database" (vacuum.c's
// sqlite3RunVacuum through sqlite3BtreeOpen). A file of length zero is neither
// -- SQLite treats it as an empty database and overwrites it (probed: a 0-byte
// file is accepted and comes back full-size, while a 5-byte one is
// SQLITE_NOTADB). A database here is one of this engine's.
func vacuumIntoTargetUsable(path string) error {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return nil // absent, unreadable, or empty: let the write itself report
	}
	f, oerr := os.Open(path)
	if oerr != nil {
		return nil
	}
	head := make([]byte, len(segFileMagic))
	n, _ := io.ReadFull(f, head)
	f.Close()
	if n == len(head) && string(head) == segFileMagic {
		return fmt.Errorf("engine: output file already exists")
	}
	return fmt.Errorf("engine: file is not a database")
}


// tableHasAnyIndex reports whether any index (of any kind: a UNIQUE/PRIMARY KEY
// auto-index, an explicit CREATE INDEX, or an expression/partial index) is registered against the named table. Its presence is exactly what
// makes C SQLite's VACUUM preserve, rather than renumber, that table's
// rowids (see vacuumRenumberTable).
func (db *DB) tableHasAnyIndex(tableName string) bool {
	for _, ix := range db.indexes {
		if equalFoldName(ix.table, tableName) {
			return true
		}
	}
	return false
}

// vacuumRenumberMainSchema applies VACUUM's rowid renumbering to every eligible
// main-schema table.
func (db *DB) vacuumRenumberMainSchema() error {
	for _, t := range db.tables {
		if err := db.vacuumRenumberTable(t); err != nil {
			return err
		}
	}
	return nil
}

// vacuumRenumberTable reassigns t's rowids to a contiguous 1..N in ascending
// (signed) old-rowid order -- but only for a table C SQLite's VACUUM would
// renumber: an ordinary rowid table (not WITHOUT ROWID) with no INTEGER PRIMARY
// KEY alias and no index of any kind. For every other table (WITHOUT ROWID, an
// IPK/rowid-alias table, or any table carrying an index) C SQLite preserves
// the existing rowids, so this does nothing. Because renumbering only ever
// happens when the table has NO index, reassigning the live row store's map
// keys is the entire operation -- there is no index b-tree to rebuild, and the
// next auto-assigned rowid (nextRowidForTable, one past the new maximum N)
// follows automatically.
func (db *DB) vacuumRenumberTable(t *tableMeta) error {
	if t.withoutRowid || t.ipkIndex >= 0 || db.tableHasAnyIndex(t.name) {
		return nil
	}
	// See table_load.go's package doc comment: VACUUM renumbers every
	// eligible table's rowids, so it inherently needs every table's rows
	// loaded, the same way ANALYZE does -- this is not a laziness win for
	// VACUUM specifically, just correctness for a Stage-0 session.
	if err := db.ensureTableLoaded(t); err != nil {
		return err
	}
	if t.rows.len() == 0 {
		return nil
	}
	rowids := make([]uint64, 0, t.rows.len())
	for rid := range t.rows.all() {
		rowids = append(rowids, rid)
	}
	// Signed order: a rowid is a SQLite INTEGER stored bit-for-bit in a uint64
	// (see btree_write.go's rowidLess), so a negative explicit rowid sorts
	// before any positive one -- matching C SQLite's ascending-rowid copy
	// order (verified: rowids -100,-5,3,100 renumber to 1,2,3,4 in that order).
	sort.Slice(rowids, func(i, j int) bool { return rowidLess(rowids[i], rowids[j]) })
	newRows := make(map[uint64][]Value, t.rows.len())
	for i, rid := range rowids {
		newRows[uint64(i+1)] = t.rows.row(rid)
	}
	// A new store: neither cache of the old one (its maximum, its order)
	// describes the renumbered key set. replaceRows, so the rewrite at commit
	// keeps it rather than reloading the table from the file (segment_ddl.go).
	t.replaceRows(newRowStore(newRows, nil))
	return nil
}

// IsVacuumStatement reports whether sqlText is a VACUUM, in either form (bare,
// schema-qualified, or VACUUM INTO).
//
// Exported for the driver, which declines the whole family on this format:
// VACUUM rebuilds a SQLite-format file page by page, and the counterpart on a
// format with no pages is COMPACTION (segment_compact.go). Lexing rather than
// matching a prefix, for the reason every other statement-kind helper here does:
// "VACUUMED" is an identifier, not a VACUUM.
func IsVacuumStatement(sqlText string) bool {
	toks, err := lex(sqlText)
	if err != nil {
		return false
	}
	p := newParser(sqlText, toks)
	return p.consumeKeyword("VACUUM")
}

// vacuumRenumberSchemaRowids gives MAIN's catalog the rowids C's VACUUM leaves
// it with. C rebuilds the schema into a fresh database and copies that back, so
// every row is renumbered in the order vacuum.c recreates them:
//
//   - each ordinary table (not sqlite_sequence, not a virtual table), in rowid
//     order, by re-running its CREATE -- which writes the table's row, then its
//     automatic indexes', and, for the first AUTOINCREMENT table, sqlite_sequence
//     (vacuum.c:298-303);
//   - each explicit index, in rowid order (vacuum.c:305-309; an automatic
//     index's NULL sql is skipped by execSql's "CRE"/"INS" filter, :47-48);
//   - every view, trigger and virtual table, appended in rowid order by
//     "INSERT INTO vacuum_db.sqlite_schema SELECT*FROM ..." (vacuum.c:333-338).
func (db *DB) vacuumRenumberSchemaRowids() {
	byOld := func(a, b uint64) int { return cmp.Compare(a, b) }
	var tables []*tableMeta
	var seqTable *tableMeta
	for _, t := range db.tables {
		switch {
		case t.isTemp:
		case equalFoldName(t.name, sqliteSequenceTableName):
			seqTable = t
		default:
			tables = append(tables, t)
		}
	}
	slices.SortStableFunc(tables, func(a, b *tableMeta) int { return byOld(a.schemaSeq, b.schemaSeq) })
	var explicit []*indexMeta
	autoOf := map[string][]*indexMeta{}
	for _, ix := range db.indexes {
		if ix.isTemp {
			continue
		}
		if ix.sql != "" {
			explicit = append(explicit, ix)
		} else {
			autoOf[r33sFoldIdent(ix.table)] = append(autoOf[r33sFoldIdent(ix.table)], ix)
		}
	}
	slices.SortStableFunc(explicit, func(a, b *indexMeta) int { return byOld(a.schemaSeq, b.schemaSeq) })
	type other struct {
		old uint64
		set func(uint64)
	}
	var others []other
	for _, v := range db.views {
		if !v.isTemp {
			others = append(others, other{v.schemaSeq, func(n uint64) { v.schemaSeq = n }})
		}
	}
	for _, g := range db.triggers {
		if !g.isTemp {
			others = append(others, other{g.schemaSeq, func(n uint64) { g.schemaSeq = n }})
		}
	}
	for _, vt := range db.vtabs {
		if !vt.isTemp {
			others = append(others, other{vt.schemaSeq, func(n uint64) { vt.schemaSeq = n }})
		}
	}
	slices.SortStableFunc(others, func(a, b other) int { return byOld(a.old, b.old) })

	var n uint64
	seqPlaced := false
	for _, t := range tables {
		n++
		t.schemaSeq = n
		autos := autoOf[r33sFoldIdent(t.name)]
		slices.SortStableFunc(autos, func(a, b *indexMeta) int { return byOld(a.schemaSeq, b.schemaSeq) })
		for _, ix := range autos {
			if ix.isTablePK {
				ix.schemaSeq = t.schemaSeq // a WITHOUT ROWID PRIMARY KEY writes no row
				continue
			}
			n++
			ix.schemaSeq = n
		}
		if t.autoIncrement && seqTable != nil && !seqPlaced {
			n++
			seqTable.schemaSeq = n
			seqPlaced = true
		}
	}
	for _, ix := range explicit {
		n++
		ix.schemaSeq = n
	}
	for _, o := range others {
		n++
		o.set(n)
	}
	if seqTable != nil && !seqPlaced {
		// No AUTOINCREMENT table re-creates it, and step 1 skips it by name, so
		// C's rebuilt database has NO sqlite_sequence: its counters for dropped
		// tables go with it (verified: "CREATE TABLE a(... AUTOINCREMENT); DROP
		// TABLE a; CREATE TABLE b(x); VACUUM" leaves b alone in sqlite_master).
		db.tables = slices.DeleteFunc(db.tables, func(t *tableMeta) bool { return t == seqTable })
	}
}
