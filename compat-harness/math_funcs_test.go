// The SQL math functions (SQLITE_ENABLE_MATH_FUNCTIONS) against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// mathFuncCases are statements both engines must answer alike, to the bit:
// each function on INTEGER, REAL, numeric and non-numeric TEXT, BLOB and
// NULL, its domain's edges, and its arity errors.
var mathFuncCases = []string{
	`SELECT pi()`, `SELECT pi(1)`, `SELECT typeof(pi())`,
	`SELECT ceil(1.5)`, `SELECT ceil(-1.5)`, `SELECT ceil(-0.5)`, `SELECT ceil(7)`, `SELECT typeof(ceil(7))`,
	`SELECT ceil('7')`, `SELECT typeof(ceil('7'))`, `SELECT ceil('7.5')`, `SELECT ceil(' 7 ')`, `SELECT ceil('7x')`,
	`SELECT ceil(x'37')`, `SELECT ceil(NULL)`, `SELECT ceil(1e999)`, `SELECT ceil(-1e999)`, `SELECT ceil()`,
	`SELECT ceiling(2.1)`, `SELECT floor(2.9)`, `SELECT floor(-2.1)`, `SELECT floor(-0.0)`, `SELECT trunc(-2.9)`,
	`SELECT trunc(-0.5)`, `SELECT trunc(9223372036854775807)`, `SELECT floor('9223372036854775808')`,
	`SELECT typeof(floor('9223372036854775808'))`, `SELECT trunc('1e2')`, `SELECT typeof(trunc('1e2'))`,
	`SELECT ln(1)`, `SELECT ln(0)`, `SELECT ln(-1)`, `SELECT ln(0.5)`, `SELECT ln('2.718281828459045')`,
	`SELECT ln('e')`, `SELECT ln(x'31')`, `SELECT ln(NULL)`, `SELECT ln(1e999)`, `SELECT ln(5e-324)`,
	`SELECT log(100)`, `SELECT log(1000)`, `SELECT log(0.001)`, `SELECT log10(1e22)`, `SELECT log10(5e-324)`,
	`SELECT log2(8)`, `SELECT log2(0.75)`, `SELECT log2(5e-324)`, `SELECT log2(1e308)`,
	`SELECT log(2, 8)`, `SELECT log(10, 100)`, `SELECT log(1, 100)`, `SELECT log(0.5, 8)`, `SELECT log(0, 8)`,
	`SELECT log(-2, 8)`, `SELECT log(2, 0)`, `SELECT log(2, -8)`, `SELECT log(10, '100x')`, `SELECT log(10, 'abc')`,
	`SELECT log(10, x'313030')`, `SELECT log(10, NULL)`, `SELECT log(NULL, 10)`, `SELECT log('10', '1000')`,
	`SELECT log('x', 100)`, `SELECT log(2, 1e999)`, `SELECT log(1e999, 2)`, `SELECT log()`, `SELECT log(1, 2, 3)`,
	`SELECT exp(0)`, `SELECT exp(1)`, `SELECT exp(-1)`, `SELECT exp(709.78)`, `SELECT exp(710)`, `SELECT exp(-746)`,
	`SELECT exp(-745.1)`, `SELECT exp(1e999)`, `SELECT exp(-1e999)`, `SELECT exp('1')`, `SELECT exp('1y')`,
	`SELECT pow(2, 10)`, `SELECT power(2, 0.5)`, `SELECT pow(-8, 1.0/3)`, `SELECT pow(-2, 3)`, `SELECT pow(-2, -3)`,
	`SELECT pow(0, 0)`, `SELECT pow(0, -1)`, `SELECT pow(-0.0, -1)`, `SELECT pow(1, 1e999)`, `SELECT pow(2, 1e999)`,
	`SELECT pow(0.5, 1e999)`, `SELECT pow(10, 308.5)`, `SELECT pow(10, -330)`, `SELECT pow('2', '3')`,
	`SELECT pow(2, 'x')`, `SELECT pow(NULL, 2)`, `SELECT pow(2)`,
	`SELECT mod(10, 3)`, `SELECT mod(10, -3)`, `SELECT mod(-10, 3)`, `SELECT mod(5.5, -1.5)`, `SELECT mod(1, 0)`,
	`SELECT mod(0, 5)`, `SELECT mod(-0.0, 5)`, `SELECT mod(1e999, 2)`, `SELECT mod(2, 1e999)`, `SELECT mod('5.5', '2')`,
	`SELECT sqrt(4)`, `SELECT sqrt(2)`, `SELECT sqrt(-1)`, `SELECT sqrt(-0.0)`, `SELECT sqrt(1e999)`, `SELECT sqrt('9')`,
	`SELECT sin(0)`, `SELECT sin(1)`, `SELECT sin(-0.5)`, `SELECT sin(1e22)`, `SELECT sin(1e300)`, `SELECT sin(1e999)`,
	`SELECT cos(0)`, `SELECT cos(1)`, `SELECT cos(1e22)`, `SELECT cos(-1e999)`, `SELECT tan(1)`, `SELECT tan(-0.5)`,
	`SELECT tan(1e22)`, `SELECT tan(1.5707963267948966)`,
	`SELECT asin(1)`, `SELECT asin(-1)`, `SELECT asin(1.0000001)`, `SELECT asin(-0.5)`, `SELECT asin(0)`,
	`SELECT acos(1)`, `SELECT acos(-1)`, `SELECT acos(2)`, `SELECT acos(0)`, `SELECT acos(-0.5)`,
	`SELECT atan(1)`, `SELECT atan(1e999)`, `SELECT atan(-1e999)`, `SELECT atan(-0.5)`, `SELECT atan(1e300)`,
	`SELECT atan2(0, 0)`, `SELECT atan2(-0.0, 0)`, `SELECT atan2(0, -0.0)`, `SELECT atan2(-0.0, -1)`, `SELECT atan2(1, 0)`,
	`SELECT atan2(5, -1)`, `SELECT atan2(5.5, 10)`, `SELECT atan2(1e999, 1e999)`, `SELECT atan2(-1e999, -1e999)`,
	`SELECT atan2(1, 1e999)`, `SELECT atan2(1, -1e999)`, `SELECT atan2('5', '-1')`, `SELECT atan2(1)`,
	`SELECT sinh(1)`, `SELECT sinh(-0.5)`, `SELECT sinh(710)`, `SELECT sinh(711)`, `SELECT sinh(1e-300)`,
	`SELECT cosh(1)`, `SELECT cosh(-0.5)`, `SELECT cosh(710)`, `SELECT cosh(711)`, `SELECT tanh(1)`, `SELECT tanh(-0.5)`,
	`SELECT tanh(25)`, `SELECT tanh(1e999)`, `SELECT tanh(-0.0)`,
	`SELECT asinh(1)`, `SELECT asinh(-0.5)`, `SELECT asinh(1e300)`, `SELECT asinh(1e999)`,
	`SELECT acosh(1)`, `SELECT acosh(1.5)`, `SELECT acosh(0.99)`, `SELECT acosh(1e300)`,
	`SELECT atanh(0)`, `SELECT atanh(-0.5)`, `SELECT atanh(1)`, `SELECT atanh(-1)`, `SELECT atanh(1.5)`,
	`SELECT degrees(1)`, `SELECT degrees(pi())`, `SELECT radians(180)`, `SELECT radians(1)`, `SELECT degrees(1e308)`,
	`SELECT radians('90')`, `SELECT degrees(x'31')`,
	`SELECT sin()`, `SELECT sin(1, 2)`, `SELECT ln(1, 2)`, `SELECT mod(1)`,
}

func TestMathFunctionsMatchCSQLite(t *testing.T) {
	db, sdb := mathEngines(t)
	for _, s := range mathFuncCases {
		mathCompare(t, db, sdb, s, false)
	}
}

// ibmMathFuncs are the functions glibc takes from IBM's library, which
// misrounds by an ulp on a fraction of a percent of inputs where musql's are
// correctly rounded (internal/libm's coremath.go).
var ibmMathFuncs = map[string]bool{"sin": true, "cos": true, "tan": true, "asin": true, "acos": true, "atan": true, "atan2": true}

// TestMathFunctionsRandomInputs runs every function on seeded random inputs
// over its domain and beyond.
func TestMathFunctionsRandomInputs(t *testing.T) {
	db, sdb := mathEngines(t)
	rng := rand.New(rand.NewSource(20261102))
	n := 300
	if testing.Short() {
		n = 60
	}
	unif := func(a, b float64) float64 { return a + (b-a)*rng.Float64() }
	bits := func() float64 {
		for {
			if f := math.Float64frombits(rng.Uint64()); !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f
			}
		}
	}
	any := func() float64 {
		switch rng.Intn(4) {
		case 0:
			return bits()
		case 1:
			return unif(-1, 1)
		case 2:
			return unif(-1000, 1000)
		}
		return unif(-1e-6, 1e-6)
	}
	for _, f := range []struct {
		name string
		gen  func() float64
	}{
		{"exp", func() float64 { return unif(-750, 715) }},
		{"ln", func() float64 { return math.Abs(any()) }},
		{"log", func() float64 { return math.Abs(any()) }},
		{"log2", func() float64 { return math.Abs(any()) }},
		{"sqrt", any}, {"sin", any}, {"cos", any}, {"tan", any},
		{"asin", func() float64 { return unif(-1.1, 1.1) }},
		{"acos", func() float64 { return unif(-1.1, 1.1) }},
		{"atan", any}, {"sinh", func() float64 { return unif(-720, 720) }},
		{"cosh", func() float64 { return unif(-720, 720) }}, {"tanh", func() float64 { return unif(-30, 30) }},
		{"asinh", any}, {"acosh", func() float64 { return 1 + math.Abs(any()) }},
		{"atanh", func() float64 { return unif(-1.1, 1.1) }},
		{"ceil", any}, {"floor", any}, {"trunc", any}, {"degrees", any}, {"radians", any},
	} {
		for range n {
			mathCompare(t, db, sdb, fmt.Sprintf("SELECT %s(%s)", f.name, sqliteFloatLiteral(f.gen())), ibmMathFuncs[f.name])
		}
	}
	for _, f := range []string{"pow", "mod", "atan2", "log"} {
		for range n {
			x, y := any(), any()
			if f == "pow" && rng.Intn(2) == 0 {
				x, y = math.Abs(x), unif(-30, 30)
			}
			if f == "log" {
				x, y = math.Abs(x), math.Abs(y)
			}
			mathCompare(t, db, sdb, fmt.Sprintf("SELECT %s(%s, %s)", f, sqliteFloatLiteral(x), sqliteFloatLiteral(y)), ibmMathFuncs[f])
		}
	}
}

func mathEngines(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	db, err := engine.Create(filepath.Join(t.TempDir(), "math.musq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sdb.Close() })
	return db, sdb
}

// mathCompare runs one single-value SELECT on both engines and requires the
// same error or the same value, a REAL to the bit (the sign of a zero
// included); nearUlp lets a REAL be an ulp from C's.
func mathCompare(t *testing.T, db *engine.Session, sdb *sql.DB, q string, nearUlp bool) {
	t.Helper()
	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	_, rows, merr := pager.Query(q)
	var cv any
	cerr := sdb.QueryRow(q).Scan(&cv)
	switch {
	case (merr == nil) != (cerr == nil):
		t.Errorf("%s: musql err=%v, C err=%v (%v)", q, merr, cerr, cv)
		return
	case merr != nil:
		if want := "engine: " + cerr.Error(); merr.Error() != want && merr.Error() != cerr.Error() {
			t.Errorf("%s: musql err %q, C err %q", q, merr, cerr)
		}
		return
	case len(rows) != 1 || len(rows[0]) != 1:
		t.Errorf("%s: musql gave %v", q, rows)
		return
	}
	got := rows[0][0]
	if c, ok := cv.(float64); ok && got.Typ == engine.Float {
		if math.Float64bits(got.F) == math.Float64bits(c) {
			return
		}
		if nearUlp && (math.Nextafter(c, math.Inf(1)) == got.F || math.Nextafter(c, math.Inf(-1)) == got.F) {
			return
		}
		t.Errorf("%s:\n  musql: %v (%x)\n  C:     %v (%x)", q, got.F, got.F, c, c)
		return
	}
	if !scalarValueMatchesReal(got, cv) {
		t.Errorf("%s:\n  musql: %s\n  C:     %#v", q, describeEngineValue(got), cv)
	}
}
