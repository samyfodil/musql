// This file implements SQLite's four connection-state functions --
// sqlite_version(), changes(), total_changes() and last_insert_rowid() -- and
// the per-connection counters behind them.
//
// Their value comes from the connection, and three change as statements run,
// so they compile to their own opcode (OpConnState) that reads the live
// counters at run time; a read inside a trigger body sees the trigger's own
// latest sub-statement.
//
// The counting rules, several of which contradict the obvious model:
//
//   - changes() is set only by INSERT/UPDATE/DELETE. SELECT, DDL, VACUUM,
//     ANALYZE, PRAGMA and transaction control leave it alone -- even a ROLLBACK
//     that discarded the counted rows. A statement modifying nothing sets it
//     to 0.
//   - Rows written by a trigger body or removed by an ON DELETE CASCADE do not
//     count toward changes(): "DELETE FROM p WHERE id=1" with two children
//     reports 1.
//   - Rows a REPLACE deleted to make room count in neither counter.
//   - total_changes() does count trigger and cascade rows (the DELETE above
//     moves it by 3).
//   - Neither counter is transactional: ROLLBACK restores neither, and rows a
//     trigger wrote before its statement aborted stay counted in
//     total_changes().
//   - A statement failing at prepare time (no such table, syntax error) leaves
//     both alone. One failing during execution with its work undone sets
//     changes() to 0; one that kept its work (INSERT OR FAIL) reports what it
//     kept.
//   - last_insert_rowid() updates per row: "INSERT INTO t(b)
//     VALUES(last_insert_rowid()),(last_insert_rowid())" after rowid 10 stores
//     10 then 11. A WITHOUT ROWID insert or a statement inserting nothing
//     leaves it; a multi-row insert that aborted keeps the last row it stored.
//
// The counters live on *DB beside caseSensitiveLike/fkEnforce and are copied
// onto every pager SnapshotPager makes.
package engine

import "fmt"

// SQLiteVersion is what sqlite_version() answers. It is a BUILD-TIME constant
// here, and it must equal the version this engine is written against, because
// the conformance gates compare its output against a linked C library's
// sqlite3_libversion() byte for byte. That library is mattn/go-sqlite3's
// amalgamation -- every "verified against 3.53.3" comment in this package cites
// the same one -- and compat-harness/connection_state_funcs_test.go fails
// loudly if the oracle ever moves off it, so this can never silently diverge.
const SQLiteVersion = "3.53.3"

// sqliteVersionNumber is SQLiteVersion in the packed form the file header
// carries at offset 96 (SQLITE_VERSION_NUMBER: major*1000000 + minor*1000 +
// patch). It is DERIVED here rather than written as a literal because it was
// last a literal -- 3045000, i.e. 3.45.0 -- and stayed there while
// SQLiteVersion moved to 3.53.3, so every file musql wrote disagreed with the
// oracle's in that field. The header field is informational, which is exactly
// why nothing caught it until the files were compared BYTE FOR BYTE.
const sqliteVersionNumber = 3*1_000_000 + 53*1_000 + 3

// setChanges is sqlite3VdbeSetChanges: it publishes n as this connection's
// changes() AND adds it to total_changes(). The two counters move together at
// every point C SQLite calls that function -- the end of a statement, the
// end of each non-SELECT step of a trigger body, and each trigger/foreign-key
// sub-program's exit -- which is the whole reason total_changes() ends up
// counting trigger and cascade rows that changes() does not: those are
// published by the INNER calls, and the outer statement's own call overwrites
// changes() afterwards while only ADDING to the total.
func (db *DB) setChanges(n int64) {
	db.nChange = n
	db.nTotalChange += n
}

// RowsInserted is "PRAGMA count_changes"'s "rows inserted" for the statement
// that just finished -- insert.c's regRowCount, which is NOT changes(). See
// DB.nRowsInserted (writer.go) for the C and the one shape that splits them
// (an upsert's DO UPDATE branch), and driver's Conn.countChangesRow, which
// is where the row itself is produced because ExecArgs has no channel for one.
func (db *DB) RowsInserted() int64 { return db.nRowsInserted }

// enterTriggerFrame is a trigger sub-program's OP_Program, and the returned
// function its OP_Halt: entering saves changes() and leaving restores it (C's
// VdbeFrame.nDbChange, sqlite3VdbeFrameRestore). So a second trigger on the
// same statement reads the value from before the first fired:
//
//	UPDATE t0 SET x=x WHERE x<3   -- changes() == 2
//	INSERT INTO t1 VALUES(1)      -- trigger 1: UPDATE t0 (5 rows);
//	                              -- trigger 2: INSERT INTO seen VALUES(changes())
//	SELECT v FROM seen            -- 2   (restore; not 5, not 0)
//
// (C fires the most recently created trigger first.) A trigger whose WHEN is
// false enters and halts in C, which is a no-op round trip, so callers skip
// the frame. total_changes() is unaffected: each body step's OP_ResetCount
// already published and cleared the frame's count.
func (db *DB) enterTriggerFrame() func() {
	saved := db.nChange
	return func() { db.nChange = saved }
}

// markConnStateOpaque records that this session wrote a virtual table, after
// which last_insert_rowid()/total_changes() decline for the rest of the
// session. A module implements writes as ordinary SQL on its shadow tables on
// the same connection, moving the counters by implementation-detail amounts
// (an fts3 insert moves total_changes() by 3; an fts4 DELETE leaves
// last_insert_rowid() 0), and this engine stores those tables differently.
//
// The exception is markVtabInsertRowid: every vtab row INSERT ends in
// OP_VUpdate, which overwrites last_insert_rowid() (vdbe.c:8746), so that
// counter stays exact. changes() is unaffected: it is the vtab's own reported
// row count.
func (db *DB) markConnStateOpaque() {
	db.totalChangesOpaque, db.lastRowidOpaque = true, true
}

// markLastRowidOpaque gives up on last_insert_rowid() alone, for an fts3/fts4
// INSERT that did NOT end in the clean "OP_VUpdate stored this docid" state
// markVtabInsertRowid describes: one that failed part-way. C SQLite keeps
// whatever the module's own shadow-table SQL last stored there -- verified:
// "INSERT INTO t1(docid,a) VALUES(11,'p'),(11,'q')" into a table already
// holding docid 11 reports "constraint failed" and leaves
// last_insert_rowid()==11, the first row it had written before aborting. This
// engine STAGES every row and writes none of them when a statement declines
// (see fts3StagedRow), so it has no such partial state to report.
func (db *DB) markLastRowidOpaque() { db.lastRowidOpaque = true }

// markVtabInsertRowid publishes what a virtual-table row INSERT leaves
// last_insert_rowid() at, and re-opens the counter markConnStateOpaque closed.
// OP_VUpdate's "if( rc==SQLITE_OK && pOp->p1 ) db->lastRowid = rowid"
// (vdbe.c:8746) runs for every top-level INSERT, whatever the module
// (insert.c:1561), so it is the rowid of the last row stored -- auto-assigned,
// explicit or negative, multi-row, INSERT ... SELECT, contentless or
// external-content, qualified, and surviving a ROLLBACK.
//
// rowid 0 is the fts3/fts4 command channel's answer ('optimize', 'rebuild',
// 'merge=...', ...): fts3's xUpdate leaves *pRowid alone and OP_VUpdate starts
// it at 0. A statement storing no rows never runs OP_VUpdate and leaves the
// counter; so does a row dropped by OR IGNORE, since the assignment needs
// rc==SQLITE_OK (see vtabConstraint).
func (db *DB) markVtabInsertRowid(rowid int64) {
	db.lastInsertRowid, db.lastRowidOpaque = rowid, false
}

// connStateFuncs is the set of function names conn_state.go owns. They are
// deliberately absent from supportedFuncs (scalar_call.go): that set is also what
// decides whether a function may appear in a GENERATED column or an INDEX
// expression, and C SQLite rejects all four THERE ("non-deterministic
// functions prohibited in index expressions") -- so leaving them out keeps
// those two DDL rejections, while the three call sites below opt them in for
// ordinary evaluation.
var connStateFuncs = map[string]bool{
	"sqlite_version":    true,
	"changes":           true,
	"total_changes":     true,
	"last_insert_rowid": true,
}

// isConnStateFunc reports whether name is one of the four. name must already
// be lower-cased (every caller holds a normalized FuncExpr.Name).
func isConnStateFunc(name string) bool { return connStateFuncs[name] }

// connStateSource is what OpConnState reads: the live *DB for a write run (so a
// trigger body sees its previous step's counts), or the copy SnapshotPager
// stamped for a read run. known is false when neither is reachable, and the
// stateful functions then decline: that is a context the session was not
// threaded into, not a fresh connection, so answering 0 would be wrong.
type connStateSource struct {
	changes, totalChanges, lastInsertRowid int64
	totalChangesOpaque, lastRowidOpaque    bool
	known                                  bool
}

func (db *DB) connState() connStateSource {
	return connStateSource{db.nChange, db.nTotalChange, db.lastInsertRowid, db.totalChangesOpaque, db.lastRowidOpaque, true}
}

func (p *ReadOnlyPager) connState() connStateSource {
	return connStateSource{p.changes, p.totalChanges, p.lastInsertRowid, p.totalChangesOpaque, p.lastRowidOpaque, true}
}

// value answers one of the four functions from this source.
func (cs connStateSource) value(name string) (Value, error) {
	if name == "sqlite_version" {
		return Value{Typ: Text, S: []byte(SQLiteVersion)}, nil // a constant; needs no connection
	}
	if !cs.known {
		return Value{}, fmt.Errorf("engine: %s() has no connection to read in this context", name)
	}
	switch name {
	case "changes":
		return Value{Typ: Int, I: cs.changes}, nil
	case "total_changes":
		if cs.totalChangesOpaque {
			return Value{}, fmt.Errorf("engine: total_changes() after a virtual-table write is not reproducible on this engine")
		}
		return Value{Typ: Int, I: cs.totalChanges}, nil
	case "last_insert_rowid":
		if cs.lastRowidOpaque {
			return Value{}, fmt.Errorf("engine: last_insert_rowid() after a virtual-table write is not reproducible on this engine")
		}
		return Value{Typ: Int, I: cs.lastInsertRowid}, nil
	}
	return Value{}, fmt.Errorf("engine: not a connection-state function: %s()", name)
}

// connStateValue resolves OpConnState from this run's source, or declines for
// a program with neither pager nor write context (e.g. RunNoFrom). See
// connStateSource.known.
//
// The order matches evalCtx.connState: every db in reach before any pager. C
// reads these off the connection (db->nChange, db->lastRowid), so a live
// session is the faithful source and a pager's copy the frozen one. m.outer
// (the enclosing query's evalCtx, which carries the write session when a read
// program runs inside a write) is in the chain so the agreement with
// evalCtx.connState is structural; aggItemLowerable depends on it. No answer
// is known to change without it.
func (m *vdbe) connStateValue(name string) (Value, error) {
	if m.wctx != nil && m.wctx.db != nil {
		return m.wctx.db.connState().value(name)
	}
	for c := m.outer; c != nil; c = c.outer {
		if c.db != nil {
			return c.db.connState().value(name)
		}
	}
	if m.pager != nil {
		return m.pager.connState().value(name)
	}
	for c := m.outer; c != nil; c = c.outer {
		if c.pager != nil {
			return c.pager.connState().value(name)
		}
	}
	return connStateSource{}.value(name)
}

// SetConnState stamps a connection's changes()/total_changes()/
// last_insert_rowid() onto a pager opened straight from disk, for a driver
// whose autocommit statements each run on their OWN throwaway session and so
// cannot carry the counters in the *DB (driver's Conn -- the same bridge
// SetForeignKeys/SetSecureDelete already are). The two opacity bits mark which
// of the counters a virtual-table write has put out of reach; see
// markConnStateOpaque.
func (p *ReadOnlyPager) SetConnState(changes, totalChanges, lastInsertRowid int64, totalChangesOpaque, lastRowidOpaque bool) {
	p.changes, p.totalChanges, p.lastInsertRowid = changes, totalChanges, lastInsertRowid
	p.totalChangesOpaque, p.lastRowidOpaque = totalChangesOpaque, lastRowidOpaque
}

// ConnState reports this session's changes(), total_changes() and
// last_insert_rowid() plus which of the last two a virtual-table write has made
// unanswerable, so a driver can carry them onto its next statement's session.
// The counterpart of SetConnState.
func (db *DB) ConnState() (changes, totalChanges, lastInsertRowid int64, totalChangesOpaque, lastRowidOpaque bool) {
	return db.nChange, db.nTotalChange, db.lastInsertRowid, db.totalChangesOpaque, db.lastRowidOpaque
}

// SetConnState restores counters a previous session on the same CONNECTION
// left behind (driver's Conn again -- see the *ReadOnlyPager method).
func (db *DB) SetConnState(changes, totalChanges, lastInsertRowid int64, totalChangesOpaque, lastRowidOpaque bool) {
	db.nChange, db.nTotalChange, db.lastInsertRowid = changes, totalChanges, lastInsertRowid
	db.totalChangesOpaque, db.lastRowidOpaque = totalChangesOpaque, lastRowidOpaque
}

// connState resolves an evalCtx chain's connection state: the WRITE session if
// this ctx (or any enclosing one) carries it -- so a trigger body's changes()
// sees the counters its own previous body statement moved -- and otherwise the
// snapshot the read was run against. A ctx with neither DECLINES; see
// connStateSource.known for why that is not an answer of zero.
//
// It has no caller left: connStateValue above walks the same chain, in the same
// order, straight off the machine. Kept because it is the written-down
// statement of that order, which aggItemLowerable's decision to stop refusing
// these functions rests on -- see connStateValue's doc comment.
func (ctx *evalCtx) connState() connStateSource {
	for c := ctx; c != nil; c = c.outer {
		if c.db != nil {
			return c.db.connState()
		}
	}
	for c := ctx; c != nil; c = c.outer {
		if c.pager != nil {
			return c.pager.connState()
		}
	}
	return connStateSource{}
}
