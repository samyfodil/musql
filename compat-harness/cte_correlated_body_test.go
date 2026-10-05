// Tests correlated references from enclosing queries into CTE bodies.
// A non-recursive CTE's body can reference columns from an enclosing query.
package compat

import "testing"

// buildCTECorrelatedBodyDB seeds one small table whose column both feeds the
// correlation and orders the output deterministically.
func buildCTECorrelatedBodyDB(t *testing.T) []string {
	t.Helper()
	return []string{
		`CREATE TABLE t1(x, y)`,
		`INSERT INTO t1 VALUES(5,'a'),(10,'b'),(15,'c')`,
	}
}

func TestCTECorrelatedBodyExists(t *testing.T) {
	stmts := buildCTECorrelatedBodyDB(t)
	differ(t, "EXISTS(WITH ... VALUES(outer column) ...)", append(stmts,
		`SELECT x FROM t1 WHERE EXISTS (WITH t2(a) AS (VALUES(x)) SELECT * FROM t2 WHERE a>7) ORDER BY x`,
	))
	differ(t, "EXISTS(WITH ... whose own WHERE references the outer column ...)", append(stmts,
		`SELECT x FROM t1 WHERE EXISTS (WITH t2(a) AS (SELECT 1 WHERE x>7) SELECT * FROM t2) ORDER BY x`,
	))
}

func TestCTECorrelatedBodyInAndScalar(t *testing.T) {
	stmts := buildCTECorrelatedBodyDB(t)
	differ(t, "outer column IN (WITH ... VALUES(outer column) ...)", append(stmts,
		`SELECT x FROM t1 WHERE x IN (WITH t2(a) AS (VALUES(x)) SELECT a FROM t2) ORDER BY x`,
	))
	differ(t, "a WHERE-position scalar subquery whose body is a WITH", append(stmts,
		`SELECT x FROM t1 WHERE (WITH t2(a) AS (VALUES(x)) SELECT a FROM t2) > 7 ORDER BY x`,
	))
}

// TestCTECorrelatedBodyNested covers nested WITH clauses with correlation.
func TestCTECorrelatedBodyNested(t *testing.T) {
	stmts := buildCTECorrelatedBodyDB(t)
	differ(t, "WITH inside WITH inside a correlated EXISTS", append(stmts,
		`SELECT x FROM t1 WHERE EXISTS (WITH t2(a) AS (WITH t3(b) AS (VALUES(x)) SELECT b FROM t3) SELECT * FROM t2 WHERE a>7) ORDER BY x`,
	))
	differ(t, "correlation through TWO enclosing subquery levels into a WITH", append(stmts,
		`SELECT x FROM t1 WHERE EXISTS (SELECT * FROM (SELECT 1) WHERE EXISTS (WITH t2(a) AS (VALUES(x)) SELECT * FROM t2 WHERE a>7)) ORDER BY x`,
	))
}

// TestCTECorrelatedBodyUncorrelatedControl tests uncorrelated CTEs.
func TestCTECorrelatedBodyUncorrelatedControl(t *testing.T) {
	stmts := buildCTECorrelatedBodyDB(t)
	differ(t, "uncorrelated CTE inside EXISTS -- unaffected", append(stmts,
		`SELECT x FROM t1 WHERE EXISTS (WITH t2(a) AS (VALUES(99)) SELECT * FROM t2) ORDER BY x`,
	))
	// One correlated and one uncorrelated reference to same CTE.
	differ(t, "one correlated and one uncorrelated reference to the same CTE name", append(stmts,
		`SELECT x, (SELECT a FROM (WITH t2(a) AS (VALUES(1)) SELECT * FROM t2))
		 FROM t1
		 WHERE EXISTS (WITH t2(a) AS (VALUES(x)) SELECT * FROM t2 WHERE a>7)
		 ORDER BY x`,
	))
}

// TestCTECorrelatedBodyRecursiveUnaffected tests recursive CTEs.
func TestCTECorrelatedBodyRecursiveUnaffected(t *testing.T) {
	differ(t, "a plain recursive CTE, no outer reference anywhere", []string{
		`WITH RECURSIVE cnt(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM cnt WHERE n<5) SELECT * FROM cnt`,
	})
	stmts := buildCTECorrelatedBodyDB(t)
	differ(t, "a recursive CTE referenced from inside a correlated EXISTS (the outer ref is on the EXISTS side, not inside the recursive CTE)", append(stmts,
		`SELECT x FROM t1 WHERE EXISTS (WITH RECURSIVE cnt(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM cnt WHERE n<5) SELECT * FROM cnt WHERE n=3 AND x>7) ORDER BY x`,
	))
}

// TestCTECorrelatedBodyCycleStillRejected tests CTE cycles are still rejected.
func TestCTECorrelatedBodyCycleStillRejected(t *testing.T) {
	stmts := buildCTECorrelatedBodyDB(t)
	differ(t, "a genuine CTE cycle reached through the new correlated-probe path", append(stmts,
		`SELECT x FROM t1 WHERE EXISTS (WITH c(v) AS (SELECT v FROM c) SELECT * FROM c)`,
	))
}
