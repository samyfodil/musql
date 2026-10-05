// Tests for UNIQUE EXPRESSION and UNIQUE PARTIAL indexes in writes.
package compat

import "testing"

func TestUniqueExprIndexInsertParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX ux ON t(abs(a))`,
	}
	with := func(rest ...string) []string { return append(append([]string{}, setup...), rest...) }

	differ(t, "expr index: ABORT on a duplicate expression key", with(
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO t VALUES(-1,'minus one')`, // abs(-1)==abs(1): rejected
		`SELECT a,b FROM t ORDER BY a`,
		`SELECT changes() AS c`,
	))
	differ(t, "expr index: OR IGNORE skips the conflicting row", with(
		`INSERT INTO t VALUES(1,'one'),(2,'two')`,
		`INSERT OR IGNORE INTO t VALUES(-1,'skipped'),(3,'three')`,
		`SELECT a,b FROM t ORDER BY a`,
		`SELECT changes() AS c`,
	))
	differ(t, "expr index: OR REPLACE deletes the victim", with(
		`INSERT INTO t VALUES(1,'one'),(2,'two')`,
		`INSERT OR REPLACE INTO t VALUES(-1,'replaced')`,
		`SELECT a,b FROM t ORDER BY a`,
		`SELECT changes() AS c`,
	))
	differ(t, "expr index: a NULL key never conflicts", with(
		`INSERT INTO t VALUES(NULL,'n1')`,
		`INSERT INTO t VALUES(NULL,'n2')`,
		`SELECT a,b FROM t ORDER BY b`,
	))
	differ(t, "expr index: multi-row VALUES, the conflict is mid-tuple", with(
		`INSERT INTO t VALUES(5,'five')`,
		`INSERT INTO t VALUES(6,'six'),(-5,'boom'),(7,'seven')`,
		`SELECT a,b FROM t ORDER BY a`,
	))
	differ(t, "expr index: UPDATE into a conflicting key", with(
		`INSERT INTO t VALUES(1,'one'),(2,'two')`,
		`UPDATE t SET a=-1 WHERE b='two'`,
		`SELECT a,b FROM t ORDER BY b`,
		`UPDATE t SET a=9 WHERE b='two'`,
		`SELECT a,b FROM t ORDER BY b`,
	))
}

func TestUniquePartialIndexInsertParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX px ON t(a) WHERE b>0`,
	}
	with := func(rest ...string) []string { return append(append([]string{}, setup...), rest...) }

	differ(t, "partial index: rows outside the WHERE are not in the index", with(
		// validateUniqueIndex's own recorded oracle case: (1,1),(1,-1),(1,-2)
		// all accepted, then (1,5) is the violation.
		`INSERT INTO t VALUES(1,1)`,
		`INSERT INTO t VALUES(1,-1)`,
		`INSERT INTO t VALUES(1,-2)`,
		`INSERT INTO t VALUES(1,5)`,
		`SELECT a,b FROM t ORDER BY b`,
		`SELECT changes() AS c`,
	))
	differ(t, "partial index: OR IGNORE skips only the in-index duplicate", with(
		`INSERT INTO t VALUES(1,1)`,
		`INSERT OR IGNORE INTO t VALUES(1,2),(1,-3),(2,4)`,
		`SELECT a,b FROM t ORDER BY b`,
		`SELECT changes() AS c`,
	))
	differ(t, "partial index: OR REPLACE deletes the in-index victim", with(
		`INSERT INTO t VALUES(1,1),(1,-1)`,
		`INSERT OR REPLACE INTO t VALUES(1,7)`,
		`SELECT a,b FROM t ORDER BY b`,
		`SELECT changes() AS c`,
	))
	differ(t, "partial index: a NULL key never conflicts", with(
		`INSERT INTO t VALUES(NULL,1)`,
		`INSERT INTO t VALUES(NULL,2)`,
		`SELECT a,b FROM t ORDER BY b`,
	))
	differ(t, "partial index: UPDATE moving a row INTO the index conflicts", with(
		`INSERT INTO t VALUES(1,1),(1,-1)`,
		`UPDATE t SET b=3 WHERE b=-1`,
		`SELECT a,b FROM t ORDER BY b`,
		`UPDATE t SET a=2 WHERE b=-1`,
		`SELECT a,b FROM t ORDER BY b`,
	))
	differ(t, "partial index: DELETE frees the key again", with(
		`INSERT INTO t VALUES(1,1)`,
		`DELETE FROM t WHERE b=1`,
		`INSERT INTO t VALUES(1,2)`,
		`SELECT a,b FROM t ORDER BY b`,
	))
}
