// This file tests subqueries inside trigger WHEN clauses against C SQLite.
package compat

import (
	"fmt"
	"testing"
)

func TestTriggerWhenSubqueryMatchesCSQLite(t *testing.T) {
	stmts := []string{
		`CREATE TABLE king(a, b, PRIMARY KEY(a))`,
		`CREATE TABLE prince(c, d)`,
		`CREATE TRIGGER kt AFTER INSERT ON prince WHEN
           NOT EXISTS (SELECT a FROM king WHERE a = new.c)
         BEGIN
           INSERT INTO king VALUES(new.c, NULL);
         END`,
		`INSERT INTO prince VALUES(1, 2)`,
		`SELECT a,b FROM king ORDER BY a`,
		// king already has a=1, so WHEN is false and the body must NOT run.
		`INSERT INTO prince VALUES(1, 9)`,
		`SELECT a,b FROM king ORDER BY a`,
		`INSERT INTO prince VALUES(2, 3)`,
		`SELECT a,b FROM king ORDER BY a`,
		`SELECT c,d FROM prince ORDER BY c,d`,
	}
	c := run(t, "cgo", stmts)
	m := run(t, "musql", stmts)
	if len(c) != len(m) {
		t.Fatalf("statement count: cgo=%d musql=%d", len(c), len(m))
	}
	for i := range c {
		if fmt.Sprint(c[i]) != fmt.Sprint(m[i]) {
			t.Errorf("stmt %d (%s) DIVERGES\n  cgo:    %v\n  musql: %v", i, stmts[i], c[i], m[i])
		}
	}
}

// TestTriggerWhenSubqueryDeclaredCollationMatchesCSQLite tests WHEN subqueries
// with declared column collations (OLD/NEW references with declared collation).
func TestTriggerWhenSubqueryDeclaredCollationMatchesCSQLite(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(x COLLATE NOCASE PRIMARY KEY)`,
		`CREATE TABLE t2(y)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tt1 AFTER DELETE ON t1
           WHEN EXISTS (SELECT 1 FROM t2 WHERE old.x = y)
         BEGIN INSERT INTO log VALUES(old.x); END`,
		`INSERT INTO t1 VALUES('A'),('B')`,
		`INSERT INTO t2 VALUES('a'),('b')`,
		`DELETE FROM t1`,
		`SELECT m FROM log ORDER BY m`,
	}
	c := run(t, "cgo", stmts)
	m := run(t, "musql", stmts)
	if len(c) != len(m) {
		t.Fatalf("statement count: cgo=%d musql=%d", len(c), len(m))
	}
	for i := range c {
		if fmt.Sprint(c[i]) != fmt.Sprint(m[i]) {
			t.Errorf("stmt %d (%s) DIVERGES\n  cgo:    %v\n  musql: %v", i, stmts[i], c[i], m[i])
		}
	}
}
