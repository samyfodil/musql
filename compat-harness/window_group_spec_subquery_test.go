// This file gates subqueries inside window specifications in aggregate queries.
// Window spec clauses are lifted into a derived query that carries the original
// FROM, allowing correlated references to resolve correctly.
package compat

import "testing"

// TestWindowSpecSubqueryLift checks window spec subqueries with correlation.
func TestWindowSpecSubqueryLift(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE TABLE t9(k)`,
		`INSERT INTO t1 VALUES(1,50),(1,10),(2,1),(2,20),(3,30)`,
		`INSERT INTO t9 VALUES(7),(8)`,
	}
	q := func(sql ...string) []string { return append(append([]string{}, setup...), sql...) }

	// These queries answer in the oracle and used to decline.
	answering := []string{
		`SELECT a, sum(b) OVER (ORDER BY (SELECT max(k) FROM t9)) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER (ORDER BY (SELECT k FROM t9 WHERE k>t1.a LIMIT 1)) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER (PARTITION BY (SELECT k FROM t9 WHERE k>t1.a LIMIT 1)) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER (ORDER BY EXISTS(SELECT 1 FROM t9 WHERE k>t1.a)) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER (ORDER BY t1.a IN (SELECT k FROM t9)) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER w FROM t1 GROUP BY a WINDOW w AS (ORDER BY (SELECT k FROM t9 WHERE k>t1.a LIMIT 1))`,
		`SELECT sum(b) OVER (ORDER BY (SELECT max(k) FROM t9)) FROM t1`,
		`SELECT a, sum(b) OVER (ORDER BY (SELECT b) DESC, a) FROM t1 GROUP BY a`,
	}
	for _, sql := range answering {
		differ(t, sql, q(sql))
		res := run(t, "musql", q(sql))
		if last := res[len(res)-1]; last["kind"] == "error" {
			t.Errorf("still declining: %s", sql)
		}
	}
}

// TestTrigger1Mined22_10 replays a corpus statement with a trigger and window spec.
func TestTrigger1Mined22_10(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(
    a INTEGER PRIMARY KEY,
    b DOUBLE
  )`,
		`CREATE TRIGGER x AFTER UPDATE ON t1 BEGIN
   SELECT sum(b)OVER(ORDER BY (SELECT b FROM t1 AS x
                               WHERE b IN (t1.a,127,t1.b)
                               GROUP BY b))
     FROM t1
     GROUP BY a;
  END`,
		`CREATE TEMP TRIGGER x BEFORE INSERT ON t1 BEGIN
    UPDATE t1
       SET b=randomblob(10)
     WHERE b >= 'E'
       AND a < (SELECT a FROM t1 WHERE a<22 GROUP BY b);
  END`,
		`INSERT INTO t1(b) VALUES('Y'),('X'),('Z')`,
		`SELECT a, CASE WHEN typeof(b)='text' THEN quote(b) ELSE '<blob>' END, '|' FROM t1`,
	}
	differ(t, "trigger1.test trigger1-22.10", stmts)
	res := run(t, "musql", stmts)
	for i, r := range res {
		if r["kind"] == "error" {
			t.Errorf("trigger1-22.10 statement %d still declines: %v", i, r)
		}
	}
}

// TestTrigger1BodySelectAnswers checks the trigger body's window spec query.
func TestTrigger1BodySelectAnswers(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b DOUBLE)`,
		`INSERT INTO t1(b) VALUES('Y'),('X'),('Z')`,
		`SELECT sum(b)OVER(ORDER BY (SELECT b FROM t1 AS x WHERE b IN (t1.a,127,t1.b) GROUP BY b)) FROM t1 GROUP BY a`,
	}
	differ(t, "trigger1-22.10 body SELECT", stmts)
	res := run(t, "musql", stmts)
	if last := res[len(res)-1]; last["kind"] == "error" {
		t.Errorf("trigger1-22.10's body SELECT still declines: %v", last)
	}
}

// TestTriggerBodyAggregateSelectListSubquery checks trigger bodies with select-list subqueries.
func TestTriggerBodyAggregateSelectListSubquery(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(v)`,
		`INSERT INTO t1(b) VALUES('Y'),('X'),('Z')`,
		`CREATE TRIGGER tr AFTER UPDATE ON t1 BEGIN
			INSERT INTO log SELECT (SELECT x.b FROM t1 AS x WHERE x.a=t1.a) FROM t1 GROUP BY a;
		END`,
		`UPDATE t1 SET b=b WHERE a=1`,
		`SELECT v FROM log ORDER BY v`,
	}
	differ(t, "trigger body: aggregate query with a select-list subquery", stmts)
	res := run(t, "musql", stmts)
	for i, r := range res {
		if r["kind"] == "error" {
			t.Errorf("statement %d still declines: %v", i, r)
		}
	}
}

// TestWindowSelectListSubqueryStillDeclines verifies select-list subqueries still decline.
func TestWindowSelectListSubqueryStillDeclines(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE TABLE t9(k)`,
		`INSERT INTO t1 VALUES(1,50),(1,10),(2,1),(2,20),(3,30)`,
		`INSERT INTO t9 VALUES(7),(8)`,
	}
	for _, q := range []string{
		`SELECT a, sum(b) OVER () + (SELECT b) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER () + (SELECT k FROM t9 WHERE k>a LIMIT 1) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER () IN (SELECT k FROM t9) FROM t1 GROUP BY a`,
	} {
		res := run(t, "musql", append(append([]string{}, setup...), q))
		if res[len(res)-1]["kind"] != "error" {
			t.Errorf("expected a clean decline for %q, got %v", q, res[len(res)-1])
		}
	}
}
