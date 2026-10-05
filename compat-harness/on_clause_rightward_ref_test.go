// ON clause rightward reference rule: INNER joins can reference later tables unless RIGHT/FULL present.
package compat

import "testing"

// INNER join ON can reference later table; no RIGHT/FULL join.
func TestOnClauseInnerRightwardAccepted(t *testing.T) {
	differ(t, "on_rightward_inner_accepted", []string{
		`CREATE TABLE t0(w)`,
		`CREATE TABLE t1(v)`,
		`CREATE TABLE t2(x,y)`,
		`INSERT INTO t0 VALUES(1)`,
		`INSERT INTO t1 VALUES(2)`,
		`INSERT INTO t2 VALUES(3,4)`,
		`SELECT * FROM t0 JOIN t1 ON (t2.x NOTNULL) LEFT JOIN t2 ON 0`,
	})
}

// OUTER join ON cannot reference later table; unconditional decline.
func TestOnClauseOuterRightwardStaysDeclined(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t0(w)`, `CREATE TABLE t1(v)`, `CREATE TABLE t2(x,y)`,
		`SELECT * FROM t0 LEFT JOIN t1 ON (t2.x NOTNULL) JOIN t2 ON 1`,
	}
	if run(t, "cgo", stmts)[len(stmts)-1]["kind"] != "error" {
		t.Fatal("test premise wrong: oracle did not reject this")
	}
	if got := run(t, "musql", stmts)[len(stmts)-1]["kind"]; got != "error" {
		t.Errorf("expected decline for OUTER ON rightward reference, got kind=%v", got)
	}
}

// TestOnClauseInnerRightwardWithRightJoinStaysDeclined: an INNER join's ON
// referencing a table to its right must still fail when the FROM clause
// contains a RIGHT/FULL join anywhere (hasRightJoin).
func TestOnClauseInnerRightwardWithRightJoinStaysDeclined(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t0(w)`, `CREATE TABLE t1(v)`, `CREATE TABLE t2(x,y)`, `CREATE TABLE t3(z)`,
		`SELECT * FROM t0 JOIN t1 ON (t2.x NOTNULL) RIGHT JOIN t2 ON 1 JOIN t3 ON 1`,
	}
	if run(t, "cgo", stmts)[len(stmts)-1]["kind"] != "error" {
		t.Fatal("test premise wrong: oracle did not reject this")
	}
	if got := run(t, "musql", stmts)[len(stmts)-1]["kind"]; got != "error" {
		t.Errorf("expected decline for INNER ON rightward reference with a RIGHT join elsewhere, got kind=%v", got)
	}
}

// TestOnClauseUpdateFromTargetRefStaysDeclined: UPDATE...FROM's own target
// table is appended as a synthetic LAST source (update_from.go); C SQLite
// detaches it from the synthetic SELECT's SrcList before resolving names
// (update.c:222-230), so a FROM-clause ON referencing it never resolves,
// regardless of join type -- this must stay declined even for a plain INNER
// join, which is otherwise now accepted for an ordinary FROM peer.
func TestOnClauseUpdateFromTargetRefStaysDeclined(t *testing.T) {
	differ(t, "on_rightward_updatefrom_inner_target", []string{
		`CREATE TABLE t5(a INTEGER PRIMARY KEY, b TEXT, c TEXT)`,
		`CREATE TABLE m1(x INTEGER PRIMARY KEY, y TEXT)`,
		`CREATE TABLE m2(u INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t5 VALUES(1,'one','ONE')`,
		`INSERT INTO m1 VALUES(1,'i')`,
		`INSERT INTO m2 VALUES(1,'I')`,
		`UPDATE t5 SET b=y, c=v FROM m1 JOIN m2 ON (u=t5.a) WHERE x=a`,
	})
}

// TestOnClauseInnerRightwardSolverDeclinedStillAccepted: the same rightward
// rule as TestOnClauseInnerRightwardAccepted, but on a FROM clause the
// ported cost-based solver (wherePlanMultiTableOrder) itself declines --
// here because "x IN tbl" (a bare-table IN, sugar for "IN (SELECT * FROM
// tbl)") trips wherePlanHasRangeRewrite, so wherePlanMultiTableOrder's own
// ok=false. joinSource.onToWhere used to be set ONLY on the solver-succeeded
// path, so any FROM clause the solver declined for an unrelated reason fell
// back to enforcing left-to-right-only ON scoping regardless of join type --
// wrongly rejecting a query C SQLite runs. Mined from
// compat-harness/testdata/tcl/distinct2.test's own 120: a 5-way self-join
// CREATE TABLE t2 AS SELECT ... whose second JOIN's ON references the alias
// "t2" introduced by the FIFTH join term, and whose destination table is
// ALSO named t2 -- confirming (per build.c's sqlite3EndTable/AS SELECT path)
// that the CTAS destination name has zero visibility during its own SELECT;
// "t2.i0" resolves purely against the FROM clause's own alias.
func TestOnClauseInnerRightwardSolverDeclinedStillAccepted(t *testing.T) {
	differ(t, "distinct2.test 120: CTAS t2 with FROM-clause alias also named t2", []string{
		`CREATE TABLE t102 (i0 TEXT UNIQUE NOT NULL)`,
		`INSERT INTO t102 VALUES ('0'),('1'),('2')`,
		`CREATE TABLE t2 AS
		 SELECT DISTINCT *
		 FROM t102 AS t0
		 JOIN t102 AS t4 ON (t2.i0 IN t102)
		 NATURAL JOIN t102 AS t3
		 JOIN t102 AS t1 ON (t0.i0 IN t102)
		 JOIN t102 AS t2 ON (t2.i0=+t0.i0 OR (t0.i0<>500 AND t2.i0=t1.i0))`,
		`SELECT *, '|' FROM t2 ORDER BY 1, 2, 3, 4, 5`,
	})
}

// TestOnClauseGroupPlusOnToWhereNotDropped: a parenthesized join group
// coexisting with an ordinary onToWhere-eligible INNER ON elsewhere in the
// SAME FROM clause -- guards against silently dropping the moved ON
// conjunct (buildJoinPlan's groupPresent branch only ever sees `where`, not
// per-source ON clauses; this is safe only because onToWhere is never set
// when a group is present -- wherePlanMultiTableSources excludes groups
// outright -- but this pins the observable behavior directly rather than
// relying on that invariant holding forever).
func TestOnClauseGroupPlusOnToWhereNotDropped(t *testing.T) {
	differ(t, "on_group_plus_onto_where", []string{
		`CREATE TABLE t0(a)`,
		`CREATE TABLE t1(b)`,
		`CREATE TABLE t2(c)`,
		`CREATE TABLE t3(d)`,
		`INSERT INTO t0 VALUES(1),(2)`,
		`INSERT INTO t1 VALUES(1),(3)`,
		`INSERT INTO t2 VALUES(1)`,
		`INSERT INTO t3 VALUES(1)`,
		`SELECT * FROM t0 JOIN t1 ON a=b JOIN (t2 JOIN t3 ON c=d)`,
	})
}
