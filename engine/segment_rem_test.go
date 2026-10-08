package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestRemainderKernelMatchesTheLoop: "%" lowered to the program JIT's POpRem
// must answer as OP_Remainder does -- C's truncating sign, a divisor of -1
// giving 0 (MinInt64 % -1 included), and a zero divisor, which is SQL NULL,
// declining to the loop rather than answering.
func TestRemainderKernelMatchesTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, d INTEGER, z INTEGER)`}
	rng := rand.New(rand.NewSource(17))
	edges := []string{"-9223372036854775808", "9223372036854775807", "-1", "0", "1", "-7", "7"}
	for i := 1; i <= 1500; i++ {
		v := fmt.Sprint(rng.Intn(20001) - 10000)
		if i%11 == 0 {
			v = edges[i%len(edges)]
		}
		d := []string{"-1", "1", "3", "-3", "7", "10"}[i%6] // never zero
		z := fmt.Sprint(i % 5)                              // zero every fifth row
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %s, %s, %s)`, i, v, d, z))
	}
	p := newSegPair(t, stmts...)
	iv := func(n int64) Value { return Value{Typ: Int, I: n} }
	cases := []struct {
		sql    string
		args   []Value
		served bool
	}{
		{`SELECT count(*) FROM t WHERE v % 7 = 3`, nil, true},
		{`SELECT count(*) FROM t WHERE v % 7 = -3`, nil, true}, // negative dividends keep their sign
		{`SELECT count(*) FROM t WHERE v % -7 = 3`, nil, true},
		{`SELECT count(*) FROM t WHERE v % d = 0`, nil, true}, // d is -1 for a sixth of the rows
		{`SELECT count(*) FROM t WHERE v % -1 = 0`, nil, true},
		{`SELECT sum(v % 13) FROM t`, nil, true},
		{`SELECT sum(id) FROM t WHERE id % ? = 1`, []Value{iv(3)}, true},
		{`SELECT sum(id) FROM t WHERE (v + id) % ? = 1`, []Value{iv(3)}, false}, // v + id overflows on the MaxInt64 rows: declined
		{`SELECT min(v % 1000), max(v % 1000) FROM t`, nil, true},
		{`SELECT count(*) FROM t WHERE v % z = 0`, nil, false}, // a zero divisor: NULL, declined
		{`SELECT count(*) FROM t WHERE v % ? = 0`, []Value{iv(0)}, false},
		{`SELECT count(*) FROM t WHERE v % z IS NULL`, nil, false},
	}
	// A row-returning query takes the row pre-filter, which counts the rows
	// it selected rather than a served statement.
	rowQ := `SELECT id FROM t WHERE v % 9 = 4 ORDER BY id`
	_, wantRows, err := p.plain(rowQ)
	if err != nil {
		t.Fatal(err)
	}
	SegRowsSelectedForTest()
	_, gotRows, err := p.fast(rowQ)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := fmt.Sprint(typedRows(gotRows)), fmt.Sprint(typedRows(wantRows)); g != w {
		t.Errorf("%s:\n filter %.200s\n loop   %.200s", rowQ, g, w)
	}
	if JITEnabled() && SegRowsSelectedForTest() == 0 {
		t.Errorf("%s: the row pre-filter was not used", rowQ)
	}
	for _, c := range cases {
		_, want, err := p.plain(c.sql, c.args...)
		if err != nil {
			t.Fatalf("%s plain: %v", c.sql, err)
		}
		ResetSegFilterCountersForTest()
		_, got, err := p.fast(c.sql, c.args...)
		if err != nil {
			t.Fatalf("%s fast: %v", c.sql, err)
		}
		served, _ := SegFilterCountersForTest()
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s %v:\n kernel %.200s\n loop   %.200s", c.sql, c.args, g, w)
		}
		if JITEnabled() && c.served && served == 0 {
			t.Errorf("%s %v: not served by a kernel", c.sql, c.args)
		}
	}
}

// TestAbsKernelMatchesTheLoop: abs() lowered to POpAbs answers as the builtin
// does, and abs(MinInt64) -- SQLite's integer-overflow error -- declines to the
// VDBE, which raises it. The lowering trusts the name "abs" because no
// application function can take it, which the first check pins.
func TestAbsKernelMatchesTheLoop(t *testing.T) {
	if err := RegisterFunction(ScalarFunction{Name: "ABS", NArg: 1, Fn: func([]Value) (Value, error) { return Value{}, nil }}); err == nil {
		t.Fatal("an application function was registered as abs, which segLowerableFunc would lower as the builtin")
	}
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, w INTEGER)`}
	rng := rand.New(rand.NewSource(19))
	for i := 1; i <= 1200; i++ {
		w := fmt.Sprint(rng.Intn(2000) - 1000)
		if i == 600 {
			w = "-9223372036854775808"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %s)`, i, rng.Intn(20001)-10000, w))
	}
	p := newSegPair(t, stmts...)
	for _, c := range []struct {
		sql    string
		served bool
	}{
		{`SELECT count(*) FROM t WHERE abs(v) < 500`, true},
		{`SELECT count(*) FROM t WHERE abs(v - 300) BETWEEN 10 AND 900`, true},
		{`SELECT sum(abs(v)) FROM t`, true},
		{`SELECT max(abs(v)), min(abs(v)) FROM t`, true},
		{`SELECT count(*) FROM t WHERE abs(w) > 10`, false}, // reaches abs(MinInt64): declined, and an error
	} {
		_, want, werr := p.plain(c.sql)
		ResetSegFilterCountersForTest()
		_, got, gerr := p.fast(c.sql)
		served, _ := SegFilterCountersForTest()
		if (werr == nil) != (gerr == nil) || (werr != nil && werr.Error() != gerr.Error()) {
			t.Errorf("%s: kernel error %v, loop error %v", c.sql, gerr, werr)
			continue
		}
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n kernel %.200s\n loop   %.200s", c.sql, g, w)
		}
		if JITEnabled() && c.served && served == 0 {
			t.Errorf("%s: not served by a kernel", c.sql)
		}
	}
}
