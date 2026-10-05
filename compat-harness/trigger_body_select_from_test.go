// FROM-bearing SELECT statements in non-INSERT trigger bodies, with ORDER BY
// or derived-table subqueries. ORDER BY is discarded by C SQLite; bad ORDER BY
// references are silently ignored, not errors.
package compat

import "testing"

func TestTriggerBodySelectFromOrderBy(t *testing.T) {
	differ(t, "DELETE trigger body: FROM-bearing SELECT with ORDER BY, fires", []string{
		`CREATE TABLE t1(x, y)`,
		`CREATE TRIGGER Trigger1 DELETE ON t1 BEGIN SELECT t1.*, t1.x FROM t1 ORDER BY t1.x; END`,
		`INSERT INTO t1 VALUES(1,2)`,
		`INSERT INTO t1 VALUES(3,4)`,
		`DELETE FROM t1 WHERE x=1`,
		`SELECT * FROM t1`,
	})
	differ(t, "DELETE trigger body: FROM-bearing SELECT with ORDER BY, zero rows", []string{
		`CREATE TABLE t1(x, y)`,
		`CREATE TRIGGER Trigger1 DELETE ON t1 BEGIN SELECT t1.*, t1.x FROM t1 ORDER BY t1.x; END`,
		`INSERT INTO t1 VALUES(1,2)`,
		`DELETE FROM t1 WHERE x=99`,
		`SELECT * FROM t1`,
	})
	// ALTER TABLE RENAME COLUMN/TO must rewrite the trigger's stored SQL AND
	// keep it firing, exactly like altertab3.test 29.1-29.7.
	differ(t, "ALTER TABLE RENAME rewrites an ORDER-BY trigger body and it keeps firing", []string{
		`CREATE TABLE t1(x, y)`,
		`CREATE TRIGGER Trigger1 DELETE ON t1 BEGIN SELECT t1.*, t1.x FROM t1 ORDER BY t1.x; END`,
		`ALTER TABLE t1 RENAME x TO z`,
		`ALTER TABLE t1 RENAME TO t2`,
		`INSERT INTO t2 VALUES(9,9)`,
		`DELETE FROM t2`,
		`SELECT * FROM t2`,
	})
}

// TestTriggerBodySelectOrderByBadColumnIsIgnored pins the trap described in
// this file's doc comment: an ORDER BY referencing a column that does not
// exist at all must NOT error, either for a zero-row firing (where the eager,
// zero-row-safe validation pass never even looks at ORDER BY) or for a real
// one (where the body SELECT's ORDER BY is stripped before it runs).
func TestTriggerBodySelectOrderByBadColumnIsIgnored(t *testing.T) {
	differ(t, "bad ORDER BY column, trigger fires against a real row", []string{
		`CREATE TABLE t1(x, y)`,
		`CREATE TRIGGER trbad2 DELETE ON t1 BEGIN SELECT x FROM t1 ORDER BY nosuchcol; END`,
		`INSERT INTO t1 VALUES(1,2)`,
		`DELETE FROM t1 WHERE x=1`,
		`SELECT * FROM t1`,
	})
	differ(t, "bad ORDER BY column, zero-row DELETE", []string{
		`CREATE TABLE t1(x, y)`,
		`CREATE TRIGGER trbad2 DELETE ON t1 BEGIN SELECT x FROM t1 ORDER BY nosuchcol; END`,
		`DELETE FROM t1 WHERE 0`,
	})
}

// TestTriggerBodySelectFromDerivedSubquery covers the accepted derived-table
// half (altertab3.test 29.4's exact shape).
func TestTriggerBodySelectFromDerivedSubquery(t *testing.T) {
	differ(t, "AFTER DELETE trigger body: FROM a bare-projection derived table, fires", []string{
		`CREATE TABLE t2(z, y)`,
		`CREATE TRIGGER tr2 AFTER DELETE ON t2 BEGIN SELECT z, y FROM (SELECT t2.* FROM t2); END`,
		`INSERT INTO t2 VALUES(9,9)`,
		`DELETE FROM t2`,
		`SELECT * FROM t2`,
	})
	differ(t, "AFTER DELETE trigger body: FROM a bare-projection derived table, zero rows", []string{
		`CREATE TABLE t2(z, y)`,
		`CREATE TRIGGER tr2 AFTER DELETE ON t2 BEGIN SELECT z, y FROM (SELECT t2.* FROM t2); END`,
		`INSERT INTO t2 VALUES(9,9)`,
		`DELETE FROM t2 WHERE z=0`,
		`SELECT * FROM t2`,
	})
	differ(t, "ALTER TABLE RENAME rewrites a derived-subquery trigger body and it keeps firing", []string{
		`CREATE TABLE t1(x, y)`,
		`CREATE TRIGGER Trigger1 DELETE ON t1 BEGIN SELECT t1.*, t1.x FROM t1 ORDER BY t1.x; END`,
		`ALTER TABLE t1 RENAME x TO z`,
		`ALTER TABLE t1 RENAME TO t2`,
		`CREATE TRIGGER tr2 AFTER DELETE ON t2 BEGIN SELECT z, y FROM (SELECT t2.* FROM t2); END`,
		`DELETE FROM t2`,
		`ALTER TABLE t2 RENAME TO t3`,
		`INSERT INTO t3 VALUES(9,9)`,
		`DELETE FROM t3`,
		`SELECT * FROM t3`,
	})
}

// TestTriggerBodySelectFromDerivedSubqueryBadReferenceStillErrors is the
// load-bearing negative half for the derived-subquery shape specifically: a
// bad table/column reference buried inside the ACCEPTED "SELECT <cols> FROM
// (SELECT <cols> FROM <table>)" shape must still surface an error -- both
// engines resolve a trigger body LAZILY (C SQLite at the firing
// statement's own prepare, this engine via validateTriggerExprsOnce running
// at the same moment), so CREATE TRIGGER itself succeeds in both and the
// error appears once something tries to fire the trigger -- even a ZERO-ROW
// DELETE, matching this whole family's zero-row-safety property (only the
// "kind" of each result is compat-relevant -- see worker/main.go -- so the
// exact wording, "no such column: t2.nosuchcol" vs "no such column:
// nosuchcol", is not).
func TestTriggerBodySelectFromDerivedSubqueryBadReferenceStillErrors(t *testing.T) {
	differ(t, "derived subquery: bad column, zero-row firing", []string{
		`CREATE TABLE t2(a, b)`,
		`CREATE TRIGGER trbad DELETE ON t2 BEGIN SELECT a FROM (SELECT t2.nosuchcol FROM t2); END`,
		`DELETE FROM t2 WHERE 0`,
	})
	differ(t, "derived subquery: bad table, zero-row firing", []string{
		`CREATE TABLE t2(a, b)`,
		`CREATE TRIGGER trbad DELETE ON t2 BEGIN SELECT a FROM (SELECT * FROM nosuchtable); END`,
		`DELETE FROM t2 WHERE 0`,
	})
	differ(t, "outer ORDER BY: bad column, zero-row firing", []string{
		`CREATE TABLE t1(x, y)`,
		`CREATE TABLE t2(a, b)`,
		`CREATE TRIGGER trbad DELETE ON t1 BEGIN SELECT a FROM t2 ORDER BY nosuchcol2; END`,
		`DELETE FROM t1 WHERE 0`,
	})
}

// A derived subquery wider than a bare "SELECT <cols-or-star> FROM <table>"
// projection (a WHERE clause, a function call, a JOIN, further nesting) stays
// declined at CREATE TRIGGER time -- see isSchemaSafeDerivedSubquery's own doc
// comment for why, and engine's TestCreateTriggerDerivedSubqueryShapeGate for
// the gate itself. That is deliberately NOT tested here with differ(): real
// SQLite's CREATE TRIGGER is unconditionally lazy (verified directly, see the
// bad-reference case above -- it never rejects a body shape, only a bad
// reference once something actually fires the trigger), so it happily accepts
// all of those wider shapes, and this engine's earlier, shape-based decline is
// an intentional divergence from it (a decline, not a wrong answer -- see
// AGENTS.md's "decline, don't guess" contract), which differ() would only ever
// report as a mismatch.
