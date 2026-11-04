package engine

import (
	"fmt"
	"testing"
)

// TestColumnarGroupSeries: a GROUP BY over generate_series, and a CAST in a
// lowered body, answer as the loop does, and the bulk walk takes the shapes
// it should: computed INTEGER and REAL keys, math in the key, a CAST of a
// REAL to an INTEGER, the key in the output. A -0.0 or NULL key, an
// aggregate it does not sum, and a cast it does not lower are left to the
// loop.
func TestColumnarGroupSeries(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, x REAL, i INTEGER)`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, x REAL)`,
		`INSERT INTO u SELECT value, CASE WHEN value % 331 = 0 THEN 1e300 ELSE (value % 200 - 100) * 0.37 END FROM generate_series(1, 3000)`}
	for k := 1; k <= 3000; k++ {
		x := fmt.Sprint(float64(k%200-100) * 0.37)
		switch {
		case k%97 == 0:
			x = "NULL"
		case k%331 == 0:
			x = "1e300"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %s, %d)`, k, x, k%50-25))
	}
	p := newSegPair(t, stmts...)
	// bulk: 1 the walk must answer, 0 it must not, -1 either.
	cases := []struct {
		sql  string
		bulk int
	}{
		{`SELECT sum(value) FROM generate_series(1, 200000) GROUP BY round(log(value))`, 1},
		{`SELECT sum(value) FROM generate_series(1, 200000) GROUP BY cast(round(log(value)) AS INT)`, 1},
		{`SELECT sum(value) FROM generate_series(1, 200000) GROUP BY value % 7`, 1},
		{`SELECT value % 5, count(*), min(value), max(value), sum(value) FROM generate_series(-1000, 1000, 3) GROUP BY value % 5`, 1},
		{`SELECT round(value / 7.0), count(*), sum(value) FROM generate_series(1, 5000) GROUP BY round(value / 7.0)`, 1},
		{`SELECT cast(value * 1.5 AS INTEGER) % 4, count(*) FROM generate_series(1, 3000) GROUP BY cast(value * 1.5 AS INTEGER) % 4`, 1},
		{`SELECT ln(value) * 2, count(*) FROM generate_series(1, 300) GROUP BY ln(value) * 2`, 1},
		{`SELECT exp(value % 9), max(value) FROM generate_series(1, 9000) GROUP BY exp(value % 9)`, 1},
		{`SELECT (value % 2) * -1.0, count(*) FROM generate_series(1, 50) GROUP BY (value % 2) * -1.0`, 0}, // -0.0
		{`SELECT log(value - 10), count(*) FROM generate_series(1, 100) GROUP BY log(value - 10)`, 0},           // NULL
		{`SELECT avg(value) FROM generate_series(1, 100) GROUP BY value % 3`, 0},
		{`SELECT cast(value AS TEXT), count(*) FROM generate_series(1, 30) GROUP BY cast(value AS TEXT)`, 0},
		{`SELECT count(*) FROM generate_series(1, 0) GROUP BY value % 3`, -1},
		{`SELECT sum(value) FROM generate_series(1, 1000) WHERE value > 50 GROUP BY value % 3`, -1},
		{`SELECT sum(cast(value * 1.5 AS INTEGER)) FROM generate_series(1, 100000)`, -1},
		{`SELECT sum(cast(value / 3.0 AS INTEGER)), max(cast(value * 2.5 AS INT)) FROM generate_series(1, 1000)`, -1},
		{`SELECT count(*) FROM generate_series(1, 1000) WHERE cast(value / 3.0 AS INT) = 7`, -1},
		{`SELECT sum(cast(x AS INTEGER)) FROM t WHERE x < 1000`, -1},
		{`SELECT max(cast(x AS INTEGER)), min(cast(-x AS INTEGER)), count(cast(x AS INT)) FROM t`, -1},
		{`SELECT sum(cast(x * 2 AS INTEGER)), count(*) FROM t WHERE x > 0 AND x < 1000`, -1},
		{`SELECT count(*) FROM t WHERE cast(x AS INT) > 3`, -1},
		{`SELECT sum(cast(i AS REAL)), sum(cast(i * 0.5 AS INTEGER)) FROM t`, -1},
		{`SELECT cast(i / 4.0 AS INTEGER), count(*), sum(i) FROM t GROUP BY cast(i / 4.0 AS INTEGER)`, 1},
		{`SELECT round(i / 3.0), count(*) FROM t GROUP BY round(i / 3.0)`, -1},
	}
	for _, c := range cases {
		want := typedRows(p.mustPlain(c.sql))
		before := segGroupBulkHits.Load()
		got := typedRows(p.mustFast(c.sql))
		ran := segGroupBulkHits.Load() > before
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s:\n fast  %v\n plain %v", c.sql, got, want)
		}
		if JITEnabled() && c.bulk >= 0 && ran != (c.bulk == 1) {
			t.Errorf("%s: bulk ran=%v", c.sql, ran)
		}
	}
	// An in-place CAST of a REAL lowers in any body: these are compiled
	// programs, not the loop. (u has no NULL, which a REAL block's program
	// declines.)
	for _, q := range []string{
		`SELECT sum(cast(value * 1.5 AS INTEGER)) FROM generate_series(1, 100000)`,
		`SELECT count(*) FROM generate_series(1, 1000) WHERE cast(value / 3.0 AS INT) = 7`,
		`SELECT sum(cast(x AS INTEGER)) FROM u WHERE x < 1000`,
		`SELECT count(*) FROM u WHERE cast(x AS INT) > 3`,
	} {
		want := typedRows(p.mustPlain(q))
		served := segFilterServed.Load()
		got := typedRows(p.mustFast(q))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s:\n fast  %v\n plain %v", q, got, want)
		}
		if JITEnabled() && segFilterServed.Load() == served {
			t.Errorf("%s: not served", q)
		}
	}
}
