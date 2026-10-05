// This file gates fts3PromoteSegments, which folds appendable segments back
// down during ordinary write flushes, previously a silent wrong answer.
package compat

import "testing"

// TestFts3PromoteMinimalRepro checks that appendable segments are folded back.
func TestFts3PromoteMinimalRepro(t *testing.T) {
	differ(t, "minimal repro", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('one')`,
		`INSERT INTO t VALUES('two')`,
		`INSERT INTO t(t) VALUES('merge=1000,2')`,
		`UPDATE t SET a='updated' WHERE rowid=1`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		// Promoted segments must remain readable.
		`SELECT rowid, a FROM t WHERE t MATCH 'updated OR two' ORDER BY rowid`,
	})
}

// TestFts3PromoteCascadeMultiStep checks promotion of multiple consecutive merges.
func TestFts3PromoteCascadeMultiStep(t *testing.T) {
	differ(t, "cascade multi-step", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('one')`,
		`INSERT INTO t VALUES('two')`,
		`INSERT INTO t(t) VALUES('merge=1000,2')`,
		`INSERT INTO t VALUES('three')`,
		`INSERT INTO t VALUES('four')`,
		`INSERT INTO t(t) VALUES('merge=1000,2')`,
		`UPDATE t SET a='updated' WHERE rowid=1`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		`SELECT rowid, a FROM t WHERE t MATCH 'updated OR two OR three OR four' ORDER BY rowid`,
	})
}

// TestFts3PromoteNoAppendableSegment verifies promotion is a no-op when needed.
func TestFts3PromoteNoAppendableSegment(t *testing.T) {
	differ(t, "no appendable segment", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('one')`,
		`INSERT INTO t VALUES('two')`,
		`UPDATE t SET a='updated' WHERE rowid=1`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
	})
}

// TestFts3PromoteDeclinesWhenTooBig checks promotion rejects oversized segments.
func TestFts3PromoteDeclinesWhenTooBig(t *testing.T) {
	differ(t, "too big to promote", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('alphabetlongwordone')`,
		`INSERT INTO t VALUES('alphabetlongwordtwo')`,
		`INSERT INTO t VALUES('alphabetlongwordthree')`,
		`INSERT INTO t VALUES('alphabetlongwordfour')`,
		`INSERT INTO t VALUES('alphabetlongwordfive')`,
		`INSERT INTO t VALUES('alphabetlongwordsix')`,
		`INSERT INTO t VALUES('alphabetlongwordseven')`,
		`INSERT INTO t VALUES('alphabetlongwordeight')`,
		`INSERT INTO t(t) VALUES('merge=1000,8')`,
		`INSERT INTO t VALUES('x')`,
		`SELECT level, idx, end_block FROM t_segdir ORDER BY level, idx`,
	})
}

// TestFts3PromoteInsideTransaction checks promotion happens in driver transactions.
func TestFts3PromoteInsideTransaction(t *testing.T) {
	differ(t, "promote inside a driver-held transaction", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('one')`,
		`INSERT INTO t VALUES('two')`,
		`INSERT INTO t(t) VALUES('merge=1000,2')`,
		`BEGIN`,
		`UPDATE t SET a='updated' WHERE rowid=1`,
		`COMMIT`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
	})
}

// TestFts3PromoteRollbackDoesNotPromote verifies rollback undoes promotion.
func TestFts3PromoteRollbackDoesNotPromote(t *testing.T) {
	differ(t, "rollback does not promote", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('one')`,
		`INSERT INTO t VALUES('two')`,
		`INSERT INTO t(t) VALUES('merge=1000,2')`,
		`BEGIN`,
		`UPDATE t SET a='updated' WHERE rowid=1`,
		`ROLLBACK`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		`SELECT rowid, a FROM t ORDER BY rowid`,
	})
}
