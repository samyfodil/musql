// Explicit transaction control: BEGIN [DEFERRED|IMMEDIATE|EXCLUSIVE]
// [TRANSACTION], COMMIT|END [TRANSACTION], ROLLBACK [TRANSACTION], and SAVEPOINT
// name / RELEASE [SAVEPOINT] name / ROLLBACK [TRANSACTION] TO [SAVEPOINT] name.
//
// These compile like any statement: compileWriteProgram (vdbe_write.go) parses
// the verb and name and emits one OpTxn carrying both, as C parses them at
// prepare time (sqlite3BeginTransaction, build.c:5245; sqlite3EndTransaction,
// build.c:5281; sqlite3Savepoint, build.c:5303, each emitting one
// OP_AutoCommit or OP_Savepoint). opTxn then calls beginTxn/commitTxn/
// rollbackTxn/openSavepoint/releaseSavepoint/rollbackToSavepoint below.
//
// A session stays open across a ROLLBACK (the driver's held session, the
// corpus harness's session per segment), so ROLLBACK undoes in place. BEGIN
// captures what a write can mutate (captureSharedSnapshot): schema objects
// (tables with their columns, indexes, views, triggers, vtabs) and the
// schemaCookie/userVersion counters are cloned, while each loaded table's row
// store is shared and its undo log started, the snapshot keeping the offset to
// roll back to (row_store_undo.go). restoreSnapshot installs the clones and
// rolls each store back. A full copy (captureSnapshot) copies each table's row
// map and shares the rows, since stored rows are never edited in place
// (rowStore.clone).
//
// A savepoint is the same machinery one level down: db.savepoints is a stack of
// {name, snapshot}, each captured when its SAVEPOINT ran. Rules, pinned by
// compat-harness/vdbe_savepoint_test.go:
//
//   - SAVEPOINT outside a transaction starts one; COMMIT commits it, ROLLBACK
//     undoes it, a BEGIN inside it is the nested-transaction error
//     (savepoint.startedTxn).
//   - RELEASE name pops it and every later savepoint, keeping their changes
//     ("SAVEPOINT a; SAVEPOINT b; RELEASE a" makes b "no such savepoint");
//     releasing the one that started the transaction commits it, never inside
//     an enclosing BEGIN.
//   - ROLLBACK TO name restores its snapshot without popping it (repeatable)
//     and discards later savepoints; the transaction stays open.
//   - A repeated name shadows: ROLLBACK TO / RELEASE reach the innermost.
//   - An unknown name, even with no transaction open, is "no such savepoint:
//     <name>" and changes nothing.
//   - DDL rolls back too (the snapshot is the whole logical state, temp
//     included).
//   - Under "PRAGMA journal_mode=off" ROLLBACK TO restores nothing (C has no
//     sub-journal), though the stack bookkeeping still applies; a full
//     ROLLBACK still undoes everything (rollbackToSavepoint).
//
// ROLLBACK TO restores a fresh cloneSnapshot of the entry, because
// restoreSnapshot installs the snapshot's maps directly, and a later write
// would otherwise corrupt the saved state for the next ROLLBACK TO.
//
// A SAVEPOINT costs a clone of the schema objects and an undo-log mark per
// loaded table, not a copy of the rows. BEGIN's access mode is the session's
// business (Session.BeginWriteTxn locks for IMMEDIATE/EXCLUSIVE).
package engine

import (
	"errors"
	"fmt"
	"strings"
)

// txSnapshot is the deep copy of engine.DB's mutable logical state captured
// at BEGIN and restored at ROLLBACK. See this file's package doc comment for
// why every field here must be an independent copy, not a shared reference.
type txSnapshot struct {
	// marks, when set, says this snapshot SHARES its tables' row stores with the
	// live database instead of copying them, and holds each store's undo-log
	// offset to roll back to (captureSharedSnapshot, row_store_undo.go).
	marks map[*rowStore]int

	tables       []*tableMeta
	indexes      []*indexMeta
	views        []*viewMeta
	vtabs        []*vtabMeta
	triggers     []*triggerMeta
	schemaCookie uint32
	userVersion  uint32
	// applicationID is saved for the same reason userVersion is: C SQLite
	// writes it through an ordinary write transaction, so a ROLLBACK puts it
	// back -- verified directly against mattn/go-sqlite3 3.53.3, "BEGIN;
	// PRAGMA application_id=7; ROLLBACK" reads back the pre-BEGIN value.
	applicationID uint32
	captureGuard  string // a guard set in a rolled-back transaction is undone with it
	changeLogLen  int    // row-change log length at BEGIN (rowhook.go); a ROLLBACK truncates back to it
	// fkDeferred is the outstanding DEFERRED foreign key violation count
	// (DB.fkDeferred). It is transaction state, so a ROLLBACK TO must put it
	// back exactly as a rolled-back row would be: verified directly against
	// mattn/go-sqlite3 -- "BEGIN; INSERT an orphan; SAVEPOINT s; INSERT a
	// second orphan; ROLLBACK TO s; COMMIT" still fails (the FIRST orphan
	// stands), while rolling back to a savepoint taken BEFORE the only orphan
	// makes the COMMIT succeed. C SQLite saves it in the Savepoint struct
	// for the same reason.
	fkDeferred int
	// wsEdits / wsInserted are the direct-sqlite_master write overlay
	// (schema_write_direct.go). A catalog write is ordinary transactional DML
	// in C SQLite -- verified directly against mattn/go-sqlite3 3.53.3 that
	// both ROLLBACK and ROLLBACK TO a savepoint undo one -- so it is saved and
	// restored exactly like a row.
	wsEdits    map[wsCatalogKey]*wsCatalogEdit
	wsInserted int
	// fkDeferredImm is "PRAGMA defer_foreign_keys"' own counter
	// (nDeferredImmCons). C SQLite's Savepoint struct saves BOTH deferred
	// counters and OP_Savepoint's rollback branch restores both, so this rides
	// here for exactly the reason fkDeferred does. The FLAG itself does not:
	// verified directly that a SAVEPOINT, a RELEASE of an inner savepoint and a
	// ROLLBACK TO all leave "PRAGMA defer_foreign_keys" reading 1.
	fkDeferredImm int
}

// savepoint is one entry of db.savepoints: the name a SAVEPOINT statement
// gave (as written, dequoted -- compared case-insensitively, reported
// verbatim in "no such savepoint: %s") and the snapshot taken when it ran.
// startedTxn marks the entry whose SAVEPOINT started the transaction, i.e.
// the one whose RELEASE must also COMMIT it -- see this file's package doc
// comment.
type savepoint struct {
	name       string
	snap       *txSnapshot
	startedTxn bool
}

// cloneValues returns an independent copy of vs, including a fresh backing
// array for any TEXT/BLOB Value's S bytes.
func cloneValues(vs []Value) []Value {
	if vs == nil {
		return nil
	}
	out := make([]Value, len(vs))
	copy(out, vs)
	for i, v := range vs {
		if v.S != nil {
			out[i].S = append([]byte(nil), v.S...)
		}
	}
	return out
}

// cloneTableMeta deep-copies t: every field carried over, then a fresh cols
// slice and a fresh rows map over the same row slices (rowStore.clone), plus
// fresh copies of the per-session rowid logs.
//
// Copying the WHOLE struct (rather than naming the fields to keep) is what
// makes this safe as tableMeta grows: an earlier version listed five fields by
// hand, so a ROLLBACK silently reset everything else the table knew about
// itself -- its catalog (isTemp), its CHECK constraints, withoutRowid/pkIndex,
// and the AUTOINCREMENT sequence. That surfaced as an INSERT and a SELECT
// disagreeing about which of a temp/main same-named pair they meant, once a
// rolled-back statement had cleared the temp one's isTemp (fkey2.test).
func cloneTableMeta(t *tableMeta) *tableMeta { return cloneTableMetaRows(t, false) }

// cloneTableMetaRows is cloneTableMeta that, with shareRows, keeps t's own row
// store rather than copying it -- for a snapshot that rolls it back through its
// undo log instead (captureSharedSnapshot).
func cloneTableMetaRows(t *tableMeta, shareRows bool) *tableMeta {
	cp := *t
	cp.cols = append([]columnInfo(nil), t.cols...)
	if shareRows {
		cp.rows = t.rows
	} else {
		cp.rows = t.rows.clone(&cp)
	}
	cp.checks = append([]checkConstraint(nil), t.checks...)
	// The copies above carry t's compiled schema-time programs, but those
	// address columns by position in the list they were compiled against and
	// eval will only run one against THAT slice (selfRowExpr.cols,
	// vdbe_codegen.go) -- so on the fresh cols/checks slices here every single
	// evaluation would pay a fresh compile (selfRowExpr.restamp, vdbe_run.go)
	// for as long as this snapshot stayed live. Re-stamping ONCE here is per
	// table, not per row, and it keeps the clone as compiled as the table it
	// came from.
	refreshRowPrograms(&cp)
	// SHARED, not copied: the log is append-only, and every holder writes only at
	// or past its own length. A snapshot sees exactly the prefix it was taken
	// with; the one holder that ever wrote past it is the live table a rollback
	// discards. Copying it made every BEGIN O(rows inserted this session) --
	// 3MB a transaction on a 400,000-row load.
	return &cp
}

// cloneIndexMeta deep-copies ix's own slices; indexMeta otherwise holds only
// plain value fields (name/table/unique/sql).
func cloneIndexMeta(ix *indexMeta) *indexMeta {
	cp := *ix
	cp.cols = append([]string(nil), ix.cols...)
	cp.colIdx = append([]int(nil), ix.colIdx...)
	return &cp
}

// cloneViewMeta deep-copies v's own colNames slice; viewMeta's selectStmt is
// a parsed AST that CreateView never mutates after registration, so sharing
// it between the live viewMeta and its snapshot copy is safe.
func cloneViewMeta(v *viewMeta) *viewMeta {
	cp := *v
	cp.colNames = append([]string(nil), v.colNames...)
	return &cp
}

// cloneVtabMeta deep-copies vt's own args slice; every other field is an
// immutable string set once at registration (mirrors cloneViewMeta).
func cloneVtabMeta(vt *vtabMeta) *vtabMeta {
	cp := *vt
	cp.args = append([]string(nil), vt.args...)
	// A writable vtab's backing store is mutable logical state, so it must be
	// deep-copied for the transaction snapshot ROLLBACK restores wholesale (a
	// read-only module leaves store nil).
	if vt.store != nil {
		cp.store = vt.store.vtabClone()
	}
	return &cp
}

// cloneTriggerMeta deep-copies tr's own updateCols slice; triggerMeta's
// when/body are parsed ASTs that CreateTrigger never mutates after
// registration, so sharing them between the live triggerMeta and its
// snapshot copy is safe (mirrors cloneViewMeta's identical reasoning for
// viewMeta.selectStmt).
func cloneTriggerMeta(tr *triggerMeta) *triggerMeta {
	cp := *tr
	cp.updateCols = append([]string(nil), tr.updateCols...)
	return &cp
}

// captureSnapshot deep-copies db's mutable logical state (see the file doc).
// Every table is loaded first, since a later ROLLBACK restores db.tables
// wholesale and a snapshot missing a table's content would be wrong. A load
// failure is recorded in db.pendingLoadErr, which fails the statement, and this
// returns nil (several callers have no error path).
func (db *DB) captureSnapshot() *txSnapshot {
	for _, t := range db.tables {
		if err := db.ensureTableLoaded(t); err != nil {
			if db.pendingLoadErr == nil {
				db.pendingLoadErr = err
			}
			return nil
		}
	}
	return db.captureSnapshotWith(cloneTableMeta)
}

// captureSnapshotWith is captureSnapshot with the per-table clone supplied, so
// captureSharedSnapshot can share row stores through the same code.
func (db *DB) captureSnapshotWith(cloneTable func(*tableMeta) *tableMeta) *txSnapshot {
	snap := &txSnapshot{
		schemaCookie:  db.schemaCookie,
		userVersion:   db.userVersion,
		applicationID: db.applicationID,
		captureGuard:  db.captureGuard,
		changeLogLen:  db.changeMark(),
		fkDeferred:    db.fkDeferred,
		wsEdits:       cloneWritableSchemaEdits(db.wsEdits),
		wsInserted:    db.wsInserted,
		fkDeferredImm: db.fkDeferredImm,
	}
	snap.tables = make([]*tableMeta, len(db.tables))
	for i, t := range db.tables {
		snap.tables[i] = cloneTable(t)
	}
	snap.indexes = make([]*indexMeta, len(db.indexes))
	for i, ix := range db.indexes {
		snap.indexes[i] = cloneIndexMeta(ix)
	}
	snap.views = make([]*viewMeta, len(db.views))
	for i, v := range db.views {
		snap.views[i] = cloneViewMeta(v)
	}
	snap.vtabs = make([]*vtabMeta, len(db.vtabs))
	for i, v := range db.vtabs {
		snap.vtabs[i] = cloneVtabMeta(v)
	}
	snap.triggers = make([]*triggerMeta, len(db.triggers))
	for i, tr := range db.triggers {
		snap.triggers[i] = cloneTriggerMeta(tr)
	}
	return snap
}

// captureSharedSnapshot is captureSnapshot for the snapshots a TRANSACTION holds
// -- BEGIN's and each SAVEPOINT's. Instead of copying every table's rows, each
// table's row store is SHARED with the live database and its undo log started,
// and the snapshot keeps the log offset to roll back to (row_store_undo.go).
// Everything else is cloned exactly as captureSnapshot clones it.
//
// A table NOT YET LOADED is left that way, and its clone is unloaded too: this
// session has not written it, so there is nothing to undo, and a rollback that
// restores it unloaded has it read its committed rows again on first touch --
// which are its rows as of this snapshot (segSource.unloaded says why).
func (db *DB) captureSharedSnapshot() *txSnapshot {
	marks := make(map[*rowStore]int, len(db.tables))
	for _, t := range db.tables {
		if m := t.rows.startUndo(); m >= 0 {
			marks[t.rows] = m
		}
	}
	// captureSnapshot would copy the rows; the shared tables replace its copies.
	// It is called with the logs already on, so the copies it makes of any
	// page-backed store are the only ones it keeps.
	snap := db.captureSnapshotWith(func(t *tableMeta) *tableMeta { return cloneTableMetaShared(t, marks) })
	if snap != nil {
		snap.marks = marks
	}
	return snap
}

// cloneTableMetaShared is cloneTableMeta keeping t's own row store when marks
// has an undo offset for it -- the table the snapshot shares -- and copying it
// otherwise, as cloneTableMeta always did.
func cloneTableMetaShared(t *tableMeta, marks map[*rowStore]int) *tableMeta {
	_, shared := marks[t.rows]
	return cloneTableMetaRows(t, shared && t.rows != nil)
}

// cloneSnapshot returns an independent deep copy of snap, built with the same
// per-object cloners captureSnapshot uses. ROLLBACK TO needs it because
// restoreSnapshot hands the snapshot's OWN maps and slices to the *DB rather
// than copying them (see this file's package doc comment): a savepoint that
// can be rolled back to more than once has to keep its copy pristine.
func cloneSnapshot(snap *txSnapshot) *txSnapshot {
	if snap == nil {
		// See restoreSnapshot's identical nil guard: a savepoint captured
		// while ensureTableLoaded failed (captureSnapshot returns nil rather
		// than a snapshot silently missing some table's content) has nothing
		// real to clone. Composes safely with restoreSnapshot's own nil
		// no-op, and db.pendingLoadErr is what actually reports the failure.
		return nil
	}
	cp := *snap // the scalar counters and changeLogLen carry over as-is
	cp.wsEdits = cloneWritableSchemaEdits(snap.wsEdits)
	cp.tables = make([]*tableMeta, len(snap.tables))
	for i, t := range snap.tables {
		cp.tables[i] = cloneTableMetaShared(t, snap.marks) // a shared store stays shared
	}
	cp.indexes = make([]*indexMeta, len(snap.indexes))
	for i, ix := range snap.indexes {
		cp.indexes[i] = cloneIndexMeta(ix)
	}
	cp.views = make([]*viewMeta, len(snap.views))
	for i, v := range snap.views {
		cp.views[i] = cloneViewMeta(v)
	}
	cp.vtabs = make([]*vtabMeta, len(snap.vtabs))
	for i, v := range snap.vtabs {
		cp.vtabs[i] = cloneVtabMeta(v)
	}
	cp.triggers = make([]*triggerMeta, len(snap.triggers))
	for i, tr := range snap.triggers {
		cp.triggers[i] = cloneTriggerMeta(tr)
	}
	return &cp
}

// restoreSnapshot swaps db's mutable logical state back to snap wholesale --
// ROLLBACK's implementation.
func (db *DB) restoreSnapshot(snap *txSnapshot) {
	if snap == nil {
		// captureSnapshot (this file) returns nil, instead of a snapshot that
		// is silently missing some table's true content, when
		// ensureTableLoaded (table_load.go) failed to load one -- see its own
		// doc comment for why (db.pendingLoadErr already carries that failure
		// to a proper error return). There is nothing correct to restore FROM
		// here; the "Never panic" invariant (AGENTS.md) still applies even to
		// this astronomically rare failure mode, so this is a no-op rather
		// than a nil-pointer dereference on snap.tables below.
		return
	}
	// The fts3/fts4 pending terms describe %_segdir/%_segments rows that are
	// about to be restored out from under them, so they go too -- ROLLBACK and
	// ROLLBACK TO both leave no trace of what was pending (verified: the row is
	// gone AND a MATCH for it finds nothing). See fts3_txn.go.
	db.fts3TxnDiscard()
	// An fts5 'secure-delete' bump deferred by a DELETE inside this
	// transaction never got written (that is the point of deferring), so
	// there is nothing to undo beyond forgetting it -- ROLLBACK and ROLLBACK
	// TO both discard it, exactly like the fts3 line above (fts5_txn.go).
	db.fts5TxnDiscard()
	// Every *tableMeta/*indexMeta/... pointer below is about to be replaced by
	// a clone taken at BEGIN. schemaCookie is restored to its snapshot NUMBER a
	// few lines down, so it cannot signal that -- bump txGen, which exists for
	// exactly this. Doing it here rather than at the call sites covers all of
	// them at once (rollbackTxn, conflictRollback's mid-statement unwind,
	// conflict.go's and trigger.go's unwinds). See DB.txGen.
	db.txGen++
	if db.schemaCookie != snap.schemaCookie {
		// Undoing a schema change resets C's whole schema, statistics included
		// (sqlite3ResetAllSchemasOfConnection, from sqlite3RollbackAll and
		// OP_Savepoint's ROLLBACK TO), and the next statement loads them afresh.
		db.stat1Loaded = false
	}
	db.tables = snap.tables
	db.indexes = snap.indexes
	db.views = snap.views
	db.vtabs = snap.vtabs
	db.triggers = snap.triggers
	db.schemaCookie = snap.schemaCookie
	db.userVersion = snap.userVersion
	db.applicationID = snap.applicationID
	db.captureGuard = snap.captureGuard
	db.fkDeferred = snap.fkDeferred
	db.wsEdits, db.wsInserted = snap.wsEdits, snap.wsInserted
	db.fkDeferredImm = snap.fkDeferredImm
	db.truncateChanges(snap.changeLogLen)
	// A shared snapshot's rows are the live stores themselves: put each back to
	// its offset (row_store_undo.go).
	for st, mark := range snap.marks {
		st.undoTo(mark)
	}
}

// Exported wording for the three transaction-control errors, verified
// directly against C SQLite (mattn/go-sqlite3) so beginTxn/commitTxn/
// rollbackTxn below can report the identical text C SQLite does for the
// same misuse. Exported so a caller that intercepts BEGIN/COMMIT/ROLLBACK
// before ever reaching Exec (see driver/conn.go's execTxnStmt, which
// needs the identical wording for its own, Conn-level "already have a held
// transaction *DB" / "no held transaction *DB" checks) has one shared source
// of truth rather than a second copy of these strings that could drift.
const (
	ErrMsgTxnNested        = "cannot start a transaction within a transaction"
	ErrMsgNoActiveCommit   = "cannot commit - no transaction is active"
	ErrMsgNoActiveRollback = "cannot rollback - no transaction is active"
)

// beginTxn implements BEGIN: captures the current logical state so a later
// ROLLBACK can restore it, and marks a transaction active. C SQLite
// itself rejects a nested BEGIN with exactly ErrMsgTxnNested (verified
// directly), whether the open transaction came from an earlier BEGIN or from
// a SAVEPOINT that started one -- so this rejects both, rather than silently
// replacing the still-open transaction's snapshot. Nesting is what SAVEPOINT
// is for (openSavepoint below).
func (db *DB) beginTxn() error {
	if db.txActive {
		return fmt.Errorf("engine: %s", ErrMsgTxnNested)
	}
	db.txSnapshot = db.captureSharedSnapshot()
	if db.txSnapshot == nil {
		// captureSnapshot's own doc comment: a nil result means
		// ensureTableLoaded failed for some table and the real error is in
		// db.pendingLoadErr. BEGIN must not report success (db.txActive left
		// false) while leaving this session with a txSnapshot that could
		// never actually restore anything.
		err := db.pendingLoadErr
		db.pendingLoadErr = nil
		if err == nil {
			err = fmt.Errorf("engine: BEGIN: internal error: captureSnapshot failed with no recorded cause")
		}
		return err
	}
	db.txActive = true
	db.txMayHaveDirtiedMain = false
	db.txDirtiedMain = false
	db.txSchemaChanged = false
	return nil
}

// noteTransactionMayHaveDirtiedMain sets the one bit "PRAGMA journal_mode=<mode>
// inside a transaction" consults, from ExecArgs.
//
// C's rule (sqlite3PagerOkToChangeJournalMode): the mode may change in a
// transaction until the pager first modifies a page of main
// (PAGER_WRITER_CACHEMOD), after which the pragma is silently ignored:
//
//	BEGIN                              takes  (also BEGIN IMMEDIATE/EXCLUSIVE:
//	                                          a write LOCK is not a dirty page)
//	BEGIN; SELECT ...                  takes
//	BEGIN; UPDATE t SET a=a WHERE 0    takes  (a DML statement that matches no
//	BEGIN; DELETE FROM t WHERE 0       takes   row dirties nothing)
//	BEGIN; CREATE TEMP TABLE tt(z)     takes  (the TEMP pager is a different
//	BEGIN; INSERT INTO tt VALUES(1)    takes   pager)
//	BEGIN; INSERT INTO u VALUES(9)     IGNORED
//	BEGIN; PRAGMA user_version=3       IGNORED (page 1 is a page of main)
//
// This engine has no pages, so it proves only cleanliness: every statement not
// provably unable to dirty main sets the flag, succeeded or not. false means
// clean; true means maybe, and execJournalMode declines.
func (db *DB) noteTransactionMayHaveDirtiedMain(sqlText string) {
	if !db.inTransaction() || db.txMayHaveDirtiedMain {
		return
	}
	verb, ok := LeadingStatementVerb(sqlText)
	if !ok {
		db.txMayHaveDirtiedMain = true // does not even lex: assume the worst
		return
	}
	switch verb {
	case "SELECT", "VALUES",
		"BEGIN", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE":
		return
	case "PRAGMA":
		// journal_mode's own setter is the one pragma that provably does not
		// dirty a page: two of them in a row inside one clean transaction BOTH
		// take (verified -- "BEGIN; PRAGMA journal_mode=delete; PRAGMA
		// journal_mode=truncate" ends in truncate). Every other pragma is
		// treated as a possible page write, which "PRAGMA user_version=3"
		// really is.
		if stmt, perr := ParsePragma(strings.TrimSpace(sqlText)); perr == nil && stmt != nil && stmt.Name == "journal_mode" {
			return
		}
	}
	db.txMayHaveDirtiedMain = true
}

// noteTransactionDirtiedMain is the definite counterpart (DB.txDirtiedMain):
// after a statement completes it records that C's pager is certainly in
// PAGER_WRITER_CACHEMOD, where a journal_mode setter is ignored
// (execJournalMode). Only verified kinds qualify:
//
//	INSERT INTO u VALUES(10)          (>=1 row)          IGNORED
//	DELETE FROM u                     (>=1 row)          IGNORED
//	INSERT INTO uwr VALUES('x')       (WITHOUT ROWID)    IGNORED
//
// Not:
//
//	UPDATE u SET a=a                  (>=1 row!)         TAKES -- an
//	  identical-record overwrite never calls sqlite3PagerWrite, so UPDATE is
//	  excluded wholesale;
//	INSERT OR IGNORE INTO uq ...      (0 rows kept)      TAKES;
//	INSERT INTO tt VALUES(5)          (TEMP table)       TAKES -- the target
//	  must resolve to a main table.
//
// A failed statement never reaches here; unrecorded keeps the decline.
func (db *DB) noteTransactionDirtiedMain(sqlText string, rowsAffected int64) {
	if !db.inTransaction() || db.txDirtiedMain || rowsAffected < 1 {
		return
	}
	verb, ok := LeadingStatementVerb(sqlText)
	if !ok {
		return
	}
	trimmed := strings.TrimSpace(sqlText)
	var target string
	switch verb {
	case "INSERT", "REPLACE":
		if st, err := parseInsertStmt(trimmed); err == nil {
			target = st.table
		}
	case "DELETE":
		if st, err := parseDeleteStmt(trimmed); err == nil {
			target = st.table
		}
	default:
		return
	}
	if target == "" {
		return
	}
	// Temp-first resolution is the conservative direction on purpose: a
	// same-named TEMP table shadows main here even for a "main."-qualified
	// write, and a target that is a view/virtual table resolves to nil --
	// both leave the flag unset, i.e. keep the decline.
	if t := db.findTableMeta(target); t == nil || t.isTemp {
		return
	}
	db.txDirtiedMain = true
}

// MarkHeldTransaction tells this session that a DRIVER is holding it open as
// one logical transaction (driver's BeginTx), rather than running each
// statement in its own autocommit session. See DB.heldTransaction (writer.go)
// for why the distinction matters at all -- fts3/fts4's index segments are
// flushed per transaction by C SQLite, so this engine's per-statement flush
// is only equivalent in autocommit.
func (db *DB) MarkHeldTransaction() { db.heldTransaction = true }

// EndHeldTransaction is MarkHeldTransaction's counterpart, for a driver that
// keeps the SAME session after its COMMIT or ROLLBACK (a connection's held
// segment session). It clears the deferred-FK promise too
// (MarkDeferredFKCommitter): the next statement is autocommit again. Left set,
// every later statement on the connection looked like it ran inside a
// transaction -- "BEGIN; INSERT; COMMIT; VACUUM" answered "cannot VACUUM from
// within a transaction".
func (db *DB) EndHeldTransaction() { db.heldTransaction, db.deferredFKCommitter = false, false }

// HasActiveTransaction reports whether this session has a transaction the
// engine opened (a BEGIN, or one a SAVEPOINT started), not one a driver holds
// the session open for (MarkHeldTransaction). Close discards an active engine
// transaction as abandoned, so a driver committing a held session must first
// commit one a forwarded SAVEPOINT started, or its rows are dropped.
func (db *DB) HasActiveTransaction() bool { return db.txActive }

// commitTxn implements COMMIT/END: it stops tracking the transaction and keeps
// every mutation; Session.Commit writes the state next. An outstanding deferred
// foreign key violation refuses the commit and leaves the transaction open, as
// in C (a following BEGIN is "cannot start a transaction within a
// transaction", and the same COMMIT succeeds once the violation is resolved),
// which is why this returns before clearTxnState.
func (db *DB) commitTxn() error {
	if !db.txActive {
		return fmt.Errorf("engine: %s", ErrMsgNoActiveCommit)
	}
	if err := db.fkCommitCheck(); err != nil {
		return err
	}
	if err := db.fts3AutomergeCommitCheck(); err != nil {
		return err
	}
	if err := db.attachedCommitLockCheck(); err != nil {
		return err
	}
	// Every ATTACHed database this transaction wrote into commits here too, main
	// last -- see attach_write.go, including the cross-file atomicity ceiling
	// that ordering does and does not buy.
	reserved := db.reservedAttached()
	if err := db.commitAttachedWrites(); err != nil {
		return err
	}
	db.attachedCommitRefresh(reserved)
	db.clearTxnState()
	return nil
}

// clearTxnState ends the transaction bookkeeping: no transaction active, no
// BEGIN-time snapshot, and -- critically -- an EMPTY savepoint stack. Every
// way a transaction can end (COMMIT, ROLLBACK, and the ON CONFLICT ROLLBACK /
// RAISE(ROLLBACK) mid-statement unwinds in vdbe_write.go, conflict.go,
// write_update_delete.go and trigger.go) goes through here so none can leave a
// stale savepoint behind for a later RELEASE to find. C SQLite behaves
// the same way: after "BEGIN; SAVEPOINT a; ROLLBACK", "RELEASE a" reports
// "no such savepoint: a", and so does the equivalent after an ON CONFLICT
// ROLLBACK violation tore the transaction down (both verified directly).
func (db *DB) clearTxnState() {
	// An fts3/fts4 segment this transaction was accumulating into ends here:
	// COMMIT seals it (what is written stays), and every rollback path calls
	// restoreSnapshot first, which discards the terms (fts3_txn.go).
	db.fts3TxnSeal()
	// An fts5 'secure-delete' bump deferred by a DELETE inside this
	// transaction is applied here -- C fts5's flush point for a COMMIT is
	// xSync, which SQLite calls as the first half of two-phase commit
	// (fts5_txn.go's header). On a rollback path this is a no-op: restoreSnapshot
	// (above, on every such path) already called fts5TxnDiscard, clearing the
	// pending set before this runs.
	db.fts5TxnFlush()
	db.fts3AutomergeTouched = nil // see its own doc comment (writer.go)
	db.txActive = false
	db.txSnapshot = nil
	// The transaction's undo logs end with it (row_store_undo.go).
	for _, t := range db.tables {
		t.rows.stopUndo()
	}
	// Whatever this transaction dirtied is no longer this transaction's. Note
	// that it is cleared on the way OUT rather than only at BEGIN, so a
	// DRIVER-held transaction (which never issues a SQL BEGIN) starts clean too
	// -- see noteTransactionMayHaveDirtiedMain.
	db.txMayHaveDirtiedMain = false
	db.txDirtiedMain = false
	db.txSchemaChanged = false
	// The DEFERRED foreign key counter is per-transaction and C SQLite
	// zeroes it on every way out of one, commit and rollback alike
	// (sqlite3VdbeHalt / sqlite3RollbackAll). A rollback path reaches here
	// after restoreSnapshot has already put back the BEGIN-time value, which
	// is 0 for the same reason: outside a transaction the counter is checked
	// and cleared at the end of every statement (fkFinishStatement).
	db.fkDeferred = 0
	// "PRAGMA defer_foreign_keys" is switched off at every COMMIT and ROLLBACK
	// of the outermost transaction, and its own counter zeroed with it
	// (sqlite3VdbeHalt / sqlite3RollbackAll do both). Verified directly against
	// mattn/go-sqlite3 3.53.3, including the two cases that are NOT this: a
	// COMMIT that FAILS the deferred check leaves the transaction open and the
	// flag set (returning before clearTxnState is exactly that), and a
	// SAVEPOINT / inner RELEASE / ROLLBACK TO never clears it.
	db.deferFKs = false
	db.fkDeferredImm = 0
	// Every ATTACHed database this transaction wrote into is unwound here, which
	// on the COMMIT path is a no-op (commitAttachedWrites ran first) and on every
	// rollback path is the undo -- see attach_write.go.
	db.rollbackTxnAttachedWrites()
	// The lock a transaction held on each attachment it touched goes with it,
	// so a DETACH after this one ends is free again -- see
	// attachedDB.touchedInTxn.
	for _, ad := range db.attached {
		ad.touchedInTxn = false
		ad.reservedInTxn = false
	}
	clear(db.savepoints)
	db.savepoints = nil
}

// rollbackTxn implements ROLLBACK: restore the BEGIN-time snapshot, discarding
// every row and schema change since.
//
// An attached database's session is rolled back only if this transaction wrote
// into it (clearTxnState's rollbackTxnAttachedWrites): autocommit writes before
// BEGIN were committed in C and must survive (enterTxnAttachedWrite flushes and
// reopens such a session at the transaction's first write):
//
//	ATTACH 'test2.db' AS aux; CREATE TABLE aux.t4(a,b,c);
//	INSERT INTO aux.t4 VALUES(7,8,9);          -- both in autocommit
//	BEGIN; ...; ROLLBACK;
//	INSERT INTO aux.t4 VALUES(17,18,19);       -- t4 must still exist
//
// (trigger1.test#7).
func (db *DB) rollbackTxn() error {
	if !db.txActive {
		return fmt.Errorf("engine: %s", ErrMsgNoActiveRollback)
	}
	db.restoreSnapshot(db.txSnapshot)
	// A schema-changing DDL that ran inside this transaction makes real
	// SQLite RELOAD the schema on rollback (sqlite3RollbackAll, main.c:1509 --
	// see DB.txSchemaChanged's own doc comment for the exact gate this
	// mirrors). restoreSnapshot just above already put db.tables/wsEdits back
	// to their BEGIN-time state, which for wsEdits is a no-op whenever the
	// catalog write predates the BEGIN (the shape writableSchemaDDLDecline's
	// own DDL-inside-transaction case exists for) -- reloadSchemaFromEdits is
	// what turns that restored-but-still-corrupted catalog into the same
	// "vanished object, surviving row" state a real reload leaves.
	if db.txSchemaChanged && db.writableSchemaEditsActive() {
		db.reloadSchemaFromEdits()
	}
	if db.txSchemaChanged {
		db.rtreeConns.reset() // sqlite3ResetAllSchemasOfConnection disconnects every vtab
	}
	db.clearTxnState()
	return nil
}

// ErrMsgNoSuchSavepoint is C SQLite's wording for a RELEASE or ROLLBACK TO
// naming a savepoint that is not on the stack -- including when there is no
// transaction open at all, where it reports THIS rather than a
// no-active-transaction error (verified directly against mattn/go-sqlite3:
// a bare "RELEASE x" in autocommit gives "no such savepoint: x"). The name is
// echoed exactly as the statement spelled it, after dequoting: RELEASE
// "Mixed Case" reports `no such savepoint: Mixed Case`.
const ErrMsgNoSuchSavepoint = "no such savepoint: "

func errNoSuchSavepoint(name string) error {
	return fmt.Errorf("engine: %s%s", ErrMsgNoSuchSavepoint, name)
}

// findSavepoint returns the index of the INNERMOST savepoint named name, or
// -1. Innermost, not outermost, because a repeated name shadows: C SQLite
// resolves "SAVEPOINT a; INSERT 1; SAVEPOINT a; INSERT 2; ROLLBACK TO a" to
// the second one, keeping row 1 and dropping row 2 (verified directly).
// Names compare case-insensitively ("SAVEPOINT Abc" is reachable as
// "ROLLBACK TO ABC", verified).
func (db *DB) findSavepoint(name string) int {
	for i := len(db.savepoints) - 1; i >= 0; i-- {
		if equalFoldName(db.savepoints[i].name, name) {
			return i
		}
	}
	return -1
}

// openSavepoint implements SAVEPOINT name: push a snapshot of the current
// state, starting a transaction first if none is open (C SQLite's
// SAVEPOINT does exactly that -- verified: a BEGIN immediately after a
// SAVEPOINT in autocommit is rejected with ErrMsgTxnNested, and a following
// COMMIT commits). Unlike BEGIN there is no "already active" error: nesting
// is the whole point.
func (db *DB) openSavepoint(name string) error {
	// Same reason beginTxn opts out of the incremental-commit fast path: a
	// snapshot restore does not restore each tableMeta's incremental-commit
	// bookkeeping. See beginTxn's own comment.
	// A SAVEPOINT SEALS the fts3/fts4 segment being accumulated: terms written
	// before it are already in their own segment, so a later ROLLBACK TO cannot
	// take them with it. Verified against the oracle -- see fts3_txn.go.
	db.fts3TxnSeal()
	// A SAVEPOINT also flushes any fts5 'secure-delete' bump deferred so far,
	// unconditionally -- C fts5's fts5SavepointMethod does too, on every
	// savepoint open (fts5_txn.go). Deliberately BEFORE captureSnapshot below:
	// the write this performs (fts5ConfigPut) must land in the state THIS
	// savepoint's own snapshot captures, so a later ROLLBACK TO this savepoint
	// does not also undo it -- matching C fts5, where the flush happens
	// before SQLite takes the pager-level savepoint mark.
	db.fts5TxnFlush()
	snap := db.captureSharedSnapshot()
	if snap == nil {
		// See captureSnapshot's/beginTxn's identical check: a nil result
		// means ensureTableLoaded failed and the real error is in
		// db.pendingLoadErr. SAVEPOINT must not report success while pushing
		// a savepoint entry that could never actually restore anything.
		err := db.pendingLoadErr
		db.pendingLoadErr = nil
		if err == nil {
			err = fmt.Errorf("engine: SAVEPOINT: internal error: captureSnapshot failed with no recorded cause")
		}
		return err
	}
	sp := savepoint{name: name, snap: snap}
	if !db.txActive {
		// This SAVEPOINT is what starts the transaction, so its snapshot is
		// also the one a plain ROLLBACK must restore. Sharing the pointer is
		// safe: rollbackTxn restores it and clears the stack in the same
		// breath, and rollbackToSavepoint never hands a stored snapshot out
		// without cloning it first (see cloneSnapshot).
		db.txActive = true
		db.txSnapshot = sp.snap
		sp.startedTxn = true
	}
	db.savepoints = append(db.savepoints, sp)
	// A transaction spans every member database, and so does a SAVEPOINT: an
	// attachment's writes live in their OWN session (attach_write.go), which
	// carries this same snapshot machinery, so the verb is mirrored onto each
	// open one. Without it a "ROLLBACK TO" silently KEPT everything written
	// into an attachment, which is why such a write used to be declined
	// outright while a savepoint was open.
	for _, ad := range db.attached {
		if ad.wdb != nil {
			if err := ad.wdb.openSavepoint(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// releaseSavepoint implements RELEASE [SAVEPOINT] name: pop that savepoint
// and every savepoint opened after it, KEEPING everything they did (a
// RELEASE is a commit of the sub-transaction, not an undo). If the popped
// entry is the one that started the transaction, the transaction itself
// commits too -- see this file's package doc comment.
func (db *DB) releaseSavepoint(name string) error {
	i := db.findSavepoint(name)
	if i < 0 {
		return errNoSuchSavepoint(name)
	}
	startedTxn := db.savepoints[i].startedTxn
	if startedTxn {
		// This RELEASE really is the outermost COMMIT, so it runs the same
		// DEFERRED foreign key check commitTxn does -- and, exactly like a
		// failed COMMIT, changes NOTHING when it fails. Verified directly:
		// "SAVEPOINT s1; INSERT an orphan; RELEASE s1" reports FOREIGN KEY
		// constraint failed, the row is still there, and s1 is still on the
		// stack (a second RELEASE s1, after a statement resolves the
		// violation, succeeds). Releasing an INNER savepoint checks nothing:
		// the transaction it belongs to is still open.
		if err := db.fkCommitCheck(); err != nil {
			return err
		}
		if err := db.fts3AutomergeCommitCheck(); err != nil {
			return err
		}
		if err := db.attachedCommitLockCheck(); err != nil {
			return err
		}
	}
	clear(db.savepoints[i:]) // drop the popped entries' snapshots for the GC
	db.savepoints = db.savepoints[:i]
	// A RELEASE keeps what the sub-transaction did, so each attached session
	// pops its matching entry and keeps its rows too.
	for _, ad := range db.attached {
		if ad.wdb != nil && ad.wdb.findSavepoint(name) >= 0 {
			if err := ad.wdb.releaseSavepoint(name); err != nil {
				return err
			}
		}
	}
	if startedTxn {
		// This RELEASE ends the transaction, so it COMMITS -- and every
		// ATTACHed database this transaction wrote into commits with it, main
		// last, exactly as commitTxn does. Without this the attachment's
		// writes were DISCARDED: clearTxnState unwinds them
		// (rollbackTxnAttachedWrites), which is a no-op on the COMMIT path
		// only because commitAttachedWrites has already run. The path was
		// unreachable while a cross-database write with a savepoint open was
		// declined, and exposing it is what turned it up.
		reserved := db.reservedAttached()
		if err := db.commitAttachedWrites(); err != nil {
			return err
		}
		db.attachedCommitRefresh(reserved)
		db.clearTxnState()
	}
	return nil
}

// rollbackToSavepoint implements ROLLBACK [TRANSACTION] TO [SAVEPOINT] name:
// restore that savepoint's state, discard later savepoints, keep this one and
// the transaction.
//
// Under "PRAGMA journal_mode=off" the restore is skipped: sqlite3PagerOpenSavepoint
// opens no sub-journal then, so "SAVEPOINT s; CREATE TABLE gone(y); INSERT INTO
// keep VALUES(2); ROLLBACK TO s" keeps both (DB.journalOffUndoDisabled). Stack
// bookkeeping still applies ("SAVEPOINT s1; SAVEPOINT s2; ROLLBACK TO s1;
// ROLLBACK TO s2" is "no such savepoint: s2"), and a later full ROLLBACK still
// undoes everything (savepoint.test 13.4).
//
// The schema-reload gate below is separate from the restore and not inside the
// journal_mode=off guard, as in C: OP_Savepoint's SAVEPOINT_ROLLBACK computes
// isSchemaChange from DBFLAG_SchemaChange, calls sqlite3BtreeSavepoint per
// database (a no-op under off), and then unconditionally resets the schemas if
// isSchemaChange (vdbe.c:3865-3939). The reload rebuilds from db.wsEdits as they
// stand, which is correct either way (as sqlite3InitOne rebuilds from the
// catalog, prepare.c:199). With journal_mode=off, a writable_schema-corrupted
// t1, then "BEGIN; SAVEPOINT s1; CREATE TABLE t2(y); ROLLBACK TO s1; SELECT *
// FROM t1" is "no such table: t1".
func (db *DB) rollbackToSavepoint(name string) error {
	i := db.findSavepoint(name)
	if i < 0 {
		return errNoSuchSavepoint(name)
	}
	if !db.journalOffUndoDisabled() {
		// last_insert_rowid() is a CONNECTION property, not database state, and
		// C SQLite does not revert it across a ROLLBACK TO (verified directly --
		// after "BEGIN; INSERT; SAVEPOINT s; INSERT; INSERT; ROLLBACK TO s" it
		// still reports the third insert's rowid, 3, even though only the first
		// row survives). txSnapshot no longer carries it (nor the changes()
		// counters, same rule), so restoreSnapshot leaves all three alone.
		db.restoreSnapshot(cloneSnapshot(db.savepoints[i].snap))
	}
	// Same schema-reload gate rollbackTxn applies, mirroring OP_Savepoint's own
	// SAVEPOINT_ROLLBACK case (vdbe.c:3939 -- isSchemaChange, the exact same
	// DBFLAG_SchemaChange test sqlite3RollbackAll uses). Deliberately OUTSIDE
	// the journalOffUndoDisabled guard above -- see this function's own doc
	// comment for why the reload does not depend on that restore having run.
	if db.txSchemaChanged && db.writableSchemaEditsActive() {
		db.reloadSchemaFromEdits()
	}
	if db.txSchemaChanged {
		db.rtreeConns.reset() // vdbe.c:3958's reset disconnects every vtab
	}
	clear(db.savepoints[i+1:])
	db.savepoints = db.savepoints[:i+1]
	// ...and undo the same sub-transaction in every attached write session.
	// One opened AFTER this savepoint still has a matching entry, because
	// attachedWriteSession replays the whole stack onto a session it opens
	// while savepoints are held -- so rolling back to it discards everything
	// that session has ever written, which is exactly right.
	undone := false
	for _, ad := range db.attached {
		if ad.wdb != nil && ad.wdb.findSavepoint(name) >= 0 {
			if err := ad.wdb.rollbackToSavepoint(name); err != nil {
				return err
			}
			undone = true
		}
	}
	// An undo moves what a cross-database READ must see just as a write does,
	// and the attachment's read pager is a SNAPSHOT of its session -- left
	// alone it keeps serving rows the rollback just discarded.
	if undone {
		return db.refreshAttachedWriteReaders()
	}
	return nil
}

// txnVerb is parseTxnStmt's result: which of the (post-alias) verbs a
// recognized transaction-control statement names. END aliases to txnCommit
// (C SQLite treats them identically, including the exact same
// no-active-transaction error text -- verified directly).
type txnVerb int

const (
	txnBegin txnVerb = iota
	txnCommit
	txnRollback
	txnSavepoint  // SAVEPOINT name
	txnRelease    // RELEASE [SAVEPOINT] name
	txnRollbackTo // ROLLBACK [TRANSACTION] TO [SAVEPOINT] name
)

// consumeBareKeyword is p.consumeKeyword restricted to an UNQUOTED spelling.
// The savepoint grammar needs the distinction that consumeKeyword does not
// make: RELEASE's optional SAVEPOINT keyword is consumed greedily, so bare
// `RELEASE savepoint` is a syntax error in C SQLite (it reads as RELEASE
// SAVEPOINT with the name missing -- "incomplete input") while quoted
// `RELEASE "savepoint"` releases the savepoint actually NAMED savepoint.
// Both verified directly against mattn/go-sqlite3.
func consumeBareKeyword(p *parser, kw string) bool {
	t := p.peek()
	if t.kind == tkIdent && !t.quoted && t.upper() == kw {
		p.next()
		return true
	}
	return false
}

// parseSavepointName consumes a savepoint name. SQLite's grammar for it is
// the same "nm" nonterminal a bare column name uses -- a plain or quoted
// identifier, or a string literal ('x' names a savepoint) -- so the set of
// keywords it refuses unquoted is the same nonIdentifierKeywords
// (sql_parser.go) already derived for column names. That is not an
// assumption: running "SAVEPOINT <kw>" over SQLite's whole keyword list
// against the oracle rejects EXACTLY those 58 words and no others, and
// compat-harness/vdbe_savepoint_test.go re-derives both sets the same way so
// a SQLite upgrade that moved a keyword between them would fail rather than
// silently diverge.
func parseSavepointName(p *parser) (string, bool) {
	t := p.peek()
	switch {
	case t.kind == tkIdent && (t.quoted || !nonIdentifierKeywords[t.upper()]):
		p.next()
		return t.text, true // the lexer already dequoted "x"/[x]/`x`
	case t.kind == tkString:
		p.next()
		return t.str, true
	}
	return "", false
}

// parseTxnStmt parses sqlText/toks (already known to start with
// BEGIN/COMMIT/END/ROLLBACK/SAVEPOINT/RELEASE) as one of:
//
//	BEGIN [DEFERRED|IMMEDIATE|EXCLUSIVE] [TRANSACTION [name]]
//	COMMIT|END [TRANSACTION [name]]
//	ROLLBACK [TRANSACTION [name]]
//	ROLLBACK [TRANSACTION] TO [SAVEPOINT] name
//	SAVEPOINT name
//	RELEASE [SAVEPOINT] name
//
// The name on the four whole-transaction verbs is "trans_opt ::= | TRANSACTION
// | TRANSACTION nm", and SQLite ignores it ("ROLLBACK TRANSACTION foo" rolls
// back, or with none open is "cannot rollback - no transaction is active"). It
// is legal only after TRANSACTION ("BEGIN foo" is a syntax error), is an
// identifier or string ("BEGIN TRANSACTION 123" is a syntax error), and "TO"
// after TRANSACTION still means the savepoint form.
//
// ok is false for anything else, so the caller reports an unsupported
// statement. name matters only for the savepoint verbs.
func parseTxnStmt(sqlText string, toks []token) (verb txnVerb, name string, ok bool) {
	p := newParser(sqlText, toks)
	switch {
	case p.consumeKeyword("BEGIN"):
		verb = txnBegin
		if !p.consumeKeyword("DEFERRED") && !p.consumeKeyword("IMMEDIATE") {
			p.consumeKeyword("EXCLUSIVE")
		}
		if p.consumeKeyword("TRANSACTION") {
			consumeTxnName(p)
		}
	case p.consumeKeyword("COMMIT"):
		verb = txnCommit
		if p.consumeKeyword("TRANSACTION") {
			consumeTxnName(p)
		}
	case p.consumeKeyword("END"):
		verb = txnCommit
		if p.consumeKeyword("TRANSACTION") {
			consumeTxnName(p)
		}
	case p.consumeKeyword("ROLLBACK"):
		verb = txnRollback
		sawTransaction := p.consumeKeyword("TRANSACTION")
		// "ROLLBACK TO ..." is the savepoint form, and TRANSACTION may
		// precede it ("ROLLBACK TRANSACTION TO a" is valid -- verified).
		// The optional transaction NAME is therefore consumed only if this
		// is NOT the savepoint form: "ROLLBACK TRANSACTION TO sp" must
		// rollback to sp, not rollback a transaction called "TO".
		if !consumeBareKeyword(p, "TO") {
			if sawTransaction {
				consumeTxnName(p)
			}
		} else {
			verb = txnRollbackTo
			consumeBareKeyword(p, "SAVEPOINT")
			if name, ok = parseSavepointName(p); !ok {
				return 0, "", false
			}
		}
	case p.consumeKeyword("SAVEPOINT"):
		verb = txnSavepoint
		if name, ok = parseSavepointName(p); !ok {
			return 0, "", false
		}
	case p.consumeKeyword("RELEASE"):
		verb = txnRelease
		consumeBareKeyword(p, "SAVEPOINT")
		if name, ok = parseSavepointName(p); !ok {
			return 0, "", false
		}
	default:
		return 0, "", false
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return 0, "", false
	}
	return verb, name, true
}

// txnStmtSyntaxErr is C's error for a savepoint statement parseTxnStmt rejects
// (parse.y:44-51's %syntax_error: `near "X": syntax error`, or "incomplete
// input" at end of statement). It walks only the grammar needing a name
// ("SAVEPOINT nm", "RELEASE [SAVEPOINT] nm", "ROLLBACK [TRANSACTION] TO
// [SAVEPOINT] nm") plus trailing tokens; nil otherwise, leaving the caller's
// error.
func txnStmtSyntaxErr(toks []token) error {
	kw := func(i int, w string) bool {
		return i < len(toks) && toks[i].kind == tkIdent && !toks[i].quoted && toks[i].upper() == w
	}
	i := 0
	switch {
	case kw(0, "SAVEPOINT"):
		i = 1
	case kw(0, "RELEASE"):
		i = 1
		if kw(i, "SAVEPOINT") {
			i++
		}
	case kw(0, "ROLLBACK"):
		i = 1
		if kw(i, "TRANSACTION") {
			i++
		}
		if !kw(i, "TO") {
			return nil // a plain ROLLBACK, whose name is optional
		}
		i++
		if kw(i, "SAVEPOINT") {
			i++
		}
	default:
		return nil
	}
	at := func(i int) error {
		if i >= len(toks) || toks[i].kind == tkEOF {
			return errors.New("engine: incomplete input")
		}
		return fmt.Errorf("engine: near %q: syntax error", toks[i].text)
	}
	// The NAME belongs here -- parseSavepointName's own predicate.
	if i >= len(toks) || toks[i].kind == tkEOF {
		return at(i)
	}
	t := toks[i]
	if !(t.kind == tkString || (t.kind == tkIdent && (t.quoted || !nonIdentifierKeywords[t.upper()]))) {
		return at(i)
	}
	// ...and nothing but an optional ";" after it.
	i++
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == ";" {
		i++
	}
	if i < len(toks) && toks[i].kind != tkEOF {
		return at(i)
	}
	return nil
}

// consumeTxnName consumes the optional name of SQLite's
// "trans_opt ::= TRANSACTION nm" production, which SQLite parses and then
// throws away (there are no named transactions). objectNameToken is the same
// "nm" the rest of this package uses -- an identifier, quoted or not, or a
// string literal -- which is why a NUMBER is left unconsumed and falls through
// to parseTxnStmt's trailing-token check as the syntax error SQLite reports.
func consumeTxnName(p *parser) {
	if _, ok := objectNameToken(p.peek()); ok {
		p.next()
	}
}

// TxnKind is TxnStmtKind's result: which of BEGIN/COMMIT/ROLLBACK a
// statement is (END is reported as TxnCommit -- see txnVerb's doc comment).
type TxnKind int

const (
	TxnBegin TxnKind = iota
	TxnCommit
	TxnRollback
)

// TxnStmtKind reports which of BEGIN/COMMIT/END/ROLLBACK sqlText is, with
// parseTxnStmt's grammar. ok is false for anything else, including malformed
// forms and the savepoint statements (SavepointStmt classifies those), so a
// ROLLBACK TO is never mistaken for a whole-transaction ROLLBACK. Exported for
// driver.Conn (execTxnStmt), which runs these against its held session rather
// than through ExecArgs.
func TxnStmtKind(sqlText string) (kind TxnKind, ok bool) {
	trimmed := strings.TrimSpace(sqlText)
	toks, err := lex(trimmed)
	if err != nil || len(toks) == 0 || toks[0].kind != tkIdent {
		return 0, false
	}
	switch strings.ToUpper(toks[0].text) {
	case "BEGIN", "COMMIT", "END", "ROLLBACK":
	default:
		return 0, false
	}
	verb, _, ok := parseTxnStmt(trimmed, toks)
	if !ok {
		return 0, false
	}
	switch verb {
	case txnBegin:
		return TxnBegin, true
	case txnCommit:
		return TxnCommit, true
	case txnRollback:
		return TxnRollback, true
	default: // txnRollbackTo -- a savepoint statement, not a transaction one
		return 0, false
	}
}

// BeginTxnLock reports what a BEGIN takes at once: a write lock for IMMEDIATE
// and EXCLUSIVE, and the readers' lock too for EXCLUSIVE. DEFERRED, the
// default, takes nothing until the first access (build.c:5258).
func BeginTxnLock(sqlText string) (write, exclusive bool) {
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil || len(toks) < 2 || toks[0].kind != tkIdent || toks[0].upper() != "BEGIN" || toks[1].kind != tkIdent || toks[1].quoted {
		return false, false
	}
	switch toks[1].upper() {
	case "IMMEDIATE":
		return true, false
	case "EXCLUSIVE":
		return true, true
	}
	return false, false
}

// SavepointKind names which of the three savepoint statements a text is, the
// exported form of txnVerb (as TxnKind is).
type SavepointKind int

// The three savepoint statements.
const (
	SavepointOpen       SavepointKind = iota + 1 // SAVEPOINT name
	SavepointRelease                             // RELEASE [SAVEPOINT] name
	SavepointRollbackTo                          // ROLLBACK [TRANSACTION] TO [SAVEPOINT] name
)

// SavepointStmt reports which savepoint statement sqlText is and the name it
// carries, using the same grammar Exec's own dispatch parses it with.
//
// Exported so driver.Conn can HOLD a session across a savepoint the way
// it already does across a BEGIN: a SAVEPOINT in autocommit implicitly starts
// a transaction in C SQLite, and the RELEASE of that same (outermost)
// savepoint COMMITS it -- verified against 3.53.3, where a COMMIT issued
// straight after such a RELEASE reports "cannot commit - no transaction is
// active" because the RELEASE already ended it. The driver needs the NAME to
// tell that outermost release from an inner one.
func SavepointStmt(sqlText string) (kind SavepointKind, name string, ok bool) {
	trimmed := strings.TrimSpace(sqlText)
	toks, err := lex(trimmed)
	if err != nil || len(toks) == 0 || toks[0].kind != tkIdent {
		return 0, "", false
	}
	switch strings.ToUpper(toks[0].text) {
	case "SAVEPOINT", "RELEASE", "ROLLBACK":
	default:
		return 0, "", false
	}
	verb, nm, pok := parseTxnStmt(trimmed, toks)
	if !pok {
		return 0, "", false
	}
	switch verb {
	case txnSavepoint:
		return SavepointOpen, nm, true
	case txnRelease:
		return SavepointRelease, nm, true
	case txnRollbackTo:
		return SavepointRollbackTo, nm, true
	}
	return 0, "", false
}
