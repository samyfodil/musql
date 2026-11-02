package engine

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// TestColumnarGroupBulkMatchesTheLoop checks the block-at-a-time GROUP BY
// against the plain VDBE answer, and that it ran (or declined) as expected.
func TestColumnarGroupBulkMatchesTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, wide INTEGER, v INTEGER, big INTEGER, n INTEGER)`}
	rng := rand.New(rand.NewSource(7))
	for i := 1; i <= 3000; i++ {
		n := fmt.Sprint(rng.Intn(9))
		if i%97 == 0 {
			n = "NULL"
		}
		big := int64(math.MaxInt64 / 4)
		if rng.Intn(2) == 0 {
			big = -big
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d,%d,%d,%s)`,
			i, rng.Intn(13)-6, rng.Int63n(1<<40)-(1<<39), rng.Intn(2_000_001)-1_000_000, big, n))
	}
	p := newSegPair(t, stmts...)

	cases := []struct {
		sql  string
		bulk bool
	}{
		{`SELECT k, count(*) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, count(*), sum(v), count(v) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, sum(v), sum(v), count(*) FROM t GROUP BY k`, true},
		{`SELECT sum(v), k FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT wide, count(*) FROM t GROUP BY wide ORDER BY wide`, true}, // sparse keys
		{`SELECT k, sum(wide) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, count(n) FROM t GROUP BY k ORDER BY k`, false}, // NULLs
		{`SELECT k, avg(v) FROM t GROUP BY k ORDER BY k`, false},
		{`SELECT k, min(v) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, min(v), max(v) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, max(wide), count(*), min(v), sum(v) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT wide, max(v) FROM t GROUP BY wide ORDER BY wide`, true},
		{`SELECT k, max(v), v FROM t GROUP BY k ORDER BY k`, false}, // bare column: anchors
		{`SELECT k, min(n) FROM t GROUP BY k ORDER BY k`, false},    // NULLs
		{`SELECT k, count(DISTINCT v) FROM t GROUP BY k ORDER BY k`, false},
		{`SELECT id, count(*) FROM t GROUP BY id ORDER BY id LIMIT 20`, false}, // rowid key
	}
	// A computed key or a bare column is read from the segments alone, so
	// rows in the delta leave these to the loop.
	segOnly := []struct {
		sql  string
		bulk bool
	}{
		{`SELECT k+1, count(*), sum(v) FROM t GROUP BY k+1`, true},
		{`SELECT k, sum(v) FROM t GROUP BY k+1`, true},
		{`SELECT k*2-1 AS g, min(v), max(wide) FROM t GROUP BY k*2-1`, true},
		{`SELECT k % 3, sum(v), count(*) FROM t GROUP BY k % 3`, true},
		{`SELECT k + wide, count(*) FROM t GROUP BY k + wide`, true},
		{`SELECT v, count(*) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, id, wide, count(*), sum(v) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT n + 1, count(*) FROM t GROUP BY n + 1`, false}, // NULLs
		{`SELECT k / 2.0, count(*) FROM t GROUP BY k / 2.0`, false},
		{`SELECT k+1, count(*) FROM t GROUP BY k+1 ORDER BY 1`, false}, // a sorter drains it
		{`SELECT big * 5, count(*) FROM t GROUP BY big * 5`, false},    // overflow
	}
	check := func(label string, delta bool) {
		for i := range len(cases) + len(segOnly) {
			c := cases[min(i, len(cases)-1)]
			if i >= len(cases) {
				c = segOnly[i-len(cases)]
				c.bulk = c.bulk && !delta
			}
			want := typedRows(p.mustPlain(c.sql))
			before := segGroupBulkHits.Load()
			got := typedRows(p.mustFast(c.sql))
			ran := segGroupBulkHits.Load() > before
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s %s:\n fast  %v\n plain %v", label, c.sql, got, want)
			}
			if JITEnabled() && ran != c.bulk {
				t.Errorf("%s %s: bulk ran=%v", label, c.sql, ran)
			}
		}
	}
	check("segments", false)
	// Rows pending in the delta -- added, replaced and removed: the bulk path
	// merges them (the replaced and removed positions skipped, the log's rows
	// folded in after every segment's), and must agree with the loop.
	p.delta(`INSERT INTO t VALUES(5001, 3, 1, 42, 1, 1)`, `UPDATE t SET v = v + 1 WHERE id % 10 = 0`,
		`DELETE FROM t WHERE id % 7 = 0`, `UPDATE t SET k = -6 WHERE id = 5`)
	check("delta", true)
}
