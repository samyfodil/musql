package engine

import (
	"fmt"
	"testing"
)

// TestHashAggDenseMatchesSets holds a hash GROUP BY keeping its groups'
// accumulators as values (denseAggPlan) to the one keeping a set per group,
// over sums that overflow, sums whose order changes their rounding,
// infinities, NULLs, text arguments and every group interleaved, so each
// row loads a different group.
func TestHashAggDenseMatchesSets(t *testing.T) {
	stmts := []string{`CREATE TABLE t (id INTEGER PRIMARY KEY, g INT, h TEXT, i INT, r REAL, s TEXT, big INT)`}
	vals := []string{"9223372036854775807", "-9223372036854775807", "1", "1e308", "-1e308", "0.1", "NULL", "'7'", "'x'", "2.5", "-3", "1e-300"}
	for i := range 600 {
		big := 1 + int64(i%3)*(1<<62)
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d, '%c', %s, %s, %s, %d)`, i, i%37, 'a'+rune(i%5), vals[i%len(vals)], vals[(i*7)%len(vals)], vals[(i*5)%len(vals)], big))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT g, count(*), count(i), sum(i), total(r), avg(r) FROM t GROUP BY g`,
		`SELECT g, h, sum(r), avg(i), total(s), count(s) FROM t GROUP BY g, h`,
		`SELECT h, sum(i) FROM t GROUP BY h HAVING count(*) > 100`,
		`SELECT g, avg(r) + sum(i) FROM t GROUP BY g HAVING sum(r) IS NOT NULL ORDER BY 2 DESC, 1`,
		`SELECT g, total(i) FROM t GROUP BY g ORDER BY total(i), g LIMIT 7`,
		`SELECT g, count(*) FROM t GROUP BY g HAVING g IN (SELECT count(*) % 37 FROM t GROUP BY h)`,
		`SELECT g % 3, sum(r), sum(i) FROM t WHERE i > 0 GROUP BY g % 3`,
		`SELECT g, total(big), avg(big) FROM t GROUP BY g`,
		`SELECT g, sum(big) FROM t WHERE id % 37 < 30 GROUP BY g`,
		`SELECT g, sum(big) FROM t WHERE big > 1 AND g < 5 GROUP BY g`,
	}
	ask := func(q string, off bool) string {
		denseAggOffForTest = off
		defer func() { denseAggOffForTest = false }()
		_, rows, err := p.plain(q)
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprint(typedRows(rows))
	}
	for _, q := range queries {
		if want, got := ask(q, true), ask(q, false); got != want {
			t.Errorf("%s:\n dense %.400s\n sets  %.400s", q, got, want)
		}
	}
}
