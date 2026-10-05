// Differential tests for UPDATE SET with subqueries over the target table.
// Covers various subquery shapes and self-reading SET in trigger bodies.
package compat

import "testing"

// TestUpdateSetSelfReadShapes tests compiled sub-program shapes that are
// uncorrelated and read the table being updated.
func TestUpdateSetSelfReadShapes(t *testing.T) {
	differ(t, "update set compound subquery over target", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`UPDATE t SET b=(SELECT count(*) FROM (SELECT a FROM t UNION ALL SELECT a FROM t))`,
		`SELECT a,b FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	// Through a VIEW: the body resolves to the target by a different route than
	// the name the decline keys on.
	differ(t, "update set subquery over target through a view", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE VIEW v AS SELECT * FROM t`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`UPDATE t SET b=(SELECT count(*) FROM v)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Through a leading WITH: the CTE reads the target, and the statement's own
	// WITH scope has to be pushed for the SET subquery to see it at all.
	differ(t, "update set subquery over target through a CTE", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`WITH c AS (SELECT count(*) AS n FROM t) UPDATE t SET b=(SELECT n FROM c)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// A CTE that SHADOWS a real table of the same name (select.c:6028: CTE
	// resolution precedes table lookup, unconditionally).
	differ(t, "update set CTE shadows a real table", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE c(n)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`INSERT INTO c VALUES(99)`,
		`WITH c AS (SELECT count(*) AS n FROM t) UPDATE t SET b=(SELECT n FROM c)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// A conflict-resolving UPDATE: every row is assigned the same value into a
	// UNIQUE column, so all but one row is REPLACED away.
	differ(t, "update or replace set subquery over target", []string{
		`CREATE TABLE t(a,b UNIQUE)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE OR REPLACE t SET b=(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	differ(t, "update or ignore set subquery over target", []string{
		`CREATE TABLE t(a,b UNIQUE)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE OR IGNORE t SET b=(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	// The UPDATE also fires triggers, so the compiled loop runs its per-row
	// trigger programs beside the once-evaluated subquery.
	differ(t, "update set subquery over target with an AFTER trigger", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`,
		`UPDATE t SET b=(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
		`SELECT x FROM log ORDER BY rowid`,
	})
	differ(t, "update set subquery over target with a BEFORE trigger", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.b); END`,
		`UPDATE t SET b=(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
		`SELECT x FROM log ORDER BY rowid`,
	})
	// A self-reading SET subquery beside an aggregate over a JOIN of the target
	// with another table.
	differ(t, "update set join subquery over target", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
		`UPDATE t SET b=(SELECT count(*) FROM t JOIN s ON s.x=t.a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Two SET terms, both self-reading, plus a self-reading WHERE.
	differ(t, "update two self-reading SET terms and a self-reading WHERE", []string{
		`CREATE TABLE t(a,b,c)`,
		`INSERT INTO t VALUES(1,0,0),(2,0,0),(3,0,0)`,
		`UPDATE t SET b=(SELECT count(*) FROM t), c=(SELECT sum(a) FROM t) WHERE a<(SELECT max(a) FROM t)`,
		`SELECT a,b,c FROM t ORDER BY a`,
	})
}

// TestUpdateSetSelfReadInsideATriggerBody is the case with a genuinely different
// lifetime from every other one here, and the reason it gets its own function.
//
// A trigger body's statements are re-lowered at FIRE time against the database
// as the firing sees it (vdbe_live_read.go), because in C a body is one
// SubProgram invoked per row with OP_Program (trigger.c:1414-1415) whose
// per-invocation VdbeFrame zeroes the OP_Once bits its subqueries key off
// (vdbe.c:7581-7582). So "once" here means ONCE PER FIRING, not once per
// statement: the two-row INSERT below fires the body twice, and the second
// firing's subquery must count the table the FIRST firing left behind.
func TestUpdateSetSelfReadInsideATriggerBody(t *testing.T) {
	differ(t, "trigger body UPDATE SET subquery over its own target", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE fire(x)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN UPDATE t SET b=(SELECT count(*) FROM t); END`,
		`INSERT INTO fire VALUES(1)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Fired TWICE, and the body's subquery counts rows the PREVIOUS firing
	// changed -- so a body lowered once against a frozen image answers 2,2 for
	// both firings where the oracle answers 2 then 0.
	differ(t, "trigger body UPDATE SET subquery, fired twice", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE fire(x)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0); END`,
		`INSERT INTO fire VALUES(1),(2)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// The body's target is the trigger's OWN table, reached through a cascade.
	differ(t, "trigger body UPDATE SET subquery over the fired table", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE fire(x)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`CREATE TRIGGER tr AFTER INSERT ON fire BEGIN UPDATE t SET b=(SELECT sum(a) FROM t) WHERE a=new.x; END`,
		`INSERT INTO fire VALUES(1),(2)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}

// TestUpdateSetSelfReadIsNotCachedEngineDirect's driver-side twin: the same
// statement TEXT run twice must not answer the second time from the first
// run's frozen snapshot. Kept here as well as engine-direct because the two
// paths reach the plan cache differently -- driver re-prepares, so this
// case cannot see a cache bug the engine-direct one can. It is here for the
// ANSWERS, not for the cache.
func TestUpdateSetSelfReadRepeated(t *testing.T) {
	differ(t, "update set self-read repeated", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
		`SELECT a,b FROM t ORDER BY a`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
		`SELECT a,b FROM t ORDER BY a`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "update set self-read repeated inside a transaction", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`BEGIN`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
		`SELECT a,b FROM t ORDER BY a`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
		`SELECT a,b FROM t ORDER BY a`,
		`COMMIT`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}
