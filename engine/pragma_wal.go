// PRAGMA wal_checkpoint and wal_autocheckpoint on this engine's format, where
// the delta beside the segment file is the write-ahead log and a file rewrite
// -- which folds the log into the segments and drops it -- is the checkpoint
// (ConvertedCatalog.JournalWAL).
package engine

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// walDefaultAutoCheckpoint is C SQLite's own default wal_autocheckpoint, what
// the getter answers until a connection sets one (DB.walAutoCheckpoint).
const walDefaultAutoCheckpoint = 1000

// walCheckpointModes is every checkpoint mode name C SQLite accepts as
// "PRAGMA wal_checkpoint(<mode>)". Only TRUNCATE empties the log.
var walCheckpointModes = map[string]bool{
	"passive": true, "full": true, "restart": true, "truncate": true,
}

// walCheckpointDecline is the answer to any checkpoint but TRUNCATE over a
// non-empty log: C's log and checkpointed columns count the PAGE frames its
// pager wrote, and a delta holds row records, so those two numbers have no
// counterpart here. TRUNCATE leaves C's log empty too, and is answered.
func walCheckpointDecline(mode string) error {
	return fmt.Errorf("%w: PRAGMA wal_checkpoint(%s) over a non-empty log -- its log/checkpointed columns count the page frames C SQLite's pager wrote, and this format's log holds row records; TRUNCATE, and any checkpoint of an empty log, are exact and are answered", errVDBEUnsupported, strings.ToUpper(mode))
}

// segLogNonEmpty reports whether the database at path has a delta holding any
// record -- a log a checkpoint would have something to fold.
func segLogNonEmpty(path string) bool {
	fi, err := os.Stat(path + segDeltaSuffix)
	return err == nil && fi.Size() > int64(segDeltaHdrSize)
}

// walCheckpointRows returns PRAGMA wal_checkpoint's answer: three INTEGER
// columns (busy, log, checkpointed) read from the current state of the side
// files. busy is 0 (no reader wait), log and checkpointed are 0 in WAL mode
// and -1 outside it.
func walCheckpointRows(isWAL bool) ([]string, [][]Value) {
	cols := []string{"busy", "log", "checkpointed"}
	n := int64(-1)
	if isWAL {
		n = 0
	}
	return cols, [][]Value{{{Typ: Int, I: 0}, {Typ: Int, I: n}, {Typ: Int, I: n}}}
}

// walCheckpointModeOf is the checkpoint mode a statement asks for. An
// UNRECOGNIZED name is silently treated as the default PASSIVE rather than
// rejected -- verified against the oracle, which answers "PRAGMA
// wal_checkpoint(bogus)" and "PRAGMA wal_checkpoint(1)" exactly as it answers
// the bare form, with no error. (This engine's first cut errored on them, which
// the differ caught.)
func walCheckpointModeOf(stmt *PragmaStmt) string {
	if !stmt.HasValue {
		return "passive"
	}
	mode := strings.ToLower(strings.TrimSpace(stmt.ValueText))
	if !walCheckpointModes[mode] {
		return "passive"
	}
	return mode
}

// walCheckpointPragma runs "PRAGMA [db.]wal_checkpoint[(MODE)]" on a write
// session and answers it.
func (db *DB) walCheckpointPragma(stmt *PragmaStmt) ([]string, [][]Value, error) {
	mode := walCheckpointModeOf(stmt)
	// Inside an explicit TRANSACTION C SQLite refuses a checkpoint with
	// "database table is locked" -- but only once the transaction has actually
	// started: verified that "BEGIN; SELECT ...; PRAGMA wal_checkpoint" fails,
	// as do BEGIN IMMEDIATE and a transaction that has written, while a bare
	// deferred "BEGIN; PRAGMA wal_checkpoint" SUCCEEDS because nothing has been
	// read yet. This engine cannot tell an untouched deferred transaction from a
	// started one, so it declines inside ANY explicit transaction: that refuses
	// the one form C SQLite allows, which costs a statement but can never
	// answer a checkpoint C SQLite rejected. wal.test asks for exactly this
	// (a checkpoint with a read open) and the corpus scored the accept WRONG.
	//
	// Closing this gap is NOT worth the read-transaction bit it would take, and
	// that was measured rather than assumed. The mined corpus reaches this
	// decline 12 times; classifying each by what the ORACLE does with it:
	//
	//	11  the oracle REJECTS too -- fallocate/snapshot_up/snapshot_fault/
	//	    wal3/wal6, every one of them "BEGIN; <read or write>; PRAGMA
	//	    wal_checkpoint"
	//	 1  the oracle ACCEPTS -- snapshot2.test#8, an untouched deferred BEGIN
	//
	// and a REJECTION is never a corpus pass: PRAGMA is excluded from the
	// oracle-probe list (compat-harness/tcl_test.go's tclExecProbeSafe, since a
	// savepoint rollback does not undo a pragma), so a statement this engine
	// errors on stays booked as unsupported whatever the oracle would have done
	// with it. Modelling "has the read begun" would therefore convert exactly
	// one statement. The rules are recorded here for whoever needs the bit for
	// another reason, all verified against 3.53.3: the refusal fires once a
	// statement inside the transaction has TOUCHED A TABLE -- "SELECT 1" does
	// not start the read and the checkpoint still succeeds after it, while
	// "SELECT count(*) FROM t", "SELECT count(*) FROM sqlite_master", "PRAGMA
	// user_version", "PRAGMA table_info(t)" and "PRAGMA integrity_check" all do
	// -- and BEGIN IMMEDIATE/EXCLUSIVE refuse it straight away. A bare
	// "SAVEPOINT s1" behaves exactly like a deferred BEGIN, and COMMIT/ROLLBACK/
	// RELEASE release it again.
	if db.inTransaction() {
		return nil, nil, fmt.Errorf("%w: PRAGMA wal_checkpoint inside an explicit transaction (C SQLite refuses one whose read has begun with \"database table is locked\", and this engine cannot tell an untouched deferred transaction from a started one)", errVDBEUnsupported)
	}
	return db.segmentWALCheckpoint(mode)
}

// WalCheckpointPragma exports walCheckpointPragma for driver. execPragma's
// own "wal_checkpoint" case (pragma.go) discards the row this call produces --
// correct for a plain db.Exec(), which C SQLite itself never surfaces a
// result row for -- but driver's Query/QueryRow path needs the ACTUAL
// answer, including a busy=1 row: that fact cannot be reconstructed afterwards
// from file state alone (see queryPragmaStmt's read-only "wal_checkpoint"
// case, which only ever sees what the file looks like NOW, not whether THIS
// attempt made progress).
//
// Unlike execPragma's own "wal_checkpoint" case above -- which runs behind
// execPragma's unconditional checkWriteSchemaQualifier (pragma.go) and its
// declineFileScopedTempPragma gate for a name absent from
// tempPragmaAnswersMain's allowlist -- driver's conn.go calls THIS
// entry point directly, bypassing both. walCheckpointPragma itself never
// consults stmt.Schema (this call only ever checkpoints db's own file), so
// without a gate here a schema-qualified "PRAGMA <schema>.wal_checkpoint"
// silently checkpointed db (whatever <schema> named -- a nonexistent
// database, a genuinely ATTACHed one, or temp) and reported success, which a
// live regression found BLOCK-severity: an ATTACHed database can never
// actually be checkpointed through this call (a driver connection never
// mirrors its own ATTACH registry onto this write session -- see attach.go's
// package doc comment -- so db.attachedNamed always misses it here), so
// accepting its name would be exactly as wrong as accepting one that does not
// exist at all. Running the identical two checks execPragma already runs
// closes that gap for both of WalCheckpointPragma's two callers (the held
// c.tx branch and the autocommit c.path branch, driver/conn.go) with the
// SAME decline wording execPragma has always produced for them, rather than
// driver reimplementing the rule a second time.
func (db *DB) WalCheckpointPragma(stmt *PragmaStmt) ([]string, [][]Value, error) {
	if err := db.checkWriteSchemaQualifier(stmt.Schema); err != nil {
		return nil, nil, err
	}
	if err := declineFileScopedTempPragma(stmt.Schema, stmt.Name); err != nil {
		return nil, nil, err
	}
	return db.walCheckpointPragma(stmt)
}

// walAutoCheckpointPragma runs "PRAGMA [db.]wal_autocheckpoint[=N]". C SQLite
// answers ONE row of one INTEGER column named wal_autocheckpoint, holding the
// threshold in effect AFTER the statement: the default is 1000, and a value <= 0
// DISABLES automatic checkpointing and reads back as 0 (verified: "=0" and "=-5"
// both answer 0). A non-numeric value is treated as 0, which is
// sqlite3Atoi's own behavior.
func (db *DB) walAutoCheckpointPragma(stmt *PragmaStmt) ([]string, [][]Value, error) {
	if stmt.HasValue {
		n, err := strconv.Atoi(strings.TrimSpace(stmt.ValueText))
		if err != nil {
			n = 0
		}
		if n < 0 {
			n = 0
		}
		db.walAutoCheckpoint = n
	}
	return []string{"wal_autocheckpoint"}, [][]Value{{{Typ: Int, I: int64(db.walAutoCheckpoint)}}}, nil
}

// SetWalAutoCheckpoint sets this session's automatic-checkpoint threshold in
// frames; 0 disables it. The driver pushes the connection's value in before
// every statement, exactly as it does for foreign_keys.
func (db *DB) SetWalAutoCheckpoint(n int) {
	if n < 0 {
		n = 0
	}
	db.walAutoCheckpoint = n
}

// WalAutoCheckpoint reports the current threshold.
func (db *DB) WalAutoCheckpoint() int { return db.walAutoCheckpoint }

// segmentWALCheckpoint is PRAGMA wal_checkpoint's action: not in wal mode it is
// C's non-WAL answer, 0|-1|-1, and touches nothing; an EMPTY log is 0|0|0 in
// every mode; TRUNCATE over a non-empty log rewrites the file at commit and
// answers 0|0|0; any other mode over one is declined (walCheckpointDecline).
func (db *DB) segmentWALCheckpoint(mode string) ([]string, [][]Value, error) {
	if db.segWAL && segLogNonEmpty(db.path) {
		if mode != "truncate" {
			return nil, nil, walCheckpointDecline(mode)
		}
		db.segmentVacuumPending = true // the rewrite IS the checkpoint; see Session.rewriteFile
	}
	cols, rows := walCheckpointRows(db.segWAL)
	return cols, rows, nil
}
