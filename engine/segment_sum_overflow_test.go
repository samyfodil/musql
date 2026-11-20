package engine

import (
	"fmt"
	"testing"
)

// TestSegSumOverflowInRowOrder checks that a compiled sum raises C's "integer
// overflow" where C's running sum leaves int64, in row order: inside a series
// block or a segment, across them, and in a delta's rows, even when a later
// row brings the total back in range.
func TestSegSumOverflowInRowOrder(t *testing.T) {
	// Row 1 takes the sum to 7 short of the limit, the first row of the
	// second series block (65537) passes it, and the next row comes back.
	const crossing = `CASE value WHEN 1 THEN 9223372036854775800 WHEN 65537 THEN 10 WHEN 65538 THEN -10 ELSE 0 END`
	p := newSegPair(t,
		`CREATE TABLE t(v INT)`,
		`INSERT INTO t SELECT `+crossing+` FROM generate_series(1, 200000)`,
		`CREATE TABLE d(v INT)`,
		`INSERT INTO d SELECT CASE value WHEN 1 THEN 9223372036854775800 ELSE 0 END FROM generate_series(1, 1000)`,
		`CREATE TABLE u(v INT)`,
		`INSERT INTO u SELECT CASE value WHEN 1 THEN 9223372036854775800 ELSE 0 END FROM generate_series(1, 1000)`,
		`CREATE TABLE s(v INT)`,
		`INSERT INTO s SELECT value % 1000 - 500 FROM generate_series(1, 200000)`,
	)
	// d's delta appends rows that pass the limit and come back; u's updates
	// rows in the middle, which run after every segment's.
	p.delta(`INSERT INTO d VALUES (10), (-10)`, `UPDATE u SET v = 10 WHERE rowid = 500`, `UPDATE u SET v = -10 WHERE rowid = 501`)
	queries := []string{
		`SELECT sum(` + crossing + `) FROM generate_series(1, 70000)`,
		`SELECT sum(value) FROM generate_series(4611686018427387904, 4611686018427387904 + 100000)`,
		`SELECT sum(value), count(*) FROM generate_series(4611686018427387904, 4611686018427387904 + 1000)`,
		`SELECT total(` + crossing + `), avg(` + crossing + `) FROM generate_series(1, 70000)`,
		`SELECT sum(value * value) FROM generate_series(3037000400, 3037000600)`,
		`SELECT sum(value) FROM generate_series(1, 200000)`,
		`SELECT sum(v) FROM t`,
		`SELECT sum(v) FROM t WHERE v >= -100`,
		`SELECT sum(v + 0) FROM t`,
		`SELECT sum(v), count(*), max(v) FROM t`,
		`SELECT avg(v), total(v) FROM t`,
		`SELECT sum(v) FROM d`,
		`SELECT sum(v), min(v) FROM d`,
		`SELECT sum(v) FROM u`,
		`SELECT total(v) FROM u`,
		`SELECT sum(v), avg(v), count(*) FROM s`,
		`SELECT sum(v) FROM s WHERE v > 10`,
	}
	for _, q := range queries {
		_, prow, perr := p.plain(q)
		_, frow, ferr := p.fast(q)
		want, got := fmt.Sprint(typedRows(prow), perr), fmt.Sprint(typedRows(frow), ferr)
		if got != want {
			t.Errorf("%s:\n fast  %s\n plain %s", q, got, want)
		}
	}
	// The latched error is the compiled run's answer, not the loop's. The
	// same shape without an overflow is served, so a count that does not move
	// either way says the run raised it.
	served := segFilterServed.Load()
	p.mustFast(`SELECT sum(v) FROM s`)
	if JITEnabled() && segFilterServed.Load() == served {
		t.Fatal("sum(v) over s: not served")
	}
	served = segFilterServed.Load()
	declined := segFilterDeclined.Load()
	if _, _, err := p.fast(`SELECT sum(v) FROM t`); err == nil {
		t.Fatal("sum(v) over t: no error")
	}
	if JITEnabled() && (segFilterDeclined.Load() != declined || segFilterServed.Load() != served) {
		t.Errorf("sum(v) over t went back to the loop")
	}
}
