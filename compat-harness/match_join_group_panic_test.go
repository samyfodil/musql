// This file gates MATCH expressions over FTS tables in join groups. colBase
// addressing is critical for proper cursor offset calculation in these cases.
package compat

import "testing"

// TestMatchOverJoinGroupBeforeVtabServesCorrectly tests groups before the vtab.
// order, correctly served (not panicking, not wrong).
func TestMatchOverJoinGroupBeforeVtabServesCorrectly(t *testing.T) {
	differ(t, "MATCH with a materialized join GROUP positioned before the vtab", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10(value) VALUES('apple'),('banana')`,
		`CREATE TABLE a(id INTEGER)`,
		`CREATE TABLE b(id INTEGER)`,
		`CREATE TABLE c(id INTEGER)`,
		`INSERT INTO a VALUES(1),(2)`,
		`INSERT INTO b VALUES(1)`,
		`INSERT INTO c VALUES(1),(2)`,
		`SELECT t10.value FROM (a LEFT JOIN b ON a.id=b.id), t10 WHERE t10 MATCH 'apple'`,
		`SELECT t10.value, a.id, b.id FROM (a LEFT JOIN b ON a.id=b.id), t10 WHERE t10 MATCH 'apple' ORDER BY a.id`,
		// A THREE-member group (colBase advancing twice), still before t10.
		`SELECT t10.value, a.id, b.id, c.id FROM (a LEFT JOIN b ON a.id=b.id LEFT JOIN c ON a.id=c.id), t10 WHERE t10 MATCH 'apple' ORDER BY a.id`,
	})
}

// TestMatchOverJoinGroupAfterVtabDeclinesCleanly is the narrower gap left
// open: a group positioned AFTER the vtab's own scope index answers wrong
// (not panics) even with colBase addressing fixed, so it stays a clean
// decline instead.
func TestMatchOverJoinGroupAfterVtabDeclinesCleanly(t *testing.T) {
	for _, q := range []string{
		`SELECT t10.value FROM t10, (a LEFT JOIN b ON a.id=b.id) WHERE t10 MATCH 'apple'`,
		`SELECT t10.value, a.id, b.id FROM t10, (a LEFT JOIN b ON a.id=b.id) WHERE t10 MATCH 'apple' ORDER BY a.id`,
	} {
		stmts := []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10(value) VALUES('apple'),('banana')`,
			`CREATE TABLE a(id INTEGER)`,
			`CREATE TABLE b(id INTEGER)`,
			`INSERT INTO a VALUES(1),(2)`,
			`INSERT INTO b VALUES(1)`,
			q,
		}
		oracle := run(t, "cgo", stmts)
		if oracle[len(oracle)-1]["kind"] == "error" {
			t.Fatalf("expected the oracle to succeed on %q, got: %v", q, oracle[len(oracle)-1])
		}
		res := run(t, "musql", stmts) // must not crash the worker process, and must not answer wrong
		if res[len(res)-1]["kind"] != "error" {
			t.Errorf("expected a clean decline (this shape is not yet reconciled) for %q, got: %v", q, res[len(res)-1])
		}
	}
}
