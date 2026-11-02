package engine

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/internal/libm"
)

// TestMathFunctions checks func.c's math functions through SQL: the numeric
// type each argument must have, the INTEGER ceil/floor/trunc keep, the NULL a
// NaN becomes, log(B,X)'s reading of X, and the arity errors. The libm values
// themselves are internal/libm's to test.
func TestMathFunctions(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "m.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	null := Value{Typ: Null}
	f := func(x float64) Value { return Value{Typ: Float, F: x} }
	i := func(x int64) Value { return Value{Typ: Int, I: x} }
	negZero := math.Copysign(0, -1)
	for _, c := range []struct {
		q    string
		want Value
	}{
		{`pi()`, f(math.Pi)},
		{`ceil(1.5)`, f(2)}, {`ceiling(-1.5)`, f(-1)}, {`ceil(-0.5)`, f(negZero)}, {`ceil(7)`, i(7)},
		{`ceil('7')`, i(7)}, {`ceil(' 7.5 ')`, f(8)}, {`ceil('7x')`, null}, {`ceil(x'37')`, null}, {`ceil(NULL)`, null},
		{`floor(2.9)`, f(2)}, {`floor(-2.1)`, f(-3)}, {`trunc(-2.9)`, f(-2)}, {`trunc('1e2')`, f(100)},
		{`trunc('9223372036854775808')`, f(9223372036854775808)},
		{`ln(1)`, f(0)}, {`ln(0)`, null}, {`ln(-1)`, null}, {`ln('e')`, null}, {`ln(0.5)`, f(libm.Log(0.5))},
		{`log(100)`, f(2)}, {`log10(0.5)`, f(libm.Log10(0.5))}, {`log2(8)`, f(3)}, {`log2(0.75)`, f(libm.Log2(0.75))},
		{`log(2, 8)`, f(3)}, {`log(1, 8)`, null}, {`log(0.5, 8)`, null}, {`log(0, 8)`, null}, {`log(2, 0)`, null},
		{`log(10, '100x')`, f(2)}, {`log(10, 'abc')`, null}, {`log(10, x'313030')`, f(2)}, {`log(10, NULL)`, null},
		{`log('x', 100)`, null}, {`log(NULL, 100)`, null},
		{`exp(1)`, f(libm.Exp(1))}, {`exp('1')`, f(libm.Exp(1))}, {`exp('1y')`, null}, {`exp(710)`, f(math.Inf(1))},
		{`pow(2, 10)`, f(1024)}, {`power(2, 0.5)`, f(libm.Pow(2, 0.5))}, {`pow(-8, 1.0/3)`, null}, {`pow(0, -1)`, f(math.Inf(1))},
		{`pow(2, 'x')`, null}, {`pow(NULL, 2)`, null},
		{`mod(10, -3)`, f(1)}, {`mod(-10, 3)`, f(-1)}, {`mod(1, 0)`, null}, {`mod(-0.0, 5)`, f(negZero)},
		{`sqrt(4)`, f(2)}, {`sqrt(-1)`, null}, {`sqrt('9')`, f(3)},
		{`sin(1)`, f(libm.Sin(1))}, {`cos(1)`, f(libm.Cos(1))}, {`tan(1)`, f(libm.Tan(1))}, {`sin(1e999)`, null},
		{`asin(2)`, null}, {`acos(-0.5)`, f(libm.Acos(-0.5))}, {`atan(1)`, f(libm.Atan(1))},
		{`atan2(5, -1)`, f(libm.Atan2(5, -1))}, {`atan2('5', -1)`, f(libm.Atan2(5, -1))}, {`atan2(x'35', -1)`, null},
		{`sinh(1)`, f(libm.Sinh(1))}, {`cosh(1)`, f(libm.Cosh(1))}, {`tanh(1)`, f(libm.Tanh(1))},
		{`asinh(1)`, f(libm.Asinh(1))}, {`acosh(0.99)`, null}, {`atanh(1)`, f(math.Inf(1))}, {`atanh(1.5)`, null},
		{`degrees(1)`, f(1 * (180 / math.Pi))}, {`radians(180)`, f(math.Pi)},
	} {
		got := mathQuery(t, db, "SELECT "+c.q)
		if got.Typ != c.want.Typ || got.I != c.want.I || math.Float64bits(got.F) != math.Float64bits(c.want.F) {
			t.Errorf("%s = %+v, want %+v", c.q, got, c.want)
		}
	}
	for _, q := range []string{`pi(1)`, `sin()`, `sin(1, 2)`, `log()`, `log(1, 2, 3)`, `pow(2)`, `ceil()`} {
		pg, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		_, _, qerr := pg.QueryArgs("SELECT "+q, nil)
		pg.Close()
		name := q[:strings.IndexByte(q, '(')]
		if qerr == nil || !strings.Contains(qerr.Error(), "wrong number of arguments to function "+name+"()") {
			t.Errorf("%s: %v, want the arity error", q, qerr)
		}
	}
}

// TestCallMathFuncArity covers callMathFunc's own guard, which a call that
// passed the prepare-time check never trips.
func TestCallMathFuncArity(t *testing.T) {
	if _, ok, err := callMathFunc("sin", nil, UTF8); !ok || err == nil {
		t.Errorf("callMathFunc(sin) with no arguments: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := callMathFunc("nope", nil, UTF8); ok {
		t.Errorf("callMathFunc claimed nope()")
	}
	// A BLOB X in a UTF-16 database is read in that encoding, ignoring an odd
	// last byte.
	x := Value{Typ: Blob, S: []byte{'1', 0, '0', 0, '0', 0, 'x'}}
	if v, _, _ := callMathFunc("log", []Value{{Typ: Int, I: 10}, x}, UTF16LE); v.Typ != Float || v.F != 2 {
		t.Errorf("log(10, utf-16 '100') = %+v, want 2.0", v)
	}
}

func mathQuery(t *testing.T, db *Session, q string) Value {
	t.Helper()
	pg, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	_, rows, err := pg.QueryArgs(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: %v", q, rows)
	}
	return rows[0][0]
}
