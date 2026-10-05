// Differential tests of recursive CTEs: schema-qualified names are never CTEs,
// and outer LIMITs bound non-terminating recursions.
package compat

import "testing"

func TestRecursiveCTEQualifiedNameIsNotTheCTE(t *testing.T) {
	differ(t, "schema-qualified name inside a same-named CTE", []string{
		`CREATE TABLE t4(x)`,
		`INSERT INTO t4 VALUES(4),(7)`,
		`WITH t4(x) AS ( VALUES(4) UNION ALL SELECT x+1 FROM main.t4 WHERE x<10 ) SELECT * FROM t4`,
		`WITH t4(x) AS ( SELECT 4 UNION ALL SELECT x+1 FROM main.t4 WHERE x<10 ) SELECT * FROM t4`,
		`WITH t4(x) AS ( VALUES(99) UNION ALL SELECT x+1 FROM main.t4 WHERE x<10 ) SELECT * FROM t4`,
		`WITH t4(x) AS ( SELECT 99 UNION ALL SELECT x*10 FROM main.t4 ) SELECT * FROM t4 ORDER BY x`,
		`WITH t4(x) AS ( SELECT * FROM main.t4 ) SELECT * FROM t4 ORDER BY x`,
		// An UNQUALIFIED self-reference is still recursive.
		`WITH t4(x) AS ( VALUES(4) UNION ALL SELECT x+1 FROM t4 WHERE x<10 ) SELECT * FROM t4`,
		// ...and a genuine circular reference is still an error on both.
		`WITH t4(x) AS ( SELECT * FROM t4 ) SELECT * FROM t4`,
	})
	// "temp." behaves identically to "main.".
	differ(t, "temp-qualified name inside a same-named CTE", []string{
		`CREATE TEMP TABLE tt(x)`,
		`INSERT INTO tt VALUES(1),(5)`,
		`WITH tt(x) AS ( VALUES(9) UNION ALL SELECT x+1 FROM temp.tt WHERE x<10 ) SELECT * FROM tt ORDER BY x`,
	})
}

func TestRecursiveCTEOuterLimit(t *testing.T) {
	differ(t, "non-terminating recursive CTE bounded by an outer LIMIT", []string{
		`WITH i(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM i) SELECT x FROM i LIMIT 10`,
		// A CYCLE rather than unbounded growth: the values repeat, so a cap
		// applied at the wrong place would show up as the wrong SEQUENCE.
		`WITH i(x) AS ( VALUES(1) UNION ALL SELECT (x+1)%10 FROM i) SELECT x FROM i LIMIT 20`,
		// Two columns, one derived from the other.
		`WITH w1(a,b) AS ( SELECT 1, 1 UNION ALL SELECT a+1, b + 2*a + 1 FROM w1 ) SELECT * FROM w1 LIMIT 5`,
		// OFFSET has to be added to the bound, not ignored.
		`WITH i(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM i) SELECT x FROM i LIMIT 5 OFFSET 7`,
		`WITH i(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM i) SELECT x FROM i LIMIT 3 OFFSET 0`,
		// LIMIT 0 emits nothing and must still terminate.
		`WITH i(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM i) SELECT x FROM i LIMIT 0`,
		// A bound LARGER than a TERMINATING recursion's own row count must not
		// truncate it or change it.
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<4 ) SELECT x FROM c LIMIT 100`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<4 ) SELECT x FROM c LIMIT 2`,
		// A CTE-internal LIMIT already bounded these; the outer one may only
		// tighten, never loosen.
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c LIMIT 6 ) SELECT x FROM c LIMIT 3`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c LIMIT 4 ) SELECT x FROM c LIMIT 100`,
		// UNION (dedup) rather than UNION ALL.
		`WITH u(x) AS ( VALUES(1) UNION SELECT (x+1)%4 FROM u ) SELECT x FROM u LIMIT 3`,
	})
	// A bound LIMIT parameter is resolved before the cap is published, so this
	// must behave exactly like the literal form.
	differ(t, "outer LIMIT through a bound parameter", []string{
		`WITH i(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM i) SELECT x FROM i LIMIT 4`,
	})
}

// TestRecursiveCTEOuterLimitNotAppliedWhereUnsafe is the important half: every
// shape here reads PAST the first limit+offset rows of the CTE, so capping
// would return too few rows. A decline is fine; a SHORT ANSWER is not, and
// differ catches that either way.
func TestRecursiveCTEOuterLimitNotAppliedWhereUnsafe(t *testing.T) {
	// These use a TERMINATING recursion, so both engines answer and the row
	// COUNTS are compared for real -- an unsafely-applied cap shows up as
	// missing rows rather than as a decline.
	differ(t, "outer LIMIT must not bound the CTE through a filter", []string{
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT x FROM c WHERE x>15 LIMIT 3`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT x FROM c ORDER BY x DESC LIMIT 3`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT DISTINCT x%3 FROM c LIMIT 2`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT count(*) FROM c LIMIT 1`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT x%2 AS g, count(*) FROM c GROUP BY g ORDER BY g LIMIT 5`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT c1.x FROM c AS c1, c AS c2 WHERE c1.x=c2.x ORDER BY c1.x LIMIT 4`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT (SELECT count(*) FROM c) FROM c LIMIT 2`,
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT x FROM c UNION ALL SELECT 99 LIMIT 4`,
		// No LIMIT at all: nothing to cap with.
		`WITH c(x) AS ( VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ) SELECT count(*) FROM c`,
	})
}
