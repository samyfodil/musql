package compat

import "testing"

// TestDeleteAllThenInsertAsC: #392's sequence -- a filtered DELETE, a DELETE
// of every row, then an INSERT of as many again -- assigns the rowids C
// assigns, with and without a ROLLBACK that has no transaction to end.
func TestDeleteAllThenInsertAsC(t *testing.T) {
	differ(t, "delete-all-then-insert", []string{
		`CREATE TABLE t(a TEXT, b TEXT)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 20000) INSERT INTO t SELECT '2002-04-02', v FROM g`,
		`DELETE FROM t WHERE b > '12300'`,
		`SELECT count(*), min(rowid), max(rowid) FROM t`,
		`ROLLBACK`,
		`DELETE FROM t`,
		`SELECT count(*), min(rowid), max(rowid) FROM t`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 20000) INSERT INTO t SELECT '2002-04-02', v FROM g`,
		`SELECT count(*), min(rowid), max(rowid), count(DISTINCT rowid) FROM t`,
		`INSERT INTO t VALUES('x', 'y')`,
		`SELECT max(rowid), last_insert_rowid() FROM t`,
		`DELETE FROM t WHERE rowid > 5`,
		`DELETE FROM t`,
		`INSERT INTO t VALUES('x', 'y')`,
		`SELECT rowid, a, b FROM t`,
	})
}
