// Tests handling of non-integer LIMIT/OFFSET in subqueries.
// The check must happen at runtime only, not at compile time.
package compat

import "testing"

// TestSubqueryLimitDeferredCorpus is aggnested.test 5.5 verbatim.
func TestSubqueryLimitDeferredCorpus(t *testing.T) {
	flLockstep(t, "aggnested-5.5", []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(b)`,
		`CREATE TABLE a(b)`,
	},
		`WITH c AS(SELECT a)
    SELECT(SELECT(SELECT string_agg(b, b)
          LIMIT(SELECT 0.100000 *
            AVG(DISTINCT(SELECT 0 FROM a ORDER BY b, b, b))))
        FROM a GROUP BY b,
        b, b) FROM a EXCEPT SELECT b FROM a ORDER BY b,
    b, b`)
}

// TestSubqueryLimitDeferred pins both halves: the clause must still raise
// "datatype mismatch" when the subquery IS stepped, and must NOT raise when it
// is not. flLockstep compares the accept/reject verdict of every statement and
// then every query's columns and rows, so a decline where the oracle answers
// (and an answer where the oracle declines) both fail here.
func TestSubqueryLimitDeferred(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE e(z)`, // empty, so a scalar subquery over it never runs
	}
	flLockstep(t, "subquery LIMIT that is not an integer", setup,
		// NEVER STEPPED -- the enclosing scan has no rows, so 3.53.3 answers.
		`SELECT (SELECT a FROM t LIMIT NULL) FROM e`,
		`SELECT (SELECT a FROM t LIMIT 1 OFFSET NULL) FROM e`,
		`SELECT (SELECT a FROM t LIMIT (SELECT avg(z) FROM e)) FROM e`,
		`SELECT (SELECT a FROM t LIMIT 2.5) FROM e`,
		`SELECT (SELECT a FROM t LIMIT 'x') FROM e`,
		// STEPPED -- OP_MustBeInt fires, in both engines.
		`SELECT * FROM (SELECT a FROM t LIMIT NULL)`,
		`SELECT * FROM (SELECT a FROM t LIMIT 2.5)`,
		`SELECT * FROM (SELECT a FROM t LIMIT 'x')`,
		`SELECT (SELECT a FROM t LIMIT NULL)`,
		`SELECT (SELECT a FROM t LIMIT 1 OFFSET NULL)`,
		`SELECT EXISTS(SELECT a FROM t LIMIT NULL)`,
		`SELECT a FROM t WHERE a IN (SELECT a FROM t LIMIT NULL)`,
		// Shapes whose body codes no counter register (a sorter, an aggregate,
		// a GROUP BY, a compound, a window): the fold's own error stands.
		`SELECT * FROM (SELECT a FROM t ORDER BY a LIMIT NULL)`,
		`SELECT * FROM (SELECT count(*) FROM t LIMIT NULL)`,
		`SELECT * FROM (SELECT a FROM t GROUP BY a LIMIT NULL)`,
		`SELECT * FROM (SELECT a FROM t UNION SELECT 9 LIMIT NULL)`,
		`SELECT * FROM (SELECT sum(a) OVER () FROM t LIMIT NULL)`,
		// CONTROLS: a clause that DOES fold is unchanged.
		`SELECT * FROM (SELECT a FROM t LIMIT 2.0)`,
		`SELECT * FROM (SELECT a FROM t LIMIT '2')`,
		`SELECT * FROM (SELECT a FROM t LIMIT 1+1)`,
		`SELECT (SELECT a FROM t LIMIT -1)`,
		`SELECT * FROM (SELECT a FROM t LIMIT 2 OFFSET 1)`,
	)
}
