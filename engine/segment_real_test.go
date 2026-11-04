package engine

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// TestRealProgramsMatchTheLoop: REAL arithmetic, comparisons, math kernels
// and REAL aggregates in the program JIT answer exactly as the loop does, to
// the bit, over three segments, which a REAL sum must visit in order.
func TestRealProgramsMatchTheLoop(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, x REAL, y REAL, i INTEGER, n REAL)`,
		`WITH RECURSIVE c(k) AS (SELECT 1 UNION ALL SELECT k+1 FROM c WHERE k < 140000)
		 INSERT INTO t SELECT k, (k % 1000 - 500) * 0.37 + k * 1e-7, sin(k) * 50, k % 777 - 300,
		 CASE WHEN k % 5 = 0 THEN NULL ELSE k * 0.5 END FROM c`,
		`CREATE TABLE u(v)`,
		`WITH RECURSIVE c(k) AS (SELECT 1 UNION ALL SELECT k+1 FROM c WHERE k < 70000) INSERT INTO u SELECT k FROM c`,
		`WITH RECURSIVE c(k) AS (SELECT 1 UNION ALL SELECT k+1 FROM c WHERE k < 70000) INSERT INTO u SELECT k + 0.5 FROM c`,
		`CREATE TABLE z(x REAL)`,
		`INSERT INTO z VALUES (0.0), (-0.0), (1e308), (1e308), (-1e308), (5e-324), (-5e-324), (0.0)`)
	real := func(f float64) Value { return Value{Typ: Float, F: f} }
	integer := func(i int64) Value { return Value{Typ: Int, I: i} }
	cases := []struct {
		sql  string
		args []Value
		// served: 1 a kernel answers it, -1 nothing may, 0 either.
		served int
	}{
		{`SELECT sum(x), total(x), avg(x), min(x), max(x), count(x) FROM t`, nil, 1},
		{`SELECT sum(x*y), min(x/y), max(x-y), sum(x+i), sum(i*0.5) FROM t`, nil, 1},
		{`SELECT sum(sqrt(x)), count(sqrt(x)), min(ln(x)), max(log(x)), avg(log2(x)), sum(log(2, x)) FROM t`, nil, 1},
		{`SELECT sum(exp(x/100)), sum(pow(abs(x), 1.5)), max(pow(x, 2)), sum(power(y, 3)) FROM t`, nil, 1},
		{`SELECT sum(sin(x)), sum(cos(y)), min(sin(x*1e9)), max(cos(x*1e12)) FROM t`, nil, 1},
		{`SELECT sum(floor(x)), sum(ceil(y)), sum(trunc(x)), sum(-x), sum(abs(x)), max(abs(y)) FROM t`, nil, 1},
		{`SELECT sum(tan(x)), sum(atan(y)), sum(asin(y/50)), sum(acos(y/50)), sum(atan2(x, y)) FROM t`, nil, 1},
		{`SELECT sum(sinh(y/10)), sum(cosh(y/10)), sum(tanh(y)), sum(asinh(x)), sum(acosh(abs(x)+1)), sum(atanh(y/51)) FROM t`, nil, 1},
		{`SELECT sum(radians(x)), sum(degrees(y)), sum(pi()*x), sum(floor(i)), sum(sqrt(i)) FROM t`, nil, 1},
		{`SELECT sum(mod(x, 7)), sum(mod(y, x)), sum(mod(i, 3.5)), count(mod(x, y-y)) FROM t`, nil, 1},
		{`SELECT count(*), sum(x) FROM t WHERE x > 0.5`, nil, 1},
		{`SELECT count(*), sum(y) FROM t WHERE x < y AND i > 0`, nil, 1},
		{`SELECT count(*) FROM t WHERE sqrt(x) > 3`, nil, 1},
		{`SELECT count(*) FROM t WHERE ln(x) IS NULL`, nil, 1},
		{`SELECT count(*), max(x) FROM t WHERE x/(y-y) IS NOT NULL OR x > 100`, nil, 1},
		{`SELECT count(*) FROM t WHERE x BETWEEN -1 AND 1`, nil, 1},
		{`SELECT count(*) FROM t WHERE NOT (sqrt(x) > 3)`, nil, -1}, // a NULL the program cannot hold
		{`SELECT count(*), sum(x) FROM t WHERE x > ?`, []Value{real(10.25)}, 1},
		{`SELECT count(*) FROM t WHERE x > ? AND y < ?`, []Value{integer(3), real(0.5)}, 1},
		{`SELECT count(*) FROM t WHERE x > ?`, []Value{integer(1 << 60)}, 0}, // the fixed filter compares it exactly
		{`SELECT sum(y) FROM t WHERE x > ?`, []Value{integer(1 << 60)}, -1},  // not an exact double
		{`SELECT count(*), sum(y) FROM t WHERE x >= ?`, []Value{real(math.NaN())}, -1},
		{`SELECT sum(x * ?) FROM t`, []Value{integer(1<<60 + 1)}, 1},
		{`SELECT sum(n) FROM t`, nil, 1},          // a REAL column with NULLs, as NaNs
		{`SELECT sum(v), max(v) FROM u`, nil, -1}, // INTEGER in one segment, REAL in another
		{`SELECT min(x), max(x), sum(x), total(x), avg(x), count(x) FROM z`, nil, 0},
		{`SELECT min(-x), max(-x), min(x*0), max(x*0) FROM z`, nil, 0},
		{`SELECT count(DISTINCT x), sum(DISTINCT x), avg(DISTINCT x), total(DISTINCT x) FROM z`, nil, 1},
		{`SELECT count(DISTINCT x), sum(DISTINCT y), count(DISTINCT round(y)), sum(DISTINCT round(x)) FROM t`, nil, 1},
		{`SELECT sum(round(x)), min(round(y)), max(round(x * 1e15)), total(round(-x)) FROM t`, nil, 1},
		{`SELECT count(DISTINCT i), sum(DISTINCT i), min(DISTINCT x) FROM t WHERE x > 0.5`, nil, 1},
	}
	check := func() {
		t.Helper()
		for _, c := range cases {
			_, want, werr := p.plain(c.sql, c.args...)
			ResetSegFilterCountersForTest()
			_, got, gerr := p.fast(c.sql, c.args...)
			served, _ := SegFilterCountersForTest()
			if (werr == nil) != (gerr == nil) {
				t.Errorf("%s: kernel error %v, loop error %v", c.sql, gerr, werr)
				continue
			}
			if g, w := realRows(got), realRows(want); g != w {
				t.Errorf("%s %v:\n kernel %s\n loop   %s", c.sql, c.args, g, w)
			}
			if JITEnabled() && c.served == 1 && served == 0 {
				t.Errorf("%s %v: not served", c.sql, c.args)
			}
			if JITEnabled() && c.served == -1 && served != 0 {
				t.Errorf("%s %v: served, but should decline", c.sql, c.args)
			}
		}
	}
	check()
	// A delta beside the segments: a REAL program declines, and the loop
	// answers with the new rows.
	p.delta(`INSERT INTO t VALUES (200001, 1.5, 2.5, 3, 4.5)`)
	for i := range cases {
		if cases[i].served == 1 {
			cases[i].served = 0
		}
	}
	check()
}

// realRows renders rows with every REAL's bits, so -0.0 and 0.0 differ.
func realRows(rows [][]Value) string {
	var b strings.Builder
	for _, r := range rows {
		for _, v := range r {
			if v.Typ == Float {
				fmt.Fprintf(&b, "f%x|", math.Float64bits(v.F))
				continue
			}
			fmt.Fprint(&b, typedRows([][]Value{{v}}))
		}
		b.WriteString(";")
	}
	return b.String()
}

// TestRealRowFilter: a WHERE over REAL columns selects a segment's rows in
// the compiled pre-filter -- comparisons, arithmetic and math kernels, a row
// a kernel cannot judge selected for the loop to decide -- and the rows are
// the loop's.
func TestRealRowFilter(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, x REAL, y REAL, i INTEGER, n REAL)`,
		`WITH RECURSIVE c(k) AS (SELECT 1 UNION ALL SELECT k+1 FROM c WHERE k < 140000)
		 INSERT INTO t SELECT k, (k % 1000 - 500) * 0.37 + k * 1e-7, sin(k) * 50, k % 777 - 300,
		 CASE WHEN k % 5 = 0 THEN NULL ELSE k * 0.5 END FROM c`)
	real := func(f float64) Value { return Value{Typ: Float, F: f} }
	integer := func(i int64) Value { return Value{Typ: Int, I: i} }
	cases := []struct {
		sql      string
		args     []Value
		filtered bool
	}{
		{`SELECT id, x FROM t WHERE x > 180.5`, nil, true},
		{`SELECT id FROM t WHERE x < y AND i > 250`, nil, true},
		{`SELECT id, y FROM t WHERE sqrt(x) > 13.4`, nil, true},
		{`SELECT id FROM t WHERE ln(x) < 0.5`, nil, true},
		{`SELECT id FROM t WHERE pow(x, 2) > 34000 AND y > 0`, nil, true},
		{`SELECT id FROM t WHERE abs(x) < 0.2`, nil, true},
		{`SELECT id FROM t WHERE x BETWEEN -0.5 AND 0.5`, nil, true},
		{`SELECT id FROM t WHERE exp(x/100) > 6`, nil, true},
		{`SELECT id FROM t WHERE x * 2 + 1 > 360`, nil, true},
		{`SELECT id FROM t WHERE x > ?`, []Value{real(184.25)}, true},
		{`SELECT id FROM t WHERE x > ? AND y < ?`, []Value{integer(180), real(-40)}, true},
		{`SELECT id FROM t WHERE x > ?`, []Value{integer(1<<53 + 1)}, false},
		{`SELECT id FROM t WHERE x >= ? AND x < 0`, []Value{real(math.NaN())}, false},
		{`SELECT id FROM t WHERE n > 69990`, nil, false},
	}
	check := func() {
		t.Helper()
		for _, c := range cases {
			_, want, werr := p.plain(c.sql, c.args...)
			SegRowsSelectedForTest()
			_, got, gerr := p.fast(c.sql, c.args...)
			selected := SegRowsSelectedForTest()
			if (werr == nil) != (gerr == nil) {
				t.Errorf("%s: fast error %v, loop error %v", c.sql, gerr, werr)
				continue
			}
			if g, w := realRows(got), realRows(want); g != w {
				t.Errorf("%s %v: %d rows, the loop's %d", c.sql, c.args, len(got), len(want))
			}
			if JITEnabled() && c.filtered && (selected == 0 || selected >= 140000) {
				t.Errorf("%s %v: %d rows selected of 140000", c.sql, c.args, selected)
			}
		}
	}
	check()
	// A delta beside the segments: the rows are still the loop's.
	p.delta(`INSERT INTO t VALUES (200001, 190.5, -45.5, 300, 1.5)`, `DELETE FROM t WHERE id = 1000`)
	for i := range cases {
		cases[i].filtered = false
	}
	check()
}
