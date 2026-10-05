// This file tests aggregates inside window frame clauses (PARTITION BY / ORDER BY)
// written as subqueries.
package compat

import "testing"

// windowSpecAggCases returns test queries for window frame aggregates.
func windowSpecAggCases() []string {
	return []string{
		`SELECT sum(b) OVER (ORDER BY (SELECT sum(x))) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY (SELECT sum(x)) DESC) FROM t1`,
		`SELECT sum(b) OVER (PARTITION BY (SELECT sum(x))) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY EXISTS(SELECT sum(x))) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY (SELECT sum(b+x))) FROM t1`,
		`SELECT x, sum(b) OVER (ORDER BY (SELECT sum(x))) FROM t1`,
		`SELECT sum(b) OVER w FROM t1 WINDOW w AS (ORDER BY (SELECT sum(x)))`,
		`SELECT sum(b) OVER (ORDER BY (SELECT sum(x) FROM t1)) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY (SELECT sum(c) FROM t2)) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY (SELECT count(*))) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY (SELECT max(b) OVER (ORDER BY sum(x)) AS e ORDER BY e)) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY (SELECT max(b) OVER (ORDER BY sum((SELECT x))) AS e ORDER BY e)) FROM t1`,
		`SELECT max(b) OVER( ORDER BY SUM( (SELECT c FROM t2 UNION SELECT x ORDER BY c) ) ) FROM t1`,
		`SELECT sum(b) over( ORDER BY ( SELECT max(b) OVER( ORDER BY sum( (SELECT x AS c UNION SELECT 1234 ORDER BY c) ) ) AS e ORDER BY e ) ) FROM t1`,
	}
}

// TestWindowSpecSubqueryAggregate tests window frame aggregates over empty and populated tables.
func TestWindowSpecSubqueryAggregate(t *testing.T) {
	schema := []string{
		`CREATE TABLE t1(b, x)`,
		`CREATE TABLE t2(c, d)`,
		`CREATE TABLE t3(e, f)`,
	}
	rows := append(append([]string{}, schema...),
		`INSERT INTO t1 VALUES(10,1),(20,2),(30,2)`,
		`INSERT INTO t2 VALUES(5,'p'),(7,'q')`,
	)
	for _, setup := range [][]string{schema, rows} {
		for _, sql := range windowSpecAggCases() {
			stmts := append(append([]string{}, setup...), sql)
			differ(t, sql, stmts)
			res := run(t, "musql", stmts)
			if last := res[len(res)-1]; last["kind"] == "error" {
				t.Errorf("still declining: %s", sql)
			}
		}
	}
}

// TestWindowSpecSubqueryAggregateEmptyTableRowCount is the assertion the whole
// family turns on, stated on its own so a regression names it: over an EMPTY
// table, a re-associating aggregate makes the window query emit ONE row and a
// non-re-associating one makes it emit NONE. A rewrite that got the association
// backwards would still agree with differ() on the populated table.
func TestWindowSpecSubqueryAggregateEmptyTableRowCount(t *testing.T) {
	setup := []string{`CREATE TABLE t1(b, x)`}
	for _, c := range []struct {
		sql  string
		rows int
	}{
		{`SELECT sum(b) OVER (ORDER BY (SELECT sum(x))) FROM t1`, 1},
		{`SELECT sum(b) OVER (ORDER BY (SELECT sum(x) FROM t1)) FROM t1`, 0},
		{`SELECT sum(b) OVER (ORDER BY (SELECT count(*))) FROM t1`, 0},
		{`SELECT sum(b) over( ORDER BY ( SELECT max(b) OVER( ORDER BY sum( (SELECT x AS c UNION SELECT 1234 ORDER BY c) ) ) AS e ORDER BY e ) ) FROM t1`, 1},
	} {
		res := run(t, "musql", append(append([]string{}, setup...), c.sql))
		last := res[len(res)-1]
		got, _ := last["rows"].([]any)
		if last["kind"] != "rows" || len(got) != c.rows {
			t.Errorf("%s: want %d row(s), got %v", c.sql, c.rows, last)
		}
	}
}
