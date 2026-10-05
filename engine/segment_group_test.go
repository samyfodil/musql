package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestColumnarGroupByMatchesTheLoop verifies columnar GROUP BY semantics.
func TestColumnarGroupByMatchesTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, j INTEGER, v INTEGER, s TEXT, n INTEGER)`}
	rng := rand.New(rand.NewSource(31))
	for i := 1; i <= 900; i++ {
		n := fmt.Sprintf("%d", rng.Intn(50))
		if i%29 == 0 {
			n = "NULL" // a column the driver must refuse
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d,%d,'s%d',%s)`,
			i, rng.Intn(6), rng.Intn(3), rng.Intn(10000)-5000, i%11, n))
	}
	p := newSegPair(t, stmts...)

	cases := []struct {
		sql        string
		wantServed bool
	}{
		{`SELECT k, count(*) FROM t GROUP BY k`, true},
		{`SELECT k, count(*) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, j, count(*), sum(v) FROM t GROUP BY k, j ORDER BY k, j`, true},
		{`SELECT k, sum(v), count(v) FROM t GROUP BY k`, true},
		{`SELECT k, avg(v) FROM t GROUP BY k`, true},
		{`SELECT id, count(*) FROM t GROUP BY id`, true}, // rowid-alias key
		{`SELECT k, max(v) FROM t GROUP BY k`, true},
		{`SELECT k, min(v), max(v) FROM t GROUP BY k`, true},
		{`SELECT k, max(v), s FROM t GROUP BY k`, false},
		{`SELECT k, count(*) FROM t GROUP BY k HAVING count(*) > 10`, false},
		// same design paying off that count(DISTINCT v) below already showed: the
		// driver reimplements nothing, it replaces the ROW SOURCE and calls the
		// same hashAggStepRow. What used to make these wrong was reading a TEXT
		// column as an int64 block; segColReader reads the cells and heap and
		// produces exactly the Value segment.Value would, so there is nothing
		// left to disagree about. Their answers are compared against the loop's
		// above, row by row, as every case here is.
		{`SELECT s, count(*) FROM t GROUP BY s`, true},
		{`SELECT k, sum(s) FROM t GROUP BY k`, true},
		// NULL-bearing key:
		{`SELECT n, count(*) FROM t GROUP BY n`, false},
		// NULL-bearing argument:
		{`SELECT k, sum(n) FROM t GROUP BY k`, false},
		// DESC takes the sorter drain:
		{`SELECT k, count(*) FROM t GROUP BY k ORDER BY k DESC`, false},
		// expression argument:
		{`SELECT k, sum(v * 2) FROM t GROUP BY k`, false},
		// DISTINCT is SERVED, and correctly -- which is the design paying off.
		// The driver reimplements no aggregate; it replaces the row source and
		// calls the same hashAggStepRow, so count(DISTINCT v) keeps whatever
		// semantics aggStep already gave it. This case was written expecting a
		// decline and the test disagreed.
		{`SELECT k, count(DISTINCT v) FROM t GROUP BY k`, true},
		// expression key:
		{`SELECT k + 1, count(*) FROM t GROUP BY k + 1`, false},
		// Served for the same reason as the TEXT cases above: group_concat gets
		// its argument as a Value and keeps whatever semantics aggStep gave it.
		{`SELECT k, group_concat(s) FROM t GROUP BY k`, true},
	}

	want := make([][]string, len(cases))
	for i, c := range cases {
		want[i] = typedRows(p.mustPlain(c.sql))
	}
	for i, c := range cases {
		ResetSegFilterCountersForTest()
		got := typedRows(p.mustFast(c.sql))
		served, _ := SegFilterCountersForTest()
		if len(got) != len(want[i]) {
			t.Errorf("%s: %d rows columnar, %d loop", c.sql, len(got), len(want[i]))
			continue
		}
		for r := range got {
			if got[r] != want[i][r] {
				t.Errorf("%s row %d:\n columnar %s\n loop     %s", c.sql, r, got[r], want[i][r])
				break
			}
		}
		if !JITEnabled() {
			continue
		}
		if c.wantServed && served == 0 {
			t.Errorf("%s: the driver never ran, so this compares the loop with itself", c.sql)
		}
		if !c.wantServed && served != 0 {
			t.Errorf("%s: the driver served a shape it must not", c.sql)
		}
	}
}
