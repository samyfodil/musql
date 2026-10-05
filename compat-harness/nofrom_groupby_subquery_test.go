// Tests FROM-less GROUP BY when it appears inside subqueries and CTEs.
// The compiler previously rejected these with an unconditional guard; they
// now compile correctly via validateNoFromGroupByCompile.
package compat

import "testing"

// TestNoFromGroupBySubqueryCTECore tests a CTE whose core is a FROM-less
// SELECT with GROUP BY.
func TestNoFromGroupBySubqueryCTECore(t *testing.T) {
	differ(t, "CTE core is a FROM-less GROUP BY, referenced twice", []string{
		`WITH t1 AS (SELECT 1 AS c1 GROUP BY 1) SELECT a.c1 FROM t1 AS a, t1 AS b`,
	})
}

// TestNoFromGroupBySubqueryShapes tests FROM-less GROUP BY across scalar,
// EXISTS, and nested WITH subqueries.
func TestNoFromGroupBySubqueryShapes(t *testing.T) {
	differ(t, "scalar subquery body is a FROM-less GROUP BY", []string{
		`SELECT (WITH t1 AS (SELECT 1 AS c1 GROUP BY 1) SELECT c1 FROM t1)`,
	})
	differ(t, "EXISTS body is a FROM-less GROUP BY", []string{
		`SELECT EXISTS (WITH t1 AS (SELECT 1 AS c1 GROUP BY 1) SELECT * FROM t1)`,
	})
	differ(t, "a WITH nested two levels inside a scalar subquery", []string{
		`SELECT (WITH t2 AS (WITH t1 AS (SELECT 1 AS c1 GROUP BY 1) SELECT c1 FROM t1) SELECT c1 FROM t2)`,
	})
	differ(t, "GROUP BY on a select-list alias, reached the same way", []string{
		`SELECT EXISTS (WITH t1 AS (SELECT 1 AS c1 GROUP BY c1) SELECT * FROM t1)`,
	})
	differ(t, "no WITH at all -- a bare scalar subquery body", []string{
		`SELECT (SELECT 1 GROUP BY 1)`,
	})
	differ(t, "no WITH at all -- a bare EXISTS body", []string{
		`SELECT EXISTS(SELECT 1 GROUP BY 1)`,
	})
	differ(t, "no WITH at all -- an IN subquery body", []string{
		`SELECT 1 WHERE 1 IN (SELECT 1 GROUP BY 1)`,
	})
}

// TestNoFromGroupBySubqueryValidation checks that FROM-less GROUP BY
// validation rules (out-of-range ordinal, aggregate in clause, unresolvable
// name) apply identically in subqueries.
func TestNoFromGroupBySubqueryValidation(t *testing.T) {
	differ(t, "GROUP BY ordinal out of range, reached via EXISTS", []string{
		`SELECT EXISTS (WITH t1 AS (SELECT 1 AS c1 GROUP BY 2) SELECT * FROM t1)`,
	})
	differ(t, "aggregate call in GROUP BY, reached via EXISTS", []string{
		`SELECT EXISTS (WITH t1 AS (SELECT count(*) AS c1 GROUP BY 1) SELECT * FROM t1)`,
	})
	differ(t, "aggregate call in GROUP BY via a select-list alias", []string{
		`SELECT EXISTS (WITH t1 AS (SELECT max(4) AS m GROUP BY m) SELECT * FROM t1)`,
	})
	differ(t, "unresolvable GROUP BY term, reached via EXISTS", []string{
		`SELECT EXISTS (WITH t1 AS (SELECT 1 AS c1 GROUP BY nosuchcol) SELECT * FROM t1)`,
	})
}

// TestNoFromGroupBySubqueryHavingStillDeclines checks that FROM-less
// GROUP BY combined with HAVING still declines correctly in subqueries,
// as it does at the top level.
func TestNoFromGroupBySubqueryHavingStillDeclines(t *testing.T) {
	differAllowingDeclines(t, "FROM-less GROUP BY + HAVING, reached via EXISTS -- still declines", []string{
		`SELECT EXISTS (WITH t1 AS (SELECT 1 AS c1 GROUP BY c1 HAVING c1>0) SELECT * FROM t1)`,
	})
}

// TestNoFromGroupBySubqueryWith3Test tests FROM-less GROUP BY inside
// nested CTEs from the tcl corpus.
func TestNoFromGroupBySubqueryWith3Test(t *testing.T) {
	differ(t, "with3.test 4.0 verbatim", []string{
		`WITH t5(t5col1) AS (
		  SELECT (
		    WITH t3(t3col1) AS (
		      WITH t2 AS (
		        WITH t1 AS (SELECT 1 AS c1 GROUP BY 1)
		        SELECT a.c1 FROM t1 AS a, t1 AS b
		        WHERE anoncol1 = 1
		      )
		      SELECT (SELECT 1 FROM t2) FROM t2
		    )
		    SELECT t3col1 FROM t3 WHERE t3col1
		  ) FROM (SELECT 1 AS anoncol1)
		)
		SELECT t5col1, t5col1 FROM t5`,
	})
	differ(t, "with3.test 4.1 verbatim", []string{
		`SELECT EXISTS (
		    WITH RECURSIVE Table0 AS (
		      WITH RECURSIVE Table0(Col0) AS (SELECT ALL 1)
		      SELECT ALL (
		        WITH RECURSIVE Table0 AS (
		          WITH RECURSIVE Table0 AS (
		            WITH RECURSIVE Table0 AS (SELECT DISTINCT 1 GROUP BY 1)
		            SELECT DISTINCT * FROM Table0 NATURAL INNER JOIN Table0
		            WHERE Col0 = 1
		          )
		          SELECT ALL (SELECT DISTINCT * FROM Table0) FROM Table0 WHERE Col0 = 1
		        )
		        SELECT ALL * FROM Table0 NATURAL INNER JOIN Table0
		      ) FROM Table0 )
		      SELECT DISTINCT * FROM Table0 NATURAL INNER JOIN Table0
		    )`,
	})
}

// TestWith2Test101RuntimeCTEScopeFixed tests runtime CTE scope recovery
// for a GROUP BY that re-enters CTE resolution after compile time.
func TestWith2Test101RuntimeCTEScopeFixed(t *testing.T) {
	differ(t, "with2.test 10.1 verbatim -- runtime CTE-scope gap fixed", []string{
		`SELECT 1 AS c WHERE (
		  SELECT (
		    WITH t1(a) AS (VALUES( c ))
		    SELECT ( SELECT t1a.a FROM t1 AS t1a, t1 AS t1x )
		    FROM t1 AS xyz GROUP BY 1
		  )
		)`,
	})
}

// TestNoFromGroupBySubqueryWindowFunctionDeclines checks that window
// functions in a FROM-less GROUP BY correctly reject; they were previously
// silently accepted.
func TestNoFromGroupBySubqueryWindowFunctionDeclines(t *testing.T) {
	differ(t, "window function in a FROM-less CTE core's GROUP BY, reached via EXISTS", []string{
		`SELECT EXISTS (WITH t2 AS (SELECT row_number() OVER () AS c1 GROUP BY 1) SELECT * FROM t2)`,
	})
	differ(t, "same shape through a trigger body -- a rejected write, not just a wrong read", []string{
		`CREATE TABLE t10(x)`,
		`CREATE TABLE log2(y)`,
		`CREATE TRIGGER trg10 AFTER INSERT ON t10 BEGIN
		   INSERT INTO log2 SELECT EXISTS (WITH t2 AS (SELECT row_number() OVER () AS c1 GROUP BY 1) SELECT * FROM t2);
		 END`,
		`INSERT INTO t10 VALUES(1)`,
		`SELECT count(*) FROM log2`,
	})
}

// TestNoFromGroupBySubqueryCorrelatedProbeDeclines checks that a correlated
// FROM-less GROUP BY term correctly errors on each outer row.
func TestNoFromGroupBySubqueryCorrelatedProbeDeclines(t *testing.T) {
	differ(t, "correlated FROM-less GROUP BY term errors per-row on C SQLite", []string{
		`CREATE TABLE t9(x INTEGER)`,
		`INSERT INTO t9 VALUES(1),(2000000000000)`,
		`SELECT x, (SELECT 1 AS c1 GROUP BY zeroblob(x)) FROM t9`,
	})
}
