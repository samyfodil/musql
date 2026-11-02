package compat

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// realEngines opens musql and C over the same tables of REALs, written so
// musql holds them in segments: the program JIT's REAL blocks.
func realEngines(t *testing.T) (mq, c *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	var err error
	if mq, err = sql.Open(driver.DriverName, filepath.Join(dir, "m.musq")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mq.Close() })
	mq.SetMaxOpenConns(1)
	if c, err = sql.Open("sqlite3", filepath.Join(dir, "c.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetMaxOpenConns(1)
	rng := rand.New(rand.NewSource(20261101))
	var vals []string
	for i := 0; i < 3000; i++ {
		x := (rng.Float64() - 0.5) * math.Pow(10, float64(rng.Intn(9)-3))
		y := (rng.Float64() - 0.5) * 20
		switch rng.Intn(12) {
		case 0:
			x = 0
		case 1:
			y = 0
		case 2:
			x = float64(rng.Intn(7) - 3)
		}
		vals = append(vals, fmt.Sprintf("(%s, %s, %d)", realLit(x), realLit(y), rng.Intn(2000)-1000))
	}
	for _, db := range []*sql.DB{mq, c} {
		for _, s := range []string{
			`CREATE TABLE t(x REAL, y REAL, i INTEGER)`,
			`INSERT INTO t VALUES ` + strings.Join(vals, ","),
			`CREATE TABLE z(x REAL)`,
			`INSERT INTO z VALUES (0.0), (-0.0), (1e308), (1e308), (-1e308), (5e-324), (-5e-324), (0.0)`,
			`CREATE TABLE f(x REAL)`,
			`INSERT INTO f VALUES (1e308), (1e308), (-1e308), (2.5)`,
		} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(s, err)
			}
		}
	}
	return mq, c
}

// realLit is x as a literal both engines read to the same double.
func realLit(x float64) string { return fmt.Sprintf("%.17g", x) }

// realRow is a statement's one row, each column read as SQLite reports it.
func realRow(t *testing.T, db *sql.DB, q string, args []any) ([]any, error) {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		return nil, rows.Err()
	}
	out := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// sameValue compares two scanned values, a REAL to the bit, or within near
// of each other relatively.
func sameValue(a, b any, near float64) bool {
	fa, aok := a.(float64)
	fb, bok := b.(float64)
	if aok && bok {
		return math.Float64bits(fa) == math.Float64bits(fb) || math.Abs(fa-fb) <= near*math.Abs(fb)
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// ibmCall finds a call to one of the functions glibc takes from IBM's
// library, which misround an ulp on a fraction of a percent of inputs where
// musql's are correctly rounded: an aggregate over thousands of them can
// differ from C's in its last bits. That musql's JIT answers them exactly as
// its loop does is TestRealProgramsMatchTheLoop's.
var ibmCall = regexp.MustCompile(`\b(sin|cos|tan|asin|acos|atan|atan2)\(`)

// TestJITRealMatchesC runs REAL arithmetic, comparisons and math functions
// through the program JIT and holds every answer to C's, to the bit, and
// each statement to having been served by it.
func TestJITRealMatchesC(t *testing.T) {
	mq, c := realEngines(t)
	type q struct {
		sql  string
		args []any
	}
	var qs []q
	for _, e := range []string{
		"x", "y", "x*y", "x/y", "x-y", "x+i", "i*1.5", "-x", "x*-1", "abs(x)", "abs(x-y)",
		"sqrt(x)", "ln(x)", "log(x)", "log10(x)", "log2(x)", "log(2, x)", "log(x, y)", "log(y, 100)",
		"exp(x)", "exp(-x)", "exp(y)", "pow(x, y)", "pow(x, 2.5)", "pow(abs(x), y)", "power(y, 3)", "pow(2, y)",
		"sin(x)", "cos(x)", "sin(y)", "cos(y*1000)", "sin(x*1e6)", "tan(x)", "tan(y/4)", "atan(x)", "atan(y*100)",
		"asin(x/1000)", "acos(y/10)", "atan2(x, y)", "atan2(y, x*1000)", "sinh(y)", "cosh(x/100)", "tanh(y)",
		"asinh(x)", "acosh(abs(x)+1)", "atanh(y/10.5)", "mod(x, 7)", "mod(y, x)", "mod(x, y-y)", "mod(i, 2.5)",
		"floor(x)", "ceil(x)", "ceiling(y)", "trunc(x)", "floor(i)", "radians(x)", "degrees(y)", "pi()*x",
		"sqrt(i)", "exp(i/1000.0)", "x/0", "x/(y-y)", "(x+y)/(x-y)",
	} {
		for _, agg := range []string{"sum", "total", "avg", "min", "max", "count"} {
			qs = append(qs, q{sql: fmt.Sprintf("SELECT %s(%s) FROM t", agg, e)})
		}
	}
	for _, w := range []string{
		"x > 0.5", "x < y", "x <= y", "x = 0", "x != 0", "x >= -1.5 AND y < 2", "x > 1 OR y < -1",
		"sqrt(x) > 1", "NOT (sqrt(x) > 1)", "ln(x) IS NULL", "ln(x) IS NOT NULL", "x BETWEEN -1 AND 1",
		"x/y > 1", "x/y IS NULL", "exp(x) < 2", "sin(x) > 0", "pow(x, y) > 1", "i > 0 AND x > 0",
		"x > 0 = (y > 0)", "x*2 > i", "x > 3", "x < -2", "abs(x) < 1e-3",
	} {
		qs = append(qs, q{sql: "SELECT count(*), sum(x), min(y), max(x*y) FROM t WHERE " + w})
	}
	for _, a := range [][]any{{0.5}, {int64(1)}, {int64(-3)}, {2.25}} {
		qs = append(qs,
			q{"SELECT count(*), sum(y) FROM t WHERE x > ?", a},
			q{"SELECT count(*) FROM t WHERE x < ? AND y > 0", a},
			q{"SELECT sum(x * ?) FROM t", a},
			q{"SELECT max(pow(abs(x), ?)) FROM t", a})
	}
	for _, s := range []string{
		"SELECT min(x), max(x), sum(x), total(x), avg(x), count(x) FROM z",
		"SELECT min(-x), max(-x), min(x*0), max(x*0) FROM z",
		"SELECT sum(x), total(x), avg(x) FROM f",
		"SELECT sum(x*10), min(x*10), max(x*-10) FROM f",
		"SELECT sum(x), avg(x) FROM t WHERE x > 1e300",
		"SELECT total(x), count(x), min(x) FROM t WHERE x > 1e300",
	} {
		qs = append(qs, q{sql: s})
	}

	unserved := 0
	for _, x := range qs {
		engine.ResetSegFilterCountersForTest()
		got, merr := realRow(t, mq, x.sql, x.args)
		served, _ := engine.SegFilterCountersForTest()
		want, cerr := realRow(t, c, x.sql, x.args)
		if (merr == nil) != (cerr == nil) {
			t.Errorf("%s %v: musql err %v, C err %v", x.sql, x.args, merr, cerr)
			continue
		}
		if merr != nil {
			continue
		}
		near := 0.0
		if ibmCall.MatchString(x.sql) {
			near = 1e-12
		}
		for i := range want {
			if i >= len(got) || !sameValue(got[i], want[i], near) {
				t.Errorf("%s %v: musql %v, C %v", x.sql, x.args, got, want)
				break
			}
		}
		if served == 0 {
			unserved++
			t.Logf("not served by the program JIT: %s %v", x.sql, x.args)
		}
	}
	// Every statement here is one the program lowers; the few that decline
	// are ones a NULL or an inexact parameter sends back to the loop.
	if unserved > len(qs)/10 {
		t.Errorf("%d of %d statements were not served by the program JIT", unserved, len(qs))
	}
}
