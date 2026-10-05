// Tests INSERT ... RETURNING against writable virtual tables (rtree).
// Virtual tables use separate vtabReturningPlan/captureVtabRow machinery
// rather than the base-table RETURNING framework.
// supports the identical shape too, the same way (a fresh per-row snapshot
// pager) -- see returning_trigger_subquery_test.go's TestReturningSubquery.
// Verified directly against mattn/go-sqlite3 3.53.3 for the timing question
// that makes this subtle: a RETURNING subquery reading the SAME target vtab
// (self-referential) sees the LIVE, incrementally-updated state as of each
// row -- not a single pre-statement snapshot -- so a multi-row VALUES
// statement's subquery answer is DIFFERENT for its second row than its
// first. captureVtabRow builds a FRESH snapshot pager per row (only when the
// plan actually has a subquery) to reproduce that; a single whole-statement
// snapshot (the pattern engine/vdbe_write.go's writeSubqueryPager uses for an
// ordinary write's WHERE/SET) would have been WRONG here.
//
// Still declined: RETURNING inside a trigger body (mirrors prepareReturning's
// identical rule for an ordinary table) and an INSERT ... SELECT source
// combined with RETURNING against a vtab (the per-row capture is wired into
// the VALUES-sourced loop only).
package compat

import "testing"

func TestVtabInsertReturning(t *testing.T) {
	seed := []string{
		`CREATE VIRTUAL TABLE t1 USING rtree(a,b,c)`,
		`CREATE TABLE t2(b)`,
		`INSERT INTO t2 VALUES(99)`,
	}
	// The mined shape: a RETURNING subquery over an UNRELATED table.
	differ(t, "returning an unrelated subquery", append(append([]string{}, seed...),
		`INSERT INTO t1(a,b,c) VALUES(1,2,3) RETURNING (SELECT b FROM t2)`,
		`SELECT a,b,c FROM t1`,
	))
	// RETURNING the vtab's own columns, and RETURNING *.
	differ(t, "returning own columns and star", append(append([]string{}, seed...),
		`INSERT INTO t1(a,b,c) VALUES(2,4,5) RETURNING a,b,c`,
		`INSERT INTO t1(a,b,c) VALUES(3,6,7) RETURNING *`,
		`SELECT a,b,c FROM t1`,
	))
	// A multi-row VALUES insert, RETURNING an expression over each row.
	differ(t, "multi-row returning an expression", append(append([]string{}, seed...),
		`INSERT INTO t1(a,b,c) VALUES(10,20,30),(11,21,31) RETURNING a, b+c`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
	))
	// A RETURNING subquery reading the SAME target vtab: LIVE, per-row state,
	// not a frozen pre-statement snapshot -- the single-row case sees only
	// the pre-existing row, and the multi-row case's second row sees the
	// first row's own just-inserted state too.
	differ(t, "returning a self-referential subquery, single row", append(append([]string{}, seed...),
		`INSERT INTO t1(a,b,c) VALUES(1,2,3)`,
		`INSERT INTO t1(a,b,c) VALUES(2,4,5) RETURNING (SELECT count(*) FROM t1)`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
	))
	differ(t, "returning a self-referential subquery, multi row", append(append([]string{}, seed...),
		`INSERT INTO t1(a,b,c) VALUES(1,2,3)`,
		`INSERT INTO t1(a,b,c) VALUES(3,6,7),(4,8,9) RETURNING (SELECT count(*) FROM t1)`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
	))

	// Still declined: an INSERT ... SELECT source combined with RETURNING.
	res := run(t, "musql", []string{
		`CREATE VIRTUAL TABLE t1 USING rtree(a,b,c)`,
		`CREATE TABLE src(a,b,c)`,
		`INSERT INTO src VALUES(1,2,3)`,
		`INSERT INTO t1(a,b,c) SELECT a,b,c FROM src RETURNING *`,
	})
	if res[3]["kind"] != "error" {
		t.Errorf("expected vtab INSERT...SELECT...RETURNING to still decline, got %v", res[3])
	}
	// RETURNING inside a trigger body targeting a vtab: C SQLite (and
	// musql, already, unrelated to this task) rejects RETURNING in ANY
	// trigger body at CREATE TRIGGER time -- "cannot use RETURNING in a
	// trigger" -- regardless of the body statement's target table kind, so
	// insertIntoVtab's own outer!=nil guard (mirroring prepareReturning's
	// identical one for an ordinary table) can never actually be reached;
	// verified directly against 3.53.3 that the CREATE TRIGGER itself is
	// what fails, not a later firing.
	res = run(t, "musql", []string{
		`CREATE VIRTUAL TABLE t1 USING rtree(a,b,c)`,
		`CREATE TABLE t3(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t3 BEGIN INSERT INTO t1(a,b,c) VALUES(new.x,new.x,new.x) RETURNING a; END`,
	})
	if res[2]["kind"] != "error" {
		t.Errorf("expected CREATE TRIGGER with a RETURNING body statement (vtab target) to decline, got %v", res[2])
	}
}
