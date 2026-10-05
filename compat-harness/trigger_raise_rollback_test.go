// RAISE(ROLLBACK) in AFTER INSERT trigger: unwinds whole transaction.
package compat

import "testing"

func createTbl3AndTriggers() []string {
	return []string{
		`CREATE TABLE tbl(a, b ,c)`,
		`CREATE TRIGGER before_tbl_insert BEFORE INSERT ON tbl BEGIN SELECT CASE WHEN (new.a = 4) THEN RAISE(IGNORE) END; END`,
		`CREATE TRIGGER after_tbl_insert AFTER INSERT ON tbl BEGIN SELECT CASE WHEN (new.a = 1) THEN RAISE(ABORT, 'Trigger abort') WHEN (new.a = 2) THEN RAISE(FAIL, 'Trigger fail') WHEN (new.a = 3) THEN RAISE(ROLLBACK, 'Trigger rollback') END; END`,
	}
}

// ROLLBACK trigger on second insert unwinds whole transaction.
func TestTriggerRaiseRollbackUnwindsTransaction(t *testing.T) {
	stmts := append(createTbl3AndTriggers(),
		`BEGIN`,
		`INSERT INTO tbl VALUES (5, 5, 6)`,
		`INSERT INTO tbl VALUES (3, 5, 6)`,
		`SELECT * FROM tbl`,
	)
	differ(t, "RAISE(ROLLBACK) unwinds the whole transaction", stmts)
}

// TestTriggerRaiseRollbackThenCommitErrors: after the automatic unwind, a
// following COMMIT has nothing to commit.
func TestTriggerRaiseRollbackThenCommitErrors(t *testing.T) {
	stmts := append(createTbl3AndTriggers(),
		`BEGIN`,
		`INSERT INTO tbl VALUES (5, 5, 6)`,
		`INSERT INTO tbl VALUES (3, 5, 6)`,
		`COMMIT`,
	)
	differ(t, "COMMIT after an automatic ROLLBACK-trigger unwind errors", stmts)
}

// TestTriggerRaiseRollbackAgainstAbortAndFail runs the ABORT/FAIL/ROLLBACK
// progression trigger3.test itself does, in order, over ONE connection, so
// the ROLLBACK case is reached with the same accumulated state the mined
// corpus statement sees (rather than in isolation).
//
// The FAIL block deliberately does NOT re-observe table contents with a
// SELECT right after the FAIL-ing INSERT, unlike the ABORT block: a
// statement that errors with PARTIAL effects preserved (RAISE(FAIL), OR
// FAIL) is unmeasurable through differ() immediately afterward -- the worker
// harness re-runs a failing statement, doubling exactly that partial effect,
// where an ABORT/ROLLBACK's full undo is idempotent under the same re-run
// and so stays safe to observe (see the "Worker double-execution trap" note:
// this was caught here as a spurious extra (2,5,6) row musql alone showed).
func TestTriggerRaiseRollbackAgainstAbortAndFail(t *testing.T) {
	stmts := append(createTbl3AndTriggers(),
		// ABORT: the whole statement (and this transaction, since it is the
		// only thing in it) is undone; an explicit ROLLBACK still works.
		`BEGIN`,
		`INSERT INTO tbl VALUES (5, 5, 6)`,
		`INSERT INTO tbl VALUES (1, 5, 6)`,
		`SELECT * FROM tbl`,
		`ROLLBACK`,
		`SELECT * FROM tbl`,
		// FAIL: earlier rows in the same statement/transaction are KEPT (not
		// independently re-verified here -- see doc comment above).
		`BEGIN`,
		`INSERT INTO tbl VALUES (5, 5, 6)`,
		`INSERT INTO tbl VALUES (2, 5, 6)`,
		`ROLLBACK`,
		`SELECT * FROM tbl`,
		// ROLLBACK: unwinds the whole transaction by itself.
		`BEGIN`,
		`INSERT INTO tbl VALUES (5, 5, 6)`,
		`INSERT INTO tbl VALUES (3, 5, 6)`,
		`SELECT * FROM tbl`,
	)
	differ(t, "ABORT, FAIL, then ROLLBACK trigger progression", stmts)
}

// TestTriggerRaiseRollbackNoActiveTransaction is trigger3-3.3/#3035: with NO
// transaction active (autocommit), RAISE(ROLLBACK) degrades to behaving like
// FAIL/ABORT for that single statement -- there is no enclosing transaction
// left to unwind.
func TestTriggerRaiseRollbackNoActiveTransaction(t *testing.T) {
	stmts := append(createTbl3AndTriggers(),
		`INSERT INTO tbl VALUES (3, 9, 10)`,
		`SELECT * FROM tbl`,
	)
	differ(t, "RAISE(ROLLBACK) with no active transaction", stmts)
}
