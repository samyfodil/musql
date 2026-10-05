// This file tests nQueryLoop propagation: nested query row estimates in WHERE clauses.
package compat

import "testing"

// TestNQueryLoopWhereClauseTwoLevelsDeep tests correlated subqueries nested
// two levels in WHERE clauses.
func TestNQueryLoopWhereClauseTwoLevelsDeep(t *testing.T) {
	differ(t, "nqueryloop_where_two_levels", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`SELECT a FROM t1 WHERE a IN (
			SELECT mid.a FROM t1 AS mid
			WHERE mid.b = (SELECT x FROM t2 WHERE y = mid.b*10 LIMIT 1) - 20
		)`,
	})
}

// TestNQueryLoopWhereClauseTwoLevelsDeepDistinct tests the same with DISTINCT on the outer query.
func TestNQueryLoopWhereClauseTwoLevelsDeepDistinct(t *testing.T) {
	differ(t, "nqueryloop_where_two_levels_distinct", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7),(6,2),(7,9)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`SELECT DISTINCT b FROM t1 WHERE b IN (
			SELECT mid.b FROM t1 AS mid
			WHERE mid.a = (SELECT x FROM t2 WHERE y = mid.b*10 LIMIT 1) / 10
		)`,
	})
}

// TestNQueryLoopHavingUnderAggregateStaysSafe is the real asymmetry the
// design explicitly does NOT resolve this pass: the same two-level nested
// subquery shape, but reached from an AGGREGATE query's own HAVING clause.
// compileScanAggregate/compileScanGroupBy clear nQueryLoopKnown
// unconditionally (see their own doc comments), so this must still answer
// correctly via whatever path it always used -- declining the ported
// planner for the nested subquery's own scan order exactly like before this
// pass, not a NEW acceptance that could get the seed wrong.
func TestNQueryLoopHavingUnderAggregateStaysSafe(t *testing.T) {
	differ(t, "nqueryloop_having_under_aggregate", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER, g INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5,1),(2,2,1),(3,9,2),(4,5,2),(5,7,1)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`SELECT g, sum(b) FROM t1 GROUP BY g
		 HAVING sum(b) > (
			SELECT mid.b FROM t1 AS mid
			WHERE mid.a = (SELECT x FROM t2 WHERE y = mid.b*10 LIMIT 1) / 10
		 )`,
	})
}

// TestNQueryLoopSelectListUnderAggregateStaysSafe is the OTHER half of the
// same asymmetry: the identical nested-subquery shape, sitting in an
// aggregate query's own SELECT LIST (evaluated at OpAggResult time, same
// post-sqlite3WhereEnd finalize phase HAVING uses) rather than its HAVING.
func TestNQueryLoopSelectListUnderAggregateStaysSafe(t *testing.T) {
	differ(t, "nqueryloop_selectlist_under_aggregate", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER, g INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5,1),(2,2,1),(3,9,2),(4,5,2),(5,7,1)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`SELECT g, sum(b), (
			SELECT mid.a FROM t1 AS mid
			WHERE mid.b = (SELECT x FROM t2 WHERE y = mid.b*10 LIMIT 1) - 20
		 ) FROM t1 GROUP BY g`,
	})
}

// TestNQueryLoopTriggerBodyStaysSafe: the same nested-subquery shape reached
// from inside a TRIGGER body. vdbe_trigger.go's own root compiler is built
// with no outer at all (c.trig set, c.outer nil), so nQueryLoopKnown starts
// at its Go zero value (false) there and this must keep declining the
// ported planner for the nested subquery's own order exactly as before.
func TestNQueryLoopTriggerBodyStaysSafe(t *testing.T) {
	differ(t, "nqueryloop_trigger_body", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE TABLE log1(v INTEGER)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`CREATE INDEX i2y ON t2(y)`,
		`CREATE TRIGGER tr1 AFTER INSERT ON log1 BEGIN
			INSERT INTO log1(rowid,v) SELECT NULL, mid.a FROM t1 AS mid
				WHERE mid.b = (SELECT x FROM t2 WHERE y = mid.b*10 LIMIT 1) - 20
				LIMIT 1;
		 END`,
		`INSERT INTO log1(v) VALUES(0)`,
		`SELECT v FROM log1 ORDER BY rowid`,
	})
}

// TestNQueryLoopCompoundArmStaysSafe: the same nested-subquery shape inside
// ONE arm of a compound SELECT. A compound arm is compiled by
// compileSubProgram exactly like any other subquery body, so this is really
// checking that compileSubProgramCompound's per-arm outer-threading composes
// correctly with the new fields rather than introducing a divergent arm.
func TestNQueryLoopCompoundArmStaysSafe(t *testing.T) {
	differ(t, "nqueryloop_compound_arm", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`SELECT a FROM t1 WHERE a IN (
			SELECT mid.a FROM t1 AS mid
			WHERE mid.b = (SELECT x FROM t2 WHERE y = mid.b*10 LIMIT 1) - 20
		 )
		 UNION
		 SELECT a FROM t1 WHERE a = 99`,
	})
}

// TestNQueryLoopOneLevelOrderByReversal is a narrower, one-hop-deep case
// aimed squarely at the interstage-heuristic/second-solve path
// (where_plan_index.go's plan(), whose second wherePathSolver pass reads the
// FIRST pass's own nRow, itself seeded from nQueryLoop): a correlated
// subquery whose own body has an ORDER BY over the indexed column, so the
// "sort vs already-in-order" tradeoff wherePathSolver's TUNING constants
// decide is exactly the thing a wrong nQueryLoop seed could flip.
func TestNQueryLoopOneLevelOrderByReversal(t *testing.T) {
	differ(t, "nqueryloop_orderby_one_level", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,1),(2,2),(3,3)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,55),(20,20),(50,15),(60,30),(40,52),(80,12),(90,25)`,
		`SELECT a, (
			SELECT group_concat(x) FROM (
				SELECT x FROM t2 WHERE y > a ORDER BY y DESC LIMIT 3
			)
		 ) FROM t1`,
	})
}
