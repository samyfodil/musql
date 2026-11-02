package engine

import (
	"path/filepath"
	"testing"
)

// TestSeriesProgramsMatchTheLoop holds the compiled program over
// generate_series to the loop it replaces: the same answer, or an error from
// both, for each aggregate, filter and series shape, and served exactly where
// the program can run.
func TestSeriesProgramsMatchTheLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path, `CREATE TABLE t(x)`)
	rp, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	ints := func(xs ...int64) []Value {
		out := make([]Value, len(xs))
		for i, x := range xs {
			out[i] = Value{Typ: Int, I: x}
		}
		return out
	}
	cases := []struct {
		sql  string
		args []Value
		// served: 1 a program answers it, -1 nothing may, 0 either.
		served int
	}{
		{`SELECT count(*) FROM generate_series(1, 1000000)`, nil, 1},
		{`SELECT sum(value) FROM generate_series(1, 1000000)`, nil, 1},
		{`SELECT sum(value), count(*), min(value), max(value), avg(value), total(value) FROM generate_series(-500, 170000, 3)`, nil, 1},
		{`SELECT count(*), sum(value) FROM generate_series(1, 200000) WHERE value % 7 = 3`, nil, 1},
		{`SELECT sum(value * 2 + 1), min(value) FROM generate_series(100000, 1, -1)`, nil, 1},
		{`SELECT count(*), sum(value), min(value) FROM generate_series(5, 1)`, nil, 1},
		{`SELECT count(*), sum(value) FROM generate_series(1, NULL)`, nil, 0},
		{`SELECT sum(value) FROM generate_series(?, ?)`, ints(3, 99999), 1},
		{`SELECT count(*) FROM generate_series(1, ?) WHERE value > ?`, ints(70000, 1234), 1},
		{`SELECT sum(value) FROM generate_series(1, 10, 0)`, nil, 1},
		{`SELECT min(value), max(value) FROM generate_series(9223372036854775800, 9223372036854775807)`, nil, 1},
		{`SELECT count(*) FROM generate_series(-9223372036854775808, 9223372036854775807, 4611686018427387904)`, nil, 1},
		{`SELECT sum(value) FROM generate_series(9223372036854775000, 9223372036854775807)`, nil, -1},
		{`SELECT sum(value / 1.5), max(sqrt(value)), total(value * 0.25) FROM generate_series(1, 100000)`, nil, 1},
		{`SELECT sum(round(value / 1.5)), sum(ln(value - 50000)) FROM generate_series(1, 100000)`, nil, 1},
		{`SELECT sum(DISTINCT round(value / 1.5)) FROM generate_series(1, 20000)`, nil, 0},
		{`SELECT count(*) FROM generate_series(1, 200000) WHERE value IN (3, 5, 70000)`, nil, 0},
		{`SELECT sum(start), sum(stop), sum(step), count(*) FROM generate_series(1, 100000, 7) WHERE value > start + 5`, nil, 1},
		{`SELECT sum(length(value)), sum(length(start)) FROM generate_series(1, 100000)`, nil, 0},
		{`SELECT s, 'sum' FROM (SELECT sum(value) s FROM generate_series(1, 100000))`, nil, 0},
		{`SELECT sum(value) FROM generate_series(1)`, nil, -1},
		{`SELECT count(*) FROM generate_series(1, 100000000)`, nil, -1},
	}
	for _, c := range cases {
		segPeepholesOffForTest = true
		_, want, werr := rp.QueryArgs(c.sql+" -- reference", c.args)
		segPeepholesOffForTest = false
		ResetSegFilterCountersForTest()
		_, got, gerr := rp.QueryArgs(c.sql, c.args)
		served, _ := SegFilterCountersForTest()
		if (werr == nil) != (gerr == nil) || (werr != nil && werr.Error() != gerr.Error()) {
			t.Errorf("%s %v: program error %v, loop error %v", c.sql, c.args, gerr, werr)
			continue
		}
		if g, w := realRows(got), realRows(want); g != w {
			t.Errorf("%s %v:\n program %s\n loop    %s", c.sql, c.args, g, w)
		}
		if JITEnabled() && c.served == 1 && served == 0 {
			t.Errorf("%s %v: not served", c.sql, c.args)
		}
		if JITEnabled() && c.served == -1 && served != 0 {
			t.Errorf("%s %v: served, but should decline", c.sql, c.args)
		}
	}
}
