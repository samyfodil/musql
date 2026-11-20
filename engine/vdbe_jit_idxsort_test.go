package engine

import (
	"fmt"
	"testing"
)

// TestVMJITTableIndexOrder: a covering automatic index over a table's
// segments, keyed on a join column with repeats and then the other columns
// read -- INTEGERs with and without NULLs, REALs, TEXT -- is built natively in
// the order the VDBE keeps it, so each join answers its rows in the same
// order with the JIT on and off: a key with long runs of repeats (f), and one
// nearly unique to its row (u), whose ties are sorted run by run.
func TestVMJITTableIndexOrder(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	p := newSegPair(t,
		`CREATE TABLE g(a INT, b INT)`,
		`CREATE TABLE f(a INT, n INT, m INT, x, s TEXT)`,
		`INSERT INTO g SELECT value % 40, value % 3 FROM generate_series(1, 300)`,
		`INSERT INTO f SELECT value % 50, (value * 7) % 11, CASE WHEN value % 4 = 0 THEN NULL ELSE (value * 13) % 5 - 2 END,
			CASE value % 6 WHEN 0 THEN NULL WHEN 1 THEN value * 0.5 WHEN 2 THEN 'x' || (value % 3) ELSE value % 4 END,
			char(65 + (value * 17) % 5) FROM generate_series(1, 3000)`,
		`CREATE TABLE u(a INT, n INT, m INT)`,
		`INSERT INTO u SELECT value % 997, (value * 7) % 3, CASE WHEN value % 5 = 0 THEN NULL ELSE (value * 13) % 4 - 2 END FROM generate_series(1, 3000)`)
	queries := []string{
		`select g.a, f.n, f.m from g left join f on f.a = g.a and f.n = g.b + 1 limit 400`,
		`select g.a, f.m, f.x from g left join f on f.a = g.a and f.n = g.b + 1 limit 400`,
		`select g.a, f.s, f.m from g left join f on f.a = g.a and f.n = g.b + 1 limit 400`,
		`select g.a, f.x, f.s, f.m from g left join f on f.a = g.a and f.n = g.b limit 400`,
		`select g.a, f.m, f.n from g left join f on f.a = g.a + 5 limit 400`,
		`select g.a, f.x, f.n from g left join f on f.a = g.a + 5 limit 400`,
		`select g.a, u.n, u.m from g left join u on u.a = g.a limit 900`,
		`select g.a, u.m, u.n from g left join u on u.a = g.a * 7 limit 900`,
		`select g.a, u.m from g left join u on u.a = g.a + 3 and u.n = g.b limit 900`,
	}
	run := func(on bool) []string {
		old := vmJITEnabled
		vmJITEnabled = on
		defer func() { vmJITEnabled = old }()
		var out []string
		for _, q := range queries {
			_, rows, err := p.fast(fmt.Sprintf("%s -- %v", q, on))
			out = append(out, fmt.Sprint(typedRows(rows), err))
		}
		return out
	}
	off, on := run(false), run(true)
	for i := range queries {
		if off[i] != on[i] {
			t.Errorf("%s:\n JIT off %.300s\n JIT on  %.300s", queries[i], off[i], on[i])
		}
	}
}
