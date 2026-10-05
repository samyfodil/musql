package compat

import "testing"

// VACUUM renumbers a table with no INTEGER PRIMARY KEY and no index
// (insert.c:3345, OP_NewRowid on the transfer path), so the next INSERT's rowid
// is one past the NEW maximum, not the one the table held before the VACUUM.
func TestVacuumRenumberMovesNextRowid(t *testing.T) {
	differ(t, "vacuum renumber next rowid", []string{
		"CREATE TABLE t(a)",
		"WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<10) INSERT INTO t SELECT i FROM c",
		"SELECT max(rowid) FROM t",
		"DELETE FROM t WHERE rowid<=5",
		"VACUUM",
		"INSERT INTO t VALUES('new')",
		"SELECT rowid, a FROM t ORDER BY rowid",
	})
}
