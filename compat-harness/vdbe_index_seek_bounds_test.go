package compat

import (
	"fmt"
	"testing"
)

// TestIndexSeekBounds verifies index binary search boundaries.
//   - The table must be seeded past a single index page. At 5000 rows the index
//     is two levels (a 23-cell interior over ~204-cell leaves), so the interior
//     descent is exercised too; a bound that is off by one there drops a whole
//     subtree rather than one row.
func TestIndexSeekBounds(t *testing.T) {
	const n = 5000

	steps := []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, dup INTEGER, txt TEXT)`,
		`CREATE INDEX i_sec ON t(sec)`,
		`CREATE INDEX i_dup ON t(dup)`,
		`CREATE INDEX i_txt ON t(txt)`,
	}
	for i := 1; i <= n; i++ {
		// sec is unique and even, so an odd probe falls strictly between two
		// stored keys; dup repeats every value 50x so a probe lands mid-run.
		steps = append(steps, fmt.Sprintf(
			`INSERT INTO t(id,sec,dup,txt) VALUES(%d,%d,%d,'row-%04d')`,
			i, i*2, i/50, i%97))
	}

	steps = append(steps,
		// below every key, on the very first, on the very last, past the end
		`SELECT id, sec FROM t WHERE sec = -1`,
		`SELECT id, sec FROM t WHERE sec = 0`,
		`SELECT id, sec FROM t WHERE sec = 2`,
		`SELECT id, sec FROM t WHERE sec = 10000`,
		`SELECT id, sec FROM t WHERE sec = 10002`,
		`SELECT id, sec FROM t WHERE sec = 999999`,
		// odd probes fall BETWEEN two stored keys -- the bound must not match
		`SELECT id, sec FROM t WHERE sec = 4999`,
		`SELECT id, sec FROM t WHERE sec = 5001`,
		// duplicate runs: the first, an interior one, the last, one past it
		`SELECT id FROM t WHERE dup = 0 ORDER BY id`,
		`SELECT id FROM t WHERE dup = 37 ORDER BY id`,
		`SELECT id FROM t WHERE dup = 100 ORDER BY id`,
		`SELECT id FROM t WHERE dup = 101 ORDER BY id`,
		// NULL never equals anything
		`SELECT id FROM t WHERE sec = NULL`,
		// text keys: first, interior, last, and a miss
		`SELECT id, txt FROM t WHERE txt = 'row-0000' ORDER BY id`,
		`SELECT id, txt FROM t WHERE txt = 'row-0042' ORDER BY id`,
		`SELECT id, txt FROM t WHERE txt = 'row-0096' ORDER BY id`,
		`SELECT id, txt FROM t WHERE txt = 'row-9999'`,
		// no ORDER BY: the seek drives the scan, so ORDER must match too
		`SELECT id, sec FROM t WHERE sec IN (2, 5000, 10000)`,
		// the seek's rowids feed a join on the other side
		`SELECT a.id, b.id FROM t a JOIN t b ON b.sec = a.sec WHERE a.dup = 3 ORDER BY a.id`,
	)

	driverParity(t, "index-seek-bounds", steps)
}
