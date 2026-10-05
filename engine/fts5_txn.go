// This file defers one observable: the %_config 'version' row bump for
// secure-delete. It flushes at xSync, xRelease, xSavepoint, and on explicit
// xCommit (but not the literal xCommitMethod callback in C).
//
//	  xSync (fts5_main.c:2117), which SQLite calls as the first half of its
//	  two-phase commit -- functionally indistinguishable from "COMMIT
//	  flushes" for a single-database transaction, which is all this mirrors.
//	- xSavepointMethod (fts5_main.c:3162) DOES flush unconditionally on every
//	  SAVEPOINT open, matching the prose exactly.
//	- xReleaseMethod (fts5_main.c:3179) flushes only when the release COLLAPSES
//	  at least one savepoint NESTED inside the one being released
//	  ("(iSavepoint+1) < pTab->iSavepoint"); releasing the innermost savepoint
//	  (nothing nested under it) is a no-op. This engine does not track
//	  per-table savepoint depth, so an inner RELEASE that should flush per
//	  that rule is left undone here -- the exact same shape fts3_txn.go
//	  already accepts as a documented residual gap for fts3 (see that file's
//	  "What is STILL not sealed" section) rather than guess at a statement
//	  classifier. Releasing the OUTERMOST savepoint (which ends the whole
//	  transaction) is unaffected: releaseSavepoint's startedTxn branch runs
//	  the real commit path (clearTxnState), which flushes exactly like a
//	  plain COMMIT does.
//	- xRollbackMethod (fts5_main.c:2153) and xRollbackToMethod
//	  (fts5_main.c:3197) both discard unconditionally in the shapes this
//	  bump can reach (nothing has been WRITTEN yet -- deferring is the whole
//	  point -- so "discard" only ever means "forget the flag", never undo a
//	  write; xRollbackTo's own nested condition governs whether anything
//	  ACCUMULATED needs discarding at all, which for a bare boolean flag is
//	  moot).
//
// # The command channel also flushes or discards, per-table
//
// 'optimize', 'merge', 'flush', 'rank' and any configuration command
// (including 'secure-delete' itself) flush the ISSUING table's own pending
// bump before they run (fts5_shadow.go's fts5CommandInsert calls
// fts5TxnFlushTable);
// 'rebuild' and 'delete-all' discard it instead, since both reinitialize the
// index from scratch and leave nothing for a deferred removal to describe
// (fts5TxnDiscardTable). 'integrity-check' does neither -- it only reads.
//
// # What stays declined
//
// An UPDATE's removal (fts5_config.go's fts5SecureDeleteGuard, called with
// deferrable=false from vtab_write.go's updateVtab) is NOT deferred: real
// fts5's per-write flush trigger inside a transaction is not only the three
// txn callbacks above but also sqlite3Fts5IndexBeginWrite's own ordering rule
// (fts5_index.c:6788-6810), which can force a flush BETWEEN the delete-half
// and insert-half of a single UPDATE depending on rowid ordering already
// pending in the hash -- a rule this engine has no per-write hash to place.
// Verified empirically (compat-harness) that two single-row, literal-rowid
// DELETEs in one transaction do NOT trigger that path (ascending, no repeat),
// which is what fts5secure3.test's mined shape is; UPDATE is not.
package engine

import "fmt"

// fts5TxnDefer records that table name has a secure-delete version bump due
// once this transaction next reaches a real flush point (fts5TxnFlush). Called
// only from fts5NoteRowsRemoved, and only when fts5SecureDeleteGuard has
// already confirmed the removal is deferrable (a DELETE, not an UPDATE).
func (db *DB) fts5TxnDefer(name string) {
	if db.fts5TxnPending == nil {
		db.fts5TxnPending = map[string]bool{}
	}
	db.fts5TxnPending[name] = true
}

// fts5TxnFlush applies every table's deferred version bump and clears the set
// -- what a real xSync (COMMIT) or xSavepoint (SAVEPOINT open) does. A nil/
// empty map (the common case: no pending bump, or already discarded by
// fts5TxnDiscard on a rollback path that runs before this) makes it a no-op,
// which is what lets this be called unconditionally from clearTxnState
// (reached by both COMMIT and every rollback unwind) without checking which
// path got there.
func (db *DB) fts5TxnFlush() {
	for name := range db.fts5TxnPending {
		db.fts5ConfigPut(name, "version", Value{Typ: Int, I: fts5SecureDeleteVersion})
	}
	db.fts5TxnPending = nil
}

// fts5TxnDiscard drops every deferred bump without writing it -- what
// xRollback/xRollbackTo do. Nothing was ever written for a deferred bump (that
// is the point of deferring), so there is no on-disk state to undo here,
// unlike fts3TxnDiscard's segment rows: forgetting the flag is the whole undo.
func (db *DB) fts5TxnDiscard() { db.fts5TxnPending = nil }

// fts5TxnFlushTable applies name's deferred bump alone, if it has one -- the
// command channel's flush points ('optimize', 'merge', 'flush', and any
// configuration command, including 'secure-delete' itself), which only ever
// touch the ONE table the command's INSERT targets. Verified directly against
// the oracle for all four (fts5SpecialInsert calls sqlite3Fts5FlushToDisk
// before ConfigSetValue in its generic fallthrough; sqlite3Fts5IndexMerge and
// sqlite3Fts5IndexOptimize both call fts5IndexFlush() unconditionally at
// entry): "BEGIN; DELETE ... (pending); INSERT INTO t(t,rank)
// VALUES('pgsz',64); SELECT version" already reads 5 before the COMMIT.
func (db *DB) fts5TxnFlushTable(name string) {
	if !db.fts5TxnPending[name] {
		return
	}
	db.fts5ConfigPut(name, "version", Value{Typ: Int, I: fts5SecureDeleteVersion})
	delete(db.fts5TxnPending, name)
}

// fts5TxnDiscardTable drops name's deferred bump alone, without writing it --
// 'rebuild' and 'delete-all', which both fully reinitialize the index from
// scratch (sqlite3Fts5StorageRebuild / sqlite3Fts5StorageDeleteAll) and so
// have nothing left for a since-deferred removal to describe. Verified
// directly against the oracle for both: %_config still reads 4 after either
// command, and stays 4 through the COMMIT that follows.
func (db *DB) fts5TxnDiscardTable(name string) { delete(db.fts5TxnPending, name) }

// fts5TxnPendingDDLGuard declines a DDL statement (CREATE/DROP/ALTER/ANALYZE/
// REINDEX) that would run while a secure-delete bump is still deferred.
//
// Real fts5 WOULD flush here too: sqlite3VtabSavepoint's statement-sub-
// transaction mechanism -- the same one fts3_txn.go's own header derives at
// length for fts3 (isMultiWrite&&mayAbort, opened by OP_Transaction before
// most DDL runs) -- calls every registered vtab's xSavepoint, which flushes
// unconditionally (fts5_txn.go's header). Verified directly: "BEGIN; DELETE
// ... (secure-delete, pending); CREATE TABLE unrelated(x); SELECT version"
// reads 5 on the oracle. Reproducing that needs the exact statement
// classifier fts3_txn.go's "What is STILL not sealed" section says has no
// safe direction to guess in (sealing where C fts3/fts5 does not is as
// wrong as not sealing where it does), and this shape is unreached by either
// of this bucket's two mined scripts -- so this declines cleanly instead of
// guessing, exactly the choice AGENTS.md's Never Wrong rule asks for.
func (db *DB) fts5TxnPendingDDLGuard(kw string) error {
	if len(db.fts5TxnPending) == 0 {
		return nil
	}
	return fmt.Errorf("engine: %s while an fts5 table has a 'secure-delete' %%_config bump deferred inside this transaction is not supported by this engine: C fts5 flushes the bump here too (its statement-sub-transaction mechanism), which this engine does not model for DDL (fts5_txn.go)", kw)
}
