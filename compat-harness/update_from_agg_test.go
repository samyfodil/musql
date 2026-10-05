// Tests aggregate functions in UPDATE ... FROM SET expressions.
// The SET expression's aggregate is computed over the joined rows of both
// the target table and the FROM clause, and only one target row is updated.
package compat

import "testing"

func TestUpdateFromAggregateSetExpr(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// The mined statement itself (upfrom1.test 3.1): both tables empty, so
		// the aggregate's single collapsed output row has a NULL leading key
		// (nothing to copy from) -- 0 rows affected, no error.
		{"mined-empty-tables", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(b)",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t0, t1",
		}},
		// A single target row: unambiguous, and the value that actually
		// exercises the aggregate (sum(1,2,3)=6).
		{"single-target-row-nonempty", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(b)",
			"INSERT INTO t1 VALUES(NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1",
		}},
		// WHERE scoping: the aggregate only sees rows the UPDATE's own WHERE
		// admits (update.c:223's pWhere2 == pWhere, folded into the synthetic
		// SELECT, not a post-filter) -- sum(2,3)=5, not sum(1,2,3)=6.
		{"where-scopes-the-aggregate", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(b)",
			"INSERT INTO t1 VALUES(NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0 WHERE a>1",
			"SELECT * FROM t1",
		}},
		// WHERE filters away every row of a NON-empty target: same "0 rows
		// affected, no error" outcome as the empty-tables case, this time via
		// an aggregate group that is empty because WHERE rejected everything.
		{"where-filters-everything-nonempty-target", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(b)",
			"INSERT INTO t1 VALUES(999)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0 WHERE a>100",
			"SELECT * FROM t1",
		}},
		// A bare (non-aggregate) FROM column mixed with an aggregate in
		// different SET items -- SQLite's "bare columns in an aggregate
		// query" extension, not a mixing error, exactly like an ordinary
		// "SELECT a, sum(b) FROM t" would be.
		{"mixed-aggregate-and-bare-column", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(b,c)",
			"INSERT INTO t1 VALUES(NULL,NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a), c=a FROM t0",
			"SELECT * FROM t1",
		}},
		{"count-star", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(b)",
			"INSERT INTO t1 VALUES(NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=count(*) FROM t0",
			"SELECT * FROM t1",
		}},
		// Multiple candidate target rows: only ONE is updated (see this
		// file's doc comment), with sum computed over the WHOLE cross join
		// (3 target rows x 3 FROM rows = 9, sum=18, not 6). Run plain and
		// under two index shapes that don't perturb the (no-min/max ->
		// first-row) anchor, to catch a silently-wrong value if the fix ever
		// regresses to depending on scan order for this simple case.
		{"multi-target-rows-plain", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(id INTEGER PRIMARY KEY, b)",
			"INSERT INTO t1 VALUES(1,NULL),(2,NULL),(3,NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1 ORDER BY id",
		}},
		{"multi-target-rows-indexed-target", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(id INTEGER PRIMARY KEY, b)",
			"CREATE INDEX ix1 ON t1(id)",
			"INSERT INTO t1 VALUES(1,NULL),(2,NULL),(3,NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1 ORDER BY id",
		}},
		{"multi-target-rows-indexed-from", []string{
			"CREATE TABLE t0(a)",
			"CREATE INDEX ix0 ON t0(a)",
			"CREATE TABLE t1(id INTEGER PRIMARY KEY, b)",
			"INSERT INTO t1 VALUES(1,NULL),(2,NULL),(3,NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1 ORDER BY id",
		}},
		{"multi-target-rows-scrambled-insert-no-pk", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(tag TEXT, b)",
			"INSERT INTO t1 VALUES('z',NULL),('m',NULL),('a',NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1 ORDER BY rowid",
		}},
		{"multi-target-rows-indexed-no-analyze", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(tag TEXT, b)",
			"CREATE INDEX ixt ON t1(tag)",
			"INSERT INTO t1 VALUES('z',NULL),('m',NULL),('a',NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1 ORDER BY rowid",
		}},
		{"large-from-tiny-target", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(id INTEGER PRIMARY KEY, b)",
			"INSERT INTO t1 VALUES(1,NULL),(2,NULL)",
			"WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<500) INSERT INTO t0 SELECT n FROM c",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1 ORDER BY id",
		}},
		// An empty target with a non-empty FROM: 0 rows affected (nothing to
		// key the update by), and changes() must agree it touched nothing.
		{"empty-target-nonempty-from", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(b)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1",
			"SELECT changes()",
		}},
		// The existing explicit scalar-subquery spelling must keep working
		// unchanged (it was never routed through checkExprSupported's SET-list
		// check at all -- this just guards against a regression).
		{"scalar-subquery-spelling-unaffected", []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(b)",
			"INSERT INTO t1 VALUES(NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			"UPDATE t1 SET b=(SELECT sum(a) FROM t0)",
			"SELECT * FROM t1",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}

// TestUpdateFromAggregateAnchorOrderFollowsThePlan: a bare-aggregate UPDATE ...
// FROM whose "which candidate target row wins" follows the query PLAN -- a
// covering index plus ANALYZE statistics let C SQLite scan the target through
// the index, alphabetical tag order, instead of rowid order, which moves the
// row the no-min/max anchor rule lands on. It used to decline, because the
// ported planner did not decide that scan; it does now, so the answer is
// asserted, with and without the statistics that flip it.
func TestUpdateFromAggregateAnchorOrderFollowsThePlan(t *testing.T) {
	for _, stat := range []string{"ANALYZE", "SELECT 1"} {
		differ(t, "update-from-anchor "+stat, []string{
			"CREATE TABLE t0(a)",
			"CREATE TABLE t1(tag TEXT, b)",
			"CREATE INDEX ixt ON t1(tag)",
			"INSERT INTO t1 VALUES('z',NULL),('m',NULL),('a',NULL)",
			"INSERT INTO t0 VALUES(1),(2),(3)",
			stat,
			"UPDATE t1 SET b=sum(a) FROM t0",
			"SELECT * FROM t1 ORDER BY rowid",
		})
	}
}
