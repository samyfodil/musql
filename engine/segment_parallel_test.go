package engine

import (
	"fmt"
	"testing"
)

// TestSegmentThreadsMatchTheLoop runs the parallelized scans over a table of
// several segments with one and with four goroutines, against the plain VDBE.
func TestSegmentThreadsMatchTheLoop(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w INTEGER)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 300000)
		 INSERT INTO t SELECT i, (i * 7919) % 13, (i * 104729) % 2000001 - 1000000, (i * 31) % 70001 FROM c`)
	queries := []string{
		`SELECT count(*) FROM t WHERE v > 0`,
		`SELECT count(*) FROM t WHERE v > -500000 AND k <> 3`,
		`SELECT sum(v) FROM t WHERE v > 100`,
		`SELECT sum(w) FROM t WHERE w < 1000`,
		`SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k`,
		`SELECT w, count(*) FROM t GROUP BY w ORDER BY w`,
	}
	defer func(n int) { segThreads = n }(segThreads)
	for _, q := range queries {
		want := fmt.Sprint(typedRows(p.mustPlain(q)))
		for _, n := range []int{1, 4} {
			segThreads = n
			if got := fmt.Sprint(typedRows(p.mustFast(q))); got != want {
				t.Errorf("threads=%d %s:\n fast  %.300s\n plain %.300s", n, q, got, want)
			}
		}
	}
}
