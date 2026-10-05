package compat

import (
	"fmt"
	"testing"
)

// TestStatementsAfterASQLTextTransaction: a connection keeps one session across
// transactions, and a SQL-text BEGIN/SAVEPOINT marks it as held by the driver.
// Nothing cleared that at COMMIT or ROLLBACK, so every later autocommit
// statement on the connection still looked transactional: VACUUM answered
// "cannot VACUUM from within a transaction" where C runs it.
func TestStatementsAfterASQLTextTransaction(t *testing.T) {
	for i, end := range [][]string{
		{"BEGIN", "INSERT INTO t VALUES(1)", "COMMIT"},
		{"BEGIN", "INSERT INTO t VALUES(1)", "ROLLBACK"},
		{"SAVEPOINT s", "INSERT INTO t VALUES(1)", "RELEASE s"},
		{"SAVEPOINT s", "INSERT INTO t VALUES(1)", "ROLLBACK"},
	} {
		for j, after := range [][]string{
			{"VACUUM", "SELECT count(*) FROM t"},
			{"PRAGMA foreign_keys=ON", "INSERT INTO c VALUES(99)", "SELECT count(*) FROM c"},
			{"PRAGMA foreign_keys=ON", "CREATE TABLE d(x REFERENCES p DEFERRABLE INITIALLY DEFERRED)", "INSERT INTO d VALUES(99)", "SELECT count(*) FROM d"},
			{"BEGIN", "COMMIT", "VACUUM"},
		} {
			st := append([]string{"CREATE TABLE t(a)", "CREATE TABLE p(k INTEGER PRIMARY KEY)", "CREATE TABLE c(x REFERENCES p)"}, end...)
			differ(t, fmt.Sprintf("end %d / after %d", i, j), append(st, after...))
		}
	}
}
