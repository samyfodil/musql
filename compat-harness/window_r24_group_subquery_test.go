// Tests subqueries beside window functions in aggregate queries.
package compat

import "testing"

func TestWindowR24GroupedWindowSubquery(t *testing.T) {
	// Test cases from window1.test.
	differ(t, "window1.test 65.3/65.4", []string{
		`CREATE TABLE t1(c1)`,
		`INSERT INTO t1 VALUES('abcd')`,
		`SELECT count() OVER (), group_concat(c1 COLLATE nocase) IN (SELECT 'aBCd') FROM t1`,
		`SELECT COUNT() OVER () LIKE lead(102030) OVER( ORDER BY sum('abcdef' COLLATE nocase) IN (SELECT 54321) ) FROM t1`,
	})

	setup := []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE TABLE t9(k)`,
		`INSERT INTO t1 VALUES(1,50),(1,10),(2,1),(2,20),(3,30)`,
		`INSERT INTO t9 VALUES(7),(8)`,
	}
	q := func(sql ...string) []string { return append(append([]string{}, setup...), sql...) }

	// A column-free subquery in every position the lift walks, with and without
	// a GROUP BY (the whole-table aggregate is the single-group instance).
	differ(t, "column-free subquery beside a window", q(
		`SELECT a, sum(b) OVER () + (SELECT 5) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER () IN (SELECT 60) FROM t1 GROUP BY a`,
		`SELECT a, EXISTS(SELECT 1) AND count(*) OVER () > 0 FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER (ORDER BY (SELECT 1)) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER (PARTITION BY (SELECT 0)) FROM t1 GROUP BY a`,
		`SELECT sum(b) OVER () + (SELECT 5) FROM t1`,
		`SELECT sum(b) IN (SELECT 111), count(*) OVER () FROM t1`,
		`SELECT a, max(b) OVER () FROM t1 GROUP BY a ORDER BY (SELECT 1), a`,
		// A subquery over a table it never names a column of, and one holding
		// its own aggregate -- both still column-free from this query's side.
		`SELECT a, sum(b) OVER () + (SELECT count(*) FROM t9) FROM t1 GROUP BY a`,
		`SELECT a, sum(b) OVER () + (SELECT 1 FROM t9 LIMIT 1) FROM t1 GROUP BY a`,
		// ...and the WINDOW-clause spelling of the same key.
		`SELECT a, sum(b) OVER w FROM t1 GROUP BY a WINDOW w AS (ORDER BY (SELECT 1))`,
	))
}

// TestWindowR24GroupedWindowSubqueryDeclines pins the half that is still
// declined: a subquery that names a column. Whether that column is the GROUP's
// anchor value (SQLite lifts it into the generated sub-select) or the
// subquery's own depends on the subquery's FROM, which this rewrite does not
// resolve -- so it declines rather than lift the wrong one.
func TestWindowR24GroupedWindowSubqueryDeclines(t *testing.T) {
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
