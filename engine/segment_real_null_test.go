package engine

import (
	"fmt"
	"testing"
)

// TestRealNullColumns: a REAL column holding NULLs is read by a compiled
// program as its block with a NaN for each NULL, which is how the program
// holds a REAL NULL. Aggregates, filters (OR, BETWEEN, a negative literal, an
// INTEGER bound), arithmetic and NULL tests over it answer as the loop does,
// and are compiled programs rather than the loop.
func TestRealNullColumns(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, x REAL, y REAL, k INTEGER)`}
	for i := 1; i <= 4000; i++ {
		x, y := fmt.Sprint(float64(i%300-150)*0.25), fmt.Sprint(float64(i%17)*1.5)
		if i%7 == 0 {
			x = "NULL"
		}
		if i%11 == 0 {
			y = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %s, %s, %d)`, i, x, y, i%9))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT count(*) FROM t WHERE x > 3`,
		`SELECT count(*) FROM t WHERE x <= -2.5 OR y > 10`,
		`SELECT sum(x), count(x), min(x), max(x) FROM t`,
		`SELECT total(x), avg(x) FROM t`,
		`SELECT sum(x * 2 + y) FROM t`,
		`SELECT sum(x) FROM t WHERE y IS NULL`,
		`SELECT count(*) FROM t WHERE x IS NOT NULL AND k = 3`,
		`SELECT count(*) FROM t WHERE x IS NULL`,
		`SELECT sum(DISTINCT x), count(DISTINCT x) FROM t`,
		`SELECT max(abs(x)), min(round(x)) FROM t WHERE k < 5`,
		`SELECT sum(x) FROM t WHERE x BETWEEN -10 AND 10`,
		`SELECT count(*) FROM t WHERE x + y > 4`,
		`SELECT count(*) FROM t WHERE x > -10 AND x < 10`,
		`SELECT count(*) FROM t WHERE y > 3 AND y < 20`,
		`SELECT sum(y) FROM t WHERE y > 3`,
		`SELECT count(*) FROM t WHERE x < -7 OR x > -3`,
	}
	for _, q := range queries {
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
	// Rows in the delta beside the segments: still the loop's answers.
	p.delta(`INSERT INTO t VALUES(9001, NULL, 2.5, 1)`, `UPDATE t SET x = NULL WHERE id % 13 = 0`)
	for _, q := range queries {
		if got, want := typedRows(p.mustFast(q)), typedRows(p.mustPlain(q)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("delta %s:\n fast  %v\n plain %v", q, got, want)
		}
	}
}
