package engine

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/internal/fir"
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
		{`SELECT sum(DISTINCT round(value / 1.5)), count(DISTINCT round(value / 1.5)) FROM generate_series(1, 100000)`, nil, 1},
		{`SELECT count(DISTINCT value % 10), sum(DISTINCT value / 3), avg(DISTINCT value % 1000), total(DISTINCT value % 7) FROM generate_series(1, 100000)`, nil, 1},
		{`SELECT count(DISTINCT ln(value % 100)), sum(DISTINCT ln(value % 100)), avg(DISTINCT ln(value - 50)) FROM generate_series(1, 30000)`, nil, 1},
		{`SELECT sum(DISTINCT (0.5 - value % 2) * 0.0), total(DISTINCT (value % 2 - 0.5) * 0.0), count(DISTINCT (value % 2 - 0.5) * 0.0) FROM generate_series(1, 1000)`, nil, 1},
		{`SELECT count(*) FROM generate_series(1, 200000) WHERE value IN (3, 5, 70000)`, nil, 0},
		{`SELECT sum(start), sum(stop), sum(step), count(*) FROM generate_series(1, 100000, 7) WHERE value > start + 5`, nil, 1},
		{`SELECT sum(length(value)), sum(length(start)) FROM generate_series(1, 100000)`, nil, 0},
		{`SELECT s, 'sum' FROM (SELECT sum(value) s FROM generate_series(1, 100000))`, nil, 0},
		{`SELECT sum(rowid), count(rowid) FROM generate_series(1, 1000)`, nil, 0},
		{`SELECT count(*) FROM generate_series(1,1000)a,generate_series(1,1000)b`, nil, 1},
		{`SELECT sum((a.value-1)*1000+b.value)FROM generate_series(1,1000)a,generate_series(1,1000)b`, nil, 1},
		{`SELECT count(*), sum(a.value * b.value), min(b.value - a.value), max(a.step + b.start), total(a.stop) FROM generate_series(1, 300, 7) a, generate_series(-50, 50) b WHERE a.value % 3 = b.value % 3`, nil, 0},
		{`SELECT count(*), sum(b.value) FROM generate_series(1, 0) a, generate_series(1, 10) b`, nil, 1},
		{`SELECT count(*), sum(a.value) FROM generate_series(1, 10) a, generate_series(5, 1) b`, nil, 1},
		{`SELECT sum(DISTINCT a.value * 1000 + b.value % 7), count(DISTINCT b.value / 1.5) FROM generate_series(1,300) a, generate_series(1,300) b`, nil, 1},
		{`SELECT sum(a.rowid + b.value) FROM generate_series(1, 10) a, generate_series(1, 10) b`, nil, 0},
		{`SELECT count(*), sum(b.value) FROM generate_series(1, 300) a, generate_series(1, a.value) b`, nil, -1},
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

// The round() kernel is fnRound with no digits, to the bit: halves, the
// sums that round up on their own, -0.0 and +0.0, and the edges of 2^52.
func TestRoundKernelIsFnRound(t *testing.T) {
	f := buildSegRoundKernel()
	if f == nil {
		t.Fatal("no round kernel")
	}
	xs := []float64{0, math.Copysign(0, -1), 0.5, -0.5, 1.5, -1.5, 2.5, -2.5, 0.49999999999999994, -0.49999999999999994,
		0.3, -0.3, 1e-300, -1e-300, 123456.5, -123456.5, 4503599627370495.5, -4503599627370495.5, 4503599627370496,
		-4503599627370496, 4503599627370497, 9007199254740993, -9007199254740993, 1e300, -1e300,
		math.Inf(1), math.Inf(-1), math.MaxFloat64, -math.MaxFloat64, 5e-324}
	for i := range 20000 {
		xs = append(xs, (float64(i)-10000)*0.37+float64(i%7)*0.5, math.Ldexp(1+float64(i)/20000, i%60-5))
	}
	for _, x := range xs {
		want, err := fnRound([]Value{{Typ: Float, F: x}})
		if err != nil {
			t.Fatal(err)
		}
		got, exited := fir.Eval(f, x)
		if exited || math.Float64bits(got) != math.Float64bits(want.F) {
			t.Errorf("round(%v): kernel %v (%x), fnRound %v (%x)", x, got, math.Float64bits(got), want.F, math.Float64bits(want.F))
		}
	}
	if got, _ := fir.Eval(f, math.NaN()); got == got {
		t.Error("round(NULL) is not NULL")
	}
}
