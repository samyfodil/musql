// Tests a VIEW body with a whole-table aggregate (max()) whose sole argument
// is a correlated scalar subquery with a window function. Distinguishes between
// subqueries in the select list and subqueries inside aggregate arguments.
package compat

import "testing"

// TestWindow1ViewMaxSubqueryOverIndexedTable replays window1.test's 37.10 and
// 37.20 verbatim (byte-identical to testdata/tcl/window1.test and
// ~/.cache/musql/sqlite-353/test/window1.test). Oracle answer for both: no
// rows -- t0 is never populated in the mined fixture, so max() over zero rows
// is NULL and the outer BETWEEN excludes it. See
// TestWindow1ViewMaxSubqueryNonEmptyTable below for a widened, genuinely
// row-bearing check that this is a real fix and not merely "empty on both
// sides".
func TestWindow1ViewMaxSubqueryOverIndexedTable(t *testing.T) {
	differ(t, "window1.test 37.10", []string{
		`CREATE TABLE t0(a UNIQUE, b PRIMARY KEY)`,
		`CREATE VIEW v0(c) AS SELECT max((SELECT count(a)OVER(ORDER BY 1))) FROM t0`,
		`SELECT c FROM v0 WHERE c BETWEEN 10 AND 20`,
	})
	differ(t, "window1.test 37.20", []string{
		`CREATE TABLE t0(a UNIQUE, b PRIMARY KEY)`,
		`CREATE VIEW v0(c) AS SELECT max((SELECT count(a)OVER(ORDER BY 1234))) FROM t0`,
		`SELECT c FROM v0 WHERE c BETWEEN -10 AND 20`,
	})
}

// TestWindow1ViewMaxSubqueryNonEmptyTable widens the mined fixture to a
// non-empty, multi-row t0 so the fix is checked against a REAL max()
// computation, not merely the mined statement's trivial "empty table -> no
// rows" case. count(a) OVER(ORDER BY 1) has no FROM of its own, so it sees
// exactly one implicit row whose "a" is the correlated reference to the
// currently-scanned t0 row -- count(a) is 1 when that row's a is non-NULL, 0
// when it is NULL -- so max() over all of t0's rows is 1 iff at least one row
// has a non-NULL a, else 0. Both cases oracle-verified against 3.53.3.
func TestWindow1ViewMaxSubqueryNonEmptyTable(t *testing.T) {
	differ(t, "window1-derived: max/subquery-arg, some non-NULL a", []string{
		`CREATE TABLE t0(a UNIQUE, b PRIMARY KEY)`,
		`INSERT INTO t0 VALUES(NULL, 1), (5, 2), (NULL, 3)`,
		`CREATE VIEW v0(c) AS SELECT max((SELECT count(a)OVER(ORDER BY 1))) FROM t0`,
		`SELECT c FROM v0 WHERE c BETWEEN 0 AND 20`,
	})
	differ(t, "window1-derived: max/subquery-arg, all NULL a", []string{
		`CREATE TABLE t0(a UNIQUE, b PRIMARY KEY)`,
		`INSERT INTO t0 VALUES(NULL, 1), (NULL, 2)`,
		`CREATE VIEW v0(c) AS SELECT max((SELECT count(a)OVER(ORDER BY 1))) FROM t0`,
		`SELECT c FROM v0 WHERE c BETWEEN 0 AND 20`,
	})
}
