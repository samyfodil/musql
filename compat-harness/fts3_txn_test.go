// This file tests fts3/fts4 DML inside explicit transactions.
// Tests verify segment layout matches C SQLite, including docid ordering
// effects on segment boundaries.
package compat

import "testing"

// fts3TxnCases are the oracle scripts probed while deriving the rules, now run
// against both engines.
var fts3TxnCases = []struct {
	name  string
	stmts []string
}{
	{"two-inserts-one-segment", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`BEGIN`, `INSERT INTO t VALUES('alpha')`, `INSERT INTO t VALUES('beta')`, `COMMIT`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		`SELECT docid, a FROM t ORDER BY docid`,
		`SELECT count(*) FROM t WHERE t MATCH 'alpha'`,
	}},
	{"descending-docids-seal", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`BEGIN`,
		`INSERT INTO t(docid,a) VALUES(9,'alpha')`,
		`INSERT INTO t(docid,a) VALUES(2,'beta')`,
		`INSERT INTO t(docid,a) VALUES(5,'gamma')`,
		`COMMIT`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		`SELECT docid,a FROM t ORDER BY docid`,
	}},
	{"ascending-docids-one-segment", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`BEGIN`,
		`INSERT INTO t(docid,a) VALUES(2,'alpha')`,
		`INSERT INTO t(docid,a) VALUES(5,'beta')`,
		`INSERT INTO t(docid,a) VALUES(9,'gamma')`,
		`COMMIT`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
	}},
	{"insert-then-delete", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(docid,a) VALUES(1,'alpha'),(2,'beta')`,
		`BEGIN`,
		`INSERT INTO t(docid,a) VALUES(3,'gamma')`,
		`DELETE FROM t WHERE docid=1`,
		`COMMIT`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		`SELECT docid FROM t WHERE t MATCH 'alpha'`,
		`SELECT docid FROM t WHERE t MATCH 'gamma'`,
	}},
	{"rollback-discards", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('alpha')`,
		`BEGIN`, `INSERT INTO t VALUES('beta')`, `ROLLBACK`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		`SELECT docid, a FROM t ORDER BY docid`,
		`SELECT count(*) FROM t WHERE t MATCH 'beta'`,
	}},
	{"updates-in-a-transaction", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(docid,a) VALUES(1,'alpha'),(2,'beta')`,
		`BEGIN`,
		`UPDATE t SET a='delta' WHERE docid=1`,
		`UPDATE t SET a='epsilon' WHERE docid=2`,
		`COMMIT`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		`SELECT docid,a FROM t ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'delta'`,
	}},
	{"autocommit-still-one-segment-each", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t VALUES('alpha')`,
		`INSERT INTO t VALUES('beta')`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
	}},
	{"fts3-too", []string{
		`CREATE VIRTUAL TABLE t USING fts3(a)`,
		`BEGIN`, `INSERT INTO t VALUES('alpha')`, `INSERT INTO t VALUES('beta')`, `COMMIT`,
		`SELECT level, idx, start_block, leaves_end_block, end_block FROM t_segdir ORDER BY level, idx`,
		`SELECT docid FROM t WHERE t MATCH 'beta'`,
	}},
}

func TestFts3TransactionSegmentsMatchCSQLite(t *testing.T) {
	for _, tc := range fts3TxnCases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "fts3txn/"+tc.name, tc.stmts) })
	}
}
