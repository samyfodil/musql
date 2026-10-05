// CTE bodies correlating to outer select-list aliases are now resolved
// correctly. These tests verify that aliases in enclosing queries can be
// referenced from within CTE bodies, with proper shadowing and scoping rules.
package compat

import "testing"

// buildCTEAliasBodyDB seeds a table for the sibling-isolation test.
func buildCTEAliasBodyDB(t *testing.T) []string {
	t.Helper()
	return []string{`CREATE TABLE t3(c)`, `INSERT INTO t3 VALUES(99)`}
}

// TestCTEAliasCorrelatedBodyBasic checks that outer aliases can be referenced
// in CTE bodies within EXISTS, IN, and scalar comparisons.
func TestCTEAliasCorrelatedBodyBasic(t *testing.T) {
	differ(t, "EXISTS(WITH ... VALUES(outer alias) ...)", []string{
		`SELECT 1 AS c WHERE EXISTS (WITH t1(a) AS (VALUES(c)) SELECT * FROM t1)`,
	})
	differ(t, "outer alias IN (WITH ... SELECT a FROM t1)", []string{
		`SELECT 1 AS c WHERE c IN (WITH t1(a) AS (VALUES(c)) SELECT a FROM t1)`,
	})
	differ(t, "a WHERE-position scalar comparison against a WITH body", []string{
		`SELECT 1 AS c WHERE (WITH t1(a) AS (VALUES(c)) SELECT a FROM t1) = 1`,
	})
}

// TestCTEAliasCorrelatedBodyNesting checks that nested CTEs can reference
// outer aliases, including in CTEs with FROM and GROUP BY clauses.
func TestCTEAliasCorrelatedBodyNesting(t *testing.T) {
	differ(t, "WITH nested inside another WITH's own body", []string{
		`SELECT 1 AS c WHERE EXISTS (WITH t1(a) AS (WITH t2(b) AS (VALUES(c)) SELECT b FROM t2) SELECT * FROM t1 WHERE a=1)`,
	})
	differ(t, "the WITH's own defining query has a real FROM and GROUP BY", []string{
		`SELECT 1 AS c WHERE (SELECT (WITH t1(a) AS (VALUES(c)) SELECT a FROM t1 AS xyz GROUP BY 1))`,
	})
}

// TestCTEAliasCorrelatedBodyShadowing checks that shadowing rules apply
// correctly within CTE bodies.
func TestCTEAliasCorrelatedBodyShadowing(t *testing.T) {
	differ(t, "the CTE's own body defines the same alias name -- it wins", []string{
		`SELECT 1 AS c WHERE EXISTS (WITH t1(a) AS (SELECT 5 AS c WHERE c=1) SELECT * FROM t1)`,
	})
	differ(t, "same shape, the CTE's own alias condition is true instead", []string{
		`SELECT 1 AS c WHERE EXISTS (WITH t1(a) AS (SELECT 5 AS c WHERE c=5) SELECT * FROM t1)`,
	})
	differ(t, "a MIDDLE level's own alias wins over a farther outer one of the identical name", []string{
		`SELECT 1 AS c WHERE EXISTS (SELECT 99 AS c WHERE EXISTS (WITH t1(a) AS (VALUES(c)) SELECT * FROM t1 WHERE a=99))`,
	})
}

// TestCTEAliasCorrelatedBodySiblingIsolation checks that sibling tables do not
// shadow the alias spliced into a CTE.
func TestCTEAliasCorrelatedBodySiblingIsolation(t *testing.T) {
	stmts := buildCTEAliasBodyDB(t)
	differ(t, "a sibling real table's own column of the identical name does not leak in", append(stmts,
		`SELECT 5 AS c WHERE EXISTS (SELECT * FROM t3, (WITH t1(a) AS (VALUES(c)) SELECT * FROM t1))`,
	))
}

// TestCTEAliasCorrelatedBodyRecursiveUnaffected confirms that recursive CTEs
// remain unchanged by this fix (they are excluded by existing guards).
func TestCTEAliasCorrelatedBodyRecursiveUnaffected(t *testing.T) {
	differAllowingDeclines(t, "a recursive CTE's own seed referencing an outer alias -- unaffected, still declines", []string{
		`SELECT 1 AS c WHERE EXISTS (WITH RECURSIVE t1(a) AS (VALUES(c) UNION ALL SELECT a+1 FROM t1 WHERE a<3) SELECT * FROM t1)`,
	})
}

// TestCTEAliasCorrelatedBodyOwnClauseStillDeclines confirms that references
// to aliases in WHERE clauses of queries with FROM remain declined.
func TestCTEAliasCorrelatedBodyOwnClauseStillDeclines(t *testing.T) {
	differAllowingDeclines(t, "an intermediate FROM-having level's own WHERE reaching into a nested WITH's body", []string{
		`SELECT 1 AS c WHERE EXISTS (SELECT * FROM (SELECT 1) WHERE EXISTS (WITH t1(a) AS (VALUES(c)) SELECT * FROM t1))`,
	})
}

// TestCTEAliasCorrelatedBodyWith2Test101 checks the complex case from with2.test 10.1,
// which involves nested CTEs with GROUP BY and subqueries referencing the same CTE.
func TestCTEAliasCorrelatedBodyWith2Test101(t *testing.T) {
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
