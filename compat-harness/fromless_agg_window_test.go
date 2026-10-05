// Bare aggregate combined with window function in FROM-less SELECT.
package compat

import "testing"

func TestFromlessAggregateWithWindow(t *testing.T) {
	differ(t, "FROM-less aggregate beside a window function", []string{
		`SELECT count(*) AS c, row_number() OVER () AS r`,
		`SELECT max(1) AS m, ntile(1) OVER () AS n`,
		`SELECT min(7) AS m, row_number() OVER () AS r`,
		`SELECT sum(3) AS s, ntile(1) OVER () AS n`,
		`SELECT count(*) AS c, count(*) OVER () AS w`,
		// The window's own value beside an aggregate of a literal.
		`SELECT total(2) AS t, rank() OVER (ORDER BY 1) AS rk`,
		`SELECT group_concat('a') AS g, row_number() OVER () AS r`,
	})
	// The same shapes with a FROM, which already worked and must not regress.
	differ(t, "aggregate beside a window function WITH a FROM", []string{
		`CREATE TABLE t0(c0)`,
		`INSERT INTO t0 VALUES(0),(1)`,
		`SELECT MIN(c0) AS m, NTILE(1) OVER() AS n FROM t0`,
		`SELECT count(*) AS c, row_number() OVER () AS r FROM t0`,
		`SELECT sum(c0) AS s, row_number() OVER (ORDER BY 1) AS r FROM t0`,
	})
	// window1.test 25's own pair, the SUBQUERY-BODY form: MIN(c0) associates
	// with the ENCLOSING query and collapses it, so the answer is ONE row, not
	// two. These were pinned as declines by
	// TestFromlessAggregateWithWindowInSubqueryStillDeclines, whose own doc
	// comment said to delete it once the engine agreed. It does: both are
	// byte-identical to 3.53.3 (0|0 and 0|1), so they belong in the battery,
	// where a regression in EITHER direction shows up, rather than behind a pin
	// that only notices one of them.
	differ(t, "FROM-less aggregate + window inside a subquery body", []string{
		`CREATE TABLE t0(c0)`,
		`INSERT INTO t0 VALUES(0),(1)`,
		`SELECT c0, (0, 0) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT c0, (0, 1) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
	})
	// A FROM-less window with NO aggregate, the pre-existing path.
	differ(t, "FROM-less window with no aggregate", []string{
		`SELECT row_number() OVER () AS r`,
		`SELECT sum(3) OVER () AS s`,
		`SELECT lag(7,1,99) OVER () AS l`,
		`SELECT sum(1) OVER () AS s LIMIT 1 OFFSET 1`,
	})
}
