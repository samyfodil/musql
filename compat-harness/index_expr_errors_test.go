package compat

import (
	"fmt"
	"testing"
)

// TestIndexExpressionErrorsOnOurFormat: C computes every index's key as it
// writes a row, so an error in an index expression or partial-index WHERE fails
// the write -- sqlite3NotPureFunc's "non-deterministic use of julianday() in an
// index" (vdbeaux.c:5627), an abs() overflow. Our format stores no index data,
// so nothing evaluated a non-UNIQUE index's expressions and the write succeeded.
func TestIndexExpressionErrorsOnOurFormat(t *testing.T) {
	for i, st := range [][]string{
		{"CREATE TABLE t(a)", "CREATE INDEX i ON t(julianday('now'))", "INSERT INTO t VALUES(1)", "SELECT count(*) FROM t"},
		{"CREATE TABLE t(a)", "CREATE INDEX i ON t(a) WHERE a > 5 AND date() > '2000'", "INSERT INTO t VALUES(1)", "INSERT INTO t VALUES(9)", "SELECT count(*) FROM t"},
		{"CREATE TABLE t(a)", "CREATE INDEX i ON t(date(a))", "INSERT INTO t VALUES('2020-01-01')", "SELECT count(*) FROM t"},
		{"CREATE TABLE t(a)", "INSERT INTO t VALUES('2020-01-01')", "CREATE INDEX i ON t(date(a, 'localtime'))", "SELECT count(*) FROM t"},
		{"CREATE TABLE t(a)", "CREATE INDEX i ON t(date(a))", "INSERT INTO t VALUES('now')", "SELECT count(*) FROM t"},
		{"CREATE TABLE t(a)", "INSERT INTO t VALUES('x')", "CREATE INDEX i ON t(date(a))", "UPDATE t SET a='now'", "SELECT a FROM t"},
		{"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)", "CREATE INDEX i ON t(abs(a))", "INSERT INTO t VALUES(-9223372036854775808)", "SELECT count(*) FROM t"},
		{"CREATE TABLE t(a INTEGER PRIMARY KEY, b)", "CREATE INDEX i ON t(date(b))", "INSERT INTO t VALUES(1,'x') ON CONFLICT DO NOTHING", "INSERT INTO t VALUES(1,'now') ON CONFLICT(a) DO UPDATE SET b='now'", "SELECT * FROM t"},
	} {
		differ(t, fmt.Sprint(i), st)
	}
}
