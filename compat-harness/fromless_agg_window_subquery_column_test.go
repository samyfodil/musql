// This file gates subqueries containing columns within aggregate+window queries
// that have no FROM clause of their own. Column references in such subqueries
// can be resolved since there is no ambiguity: they must bind to an outer scope.
package compat

import "testing"

// TestFromlessAggWindowSubqueryColumnMined checks subqueries in aggregate+window
// functions where the subquery column binds to its own FROM, not the outer query's.
func TestFromlessAggWindowSubqueryColumnMined(t *testing.T) {
	// Compared with the oracle.
	differ(t, "rowvalue.test 30.3 (verbatim)", []string{
		`CREATE TABLE t1(x INT PRIMARY KEY, y, z)`,
		`CREATE TABLE t2(a,b,c,d,e,PRIMARY KEY(a,b))WITHOUT ROWID`,
		`UPDATE t2 SET (d,d,a)=(SELECT EXISTS(SELECT 1 IN(SELECT max( 1 IN(SELECT x ORDER BY 1)) OVER(PARTITION BY sum((SELECT y FROM t1 UNION SELECT x ORDER BY 1)))INTERSECT SELECT EXISTS(SELECT 1 FROM t1 UNION SELECT x ORDER BY 1) ORDER BY 1) ORDERa)|9 AS blob, 2, 3) FROM t1 WHERE x<a`,
		`SELECT * FROM t2`,
	})

	differ(t, "self-contained minimal (subquery's column binds to its OWN FROM)", []string{
		`CREATE TABLE t1(x, y)`,
		`INSERT INTO t1 VALUES(1,10),(2,20),(3,5)`,
		`SELECT max( 1 IN (SELECT x FROM t1 ORDER BY 1) ) OVER (PARTITION BY sum((SELECT y FROM t1 ORDER BY 1)))`,
	})
}

// TestFromlessAggWindowMixedArmCompound checks a UNION where one arm has a FROM
// and the other does not; a FROM-less arm may still correlate outward.
func TestFromlessAggWindowMixedArmCompound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"mixed-arm compound correlates outward, non-indexed table", []string{
			`CREATE TABLE t1(x, y)`,
			`INSERT INTO t1 VALUES(1,10),(2,20),(3,5)`,
			`SELECT x, (SELECT max(1 IN(SELECT x ORDER BY 1)) OVER(PARTITION BY sum((SELECT y FROM t1 UNION SELECT x ORDER BY 1)))) FROM t1 ORDER BY x`,
		}},
		// Same operand in UPDATE...FROM housing.
		{"mixed-arm compound correlates outward, non-indexed table, UPDATE...FROM housing", []string{
			`CREATE TABLE t1(x, y)`,
			`INSERT INTO t1 VALUES(1,10),(2,20),(3,5)`,
			`CREATE TABLE t2(a,b)`,
			`INSERT INTO t2 VALUES(NULL,NULL)`,
			`UPDATE t2 SET (a,b)=(SELECT max(1 IN(SELECT x ORDER BY 1)) OVER(PARTITION BY sum((SELECT y FROM t1 UNION SELECT x ORDER BY 1))), 2) FROM t1 WHERE x<2`,
			`SELECT a, b FROM t2`,
		}},
	} {
		// Verify the statement answers, not just that both engines agree.
		differ(t, tc.name, tc.stmts)
		res := run(t, "musql", tc.stmts)
		if last := res[len(res)-1]; last["kind"] == "error" {
			t.Errorf("%s: declining again -- this family answered as of walkTransparentBody: %v", tc.name, last)
		}
	}
}

// TestFromlessAggWindowMixedArmCompoundOverIndexedTable tests the same shape
// over an indexed table, where an anchor-row guard causes a clean decline.
func TestFromlessAggWindowMixedArmCompoundOverIndexedTable(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(x INT PRIMARY KEY, y INT)`,
		`INSERT INTO t1 VALUES(1,10),(2,20),(3,5)`,
		`SELECT x, (SELECT max(1 IN(SELECT x ORDER BY 1)) OVER(PARTITION BY sum((SELECT y FROM t1 UNION SELECT x ORDER BY 1)))) FROM t1 ORDER BY x`,
	}
	// Compared with the oracle.
	differ(t, "mixed-arm compound over an indexed table", stmts)
}

// TestTrigger1RealFromWindowKeySubquery tests a trigger with a window ORDER BY key
// subquery that correlates to the outer query's FROM (unlike the above which has no FROM).
func TestTrigger1RealFromWindowKeySubquery(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b DOUBLE)`,
		`CREATE TRIGGER trg AFTER UPDATE ON t1 BEGIN
   SELECT sum(b)OVER(ORDER BY (SELECT b FROM t1 AS x
                               WHERE b IN (t1.a,127,t1.b)
                               GROUP BY b))
     FROM t1
     GROUP BY a;
  END`,
		`INSERT INTO t1(b) VALUES('Y')`,
		`UPDATE t1 SET b=b`,
		`SELECT a, b FROM t1`,
	}
	differ(t, "trigger1-22.10's own shape", stmts)
	res := run(t, "musql", stmts)
	for i, r := range res {
		if r["kind"] == "error" {
			t.Errorf("statement %d still declines: %v", i, r)
		}
	}
}
