// Which query a bare aggregate in a FROM-LESS subquery belongs to.
// Rules differ by clause position: select-list expressions cannot bind to
// a sibling alias, while WHERE/GROUP BY/ORDER BY can. A compound's ORDER BY
// term is already an integer before reference resolution.
package compat

import (
	"fmt"
	"testing"
)

// TestAggArmAliasCaptureAnswers pins the two shapes that used to answer the
// wrong ROW COUNT, plus the boundary cases on either side of each C fact above.
// Both engines are compared cell for cell, over an empty and a populated t1;
// the EMPTY table is what makes the two verdicts distinguishable at all (an
// aggregate query emits one row, a plain scan none).
func TestAggArmAliasCaptureAnswers(t *testing.T) {
	cases := []string{
		// Select list: the alias never captures, so the x being summed is t1's.
		`SELECT (SELECT sum((SELECT x AS c))) FROM t1`,
		`SELECT (SELECT sum((SELECT x AS x))) FROM t1`,
		`SELECT (SELECT sum((SELECT x AS c LIMIT 1))) FROM t1`,
		`SELECT (SELECT sum((SELECT x AS c UNION SELECT 1234 ORDER BY c))) FROM t1`,
		`SELECT (SELECT sum((SELECT b AS c UNION SELECT x ORDER BY c))) FROM t1`,
		// GROUP BY names the alias, and the ALIASED EXPRESSION is spliced in
		// (resolveAlias) -- so its own outer reference still counts.
		`SELECT (SELECT sum((SELECT x AS c GROUP BY c))) FROM t1`,
		// WHERE / non-compound ORDER BY name the alias, and nothing outer is
		// referenced anywhere: the aggregate stays put.
		`SELECT (SELECT sum((SELECT 5 AS x WHERE x=5))) FROM t1`,
		`SELECT (SELECT sum((SELECT 7 AS x ORDER BY x))) FROM t1`,
		// A COMPOUND's ORDER BY names the alias: already an integer by then.
		`SELECT (SELECT sum((SELECT 7 AS x UNION SELECT 8 ORDER BY x))) FROM t1`,
		`SELECT (SELECT total((SELECT 1 AS c))) FROM t1`,
		// No alias at all -- the pre-existing behaviour, unchanged.
		`SELECT (SELECT sum((SELECT x))) FROM t1`,
		`SELECT (SELECT sum((SELECT 5))) FROM t1`,
		`SELECT (SELECT sum((SELECT b+x))) FROM t1`,
	}
	schema := []string{`CREATE TABLE t1(b, x)`}
	rows := append(append([]string{}, schema...), `INSERT INTO t1 VALUES(10,1),(20,2)`)
	for _, setup := range [][]string{schema, rows} {
		for _, sql := range cases {
			differ(t, sql, append(append([]string{}, setup...), sql))
		}
	}
}

// TestAggArmAliasCaptureRowCount states the two verdicts as ROW COUNTS over a
// populated table, which is what a mis-attributed aggregate actually changes.
// differ() covers it too; this names it, so a regression reads as "the
// aggregate moved" rather than as a diff.
func TestAggArmAliasCaptureRowCount(t *testing.T) {
	setup := []string{`CREATE TABLE t1(b, x)`, `INSERT INTO t1 VALUES(10,1),(20,2)`}
	for _, c := range []struct {
		sql  string
		rows int
	}{
		{`SELECT (SELECT sum((SELECT x AS c))) FROM t1`, 1},
		{`SELECT (SELECT sum((SELECT x AS x))) FROM t1`, 1},
		{`SELECT (SELECT sum((SELECT x AS c GROUP BY c))) FROM t1`, 1},
		{`SELECT (SELECT sum((SELECT x AS c UNION SELECT 1234 ORDER BY c))) FROM t1`, 1},
		{`SELECT (SELECT sum((SELECT 5 AS x WHERE x=5))) FROM t1`, 2},
		{`SELECT (SELECT sum((SELECT 7 AS x ORDER BY x))) FROM t1`, 2},
		{`SELECT (SELECT sum((SELECT 7 AS x UNION SELECT 8 ORDER BY x))) FROM t1`, 2},
	} {
		res := run(t, "musql", append(append([]string{}, setup...), c.sql))
		last := res[len(res)-1]
		got, _ := last["rows"].([]any)
		if last["kind"] != "rows" || len(got) != c.rows {
			t.Errorf("%s: want %d row(s), got %v", c.sql, c.rows, last)
		}
	}
}

// TestAggArmAliasCaptureMatrix sweeps the rule's whole input space rather than
// its named cases: every (aggregate, aliased select-list item, clause the alias
// name is referenced from, compound or not) combination, over an empty and a
// populated table. Any of these that 3.53.3 rejects is rejected in lockstep by
// differ(); what matters is that none of them DIVERGES.
func TestAggArmAliasCaptureMatrix(t *testing.T) {
	bodies := []string{
		`SELECT x AS c`,
		`SELECT x AS x`,
		`SELECT 5 AS c`,
		`SELECT b AS c`,
		`SELECT x+1 AS c`,
	}
	tails := []string{
		``,
		` WHERE c IS NOT NULL`,
		` WHERE c=x`,
		` GROUP BY c`,
		` ORDER BY c`,
		` LIMIT 1`,
		` UNION SELECT 1234 ORDER BY c`,
		` UNION ALL SELECT 1234 ORDER BY 1`,
		` UNION SELECT b ORDER BY c`,
	}
	// min()/max() are deliberately absent: isMinMaxAggName (sql_group.go)
	// refuses to claim one through this collector at all, so the whole family
	// DECLINES -- pinned as such by TestAggArmMinMaxStillDeclines below.
	aggs := []string{"sum", "total", "count", "avg", "group_concat"}
	schema := []string{`CREATE TABLE t1(b, x)`}
	rows := append(append([]string{}, schema...), `INSERT INTO t1 VALUES(10,1),(20,2),(30,NULL)`)
	for _, setup := range [][]string{schema, rows} {
		for _, agg := range aggs {
			for _, body := range bodies {
				for _, tail := range tails {
					sql := fmt.Sprintf(`SELECT (SELECT %s((%s%s))) FROM t1`, agg, body, tail)
					differ(t, sql, append(append([]string{}, setup...), sql))
				}
			}
		}
	}
}

// TestAggArmMinMaxStillDeclines pins min/max aggregates in FROM-less subqueries.
// These are intentionally unclaimed because a min/max hoist is not reachable through
// this collector's path. Declining with an error is correct; the alternative would be
// a per-row answer instead of a single aggregated result.
func TestAggArmMinMaxStillDeclines(t *testing.T) {
	setup := []string{`CREATE TABLE t1(b, x)`, `INSERT INTO t1 VALUES(10,1),(20,2),(30,NULL)`}
	for _, sql := range []string{
		`SELECT (SELECT max((SELECT x AS c))) FROM t1`,
		`SELECT (SELECT min((SELECT x AS c))) FROM t1`,
		`SELECT (SELECT max((SELECT x))) FROM t1`,
		`SELECT (SELECT max((SELECT x AS c UNION SELECT 1234 ORDER BY c))) FROM t1`,
	} {
		res := run(t, "musql", append(append([]string{}, setup...), sql))
		if res[len(res)-1]["kind"] != "error" {
			t.Errorf("min/max hoist now answers -- close the pin and fold this case into the matrix above: %s -> %v", sql, res[len(res)-1])
		}
	}
}
