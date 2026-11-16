package engine

import (
	"fmt"
	"testing"
)

// TestHashAggDenseMatchesSets holds a hash GROUP BY keeping its groups'
// accumulators as values (denseAggPlan) to the one keeping a set per group,
// over sums that overflow, sums whose order changes their rounding,
// infinities, NULLs, text arguments and every group interleaved, so each
// row loads a different group, and enough groups to fill several of the
// dense values' chunks.
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
		// A group per row: past the dense values' first chunks.
		`SELECT id, sum(i), total(r), count(s) FROM t GROUP BY id`,
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

// TestHashAggIntKeysMatchBytes holds a hash GROUP BY keying its groups of
// INTEGERs and NULLs by value (hashAggIntKey) to the same keyed by bytes:
// an integer-valued REAL, -0.0 among them, is its INTEGER's group, any other
// REAL, a TEXT and a REAL past int64 are groups of their own, a key of five
// columns is keyed by bytes, and enough groups for many of the dense
// aggregate's chunks (denseChunkOf).
func TestHashAggIntKeysMatchBytes(t *testing.T) {
	stmts := []string{`CREATE TABLE t (id INTEGER PRIMARY KEY, a, b, c, d, e, n INT, r REAL)`}
	keys := []string{"5", "5.0", "5.5", "NULL", "'5'", "'x'", "9223372036854775807", "-0.0", "0", "1e20", "-7", "-7.0"}
	for i := range 3000 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %s, %s, %d, %d, %d, %d.25)`, i,
			keys[i%len(keys)], keys[(i/len(keys))%len(keys)], keys[(i*7)%len(keys)], i%11, i%3, i, i))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT a, count(*), sum(n), total(r), avg(n) FROM t GROUP BY a`,
		`SELECT a, b, count(*), sum(n) FROM t GROUP BY a, b`,
		`SELECT a, b, c, d, count(*), sum(r) FROM t GROUP BY a, b, c, d`,
		`SELECT a, b, c, d, e, count(*), sum(n) FROM t GROUP BY a, b, c, d, e`,
		`SELECT id % 1500, id % 7, count(*), sum(n), avg(r) FROM t GROUP BY id % 1500, id % 7`,
		`SELECT n, sum(r) FROM t GROUP BY n HAVING sum(r) > 10`,
		`SELECT a, group_concat(id), min(r), max(n) FROM t WHERE id < 200 GROUP BY a`,
		`SELECT b, a, count(*) FROM t GROUP BY b, a ORDER BY count(*) DESC, 1, 2 LIMIT 20`,
	}
	ask := func(q string, off bool) string {
		hashAggIntKeysOffForTest = off
		defer func() { hashAggIntKeysOffForTest = false }()
		_, rows, err := p.plain(q)
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprint(typedRows(rows))
	}
	for _, q := range queries {
		if want, got := ask(q, true), ask(q, false); got != want {
			t.Errorf("%s:\n by value %.400s\n by bytes %.400s", q, got, want)
		}
	}
}
