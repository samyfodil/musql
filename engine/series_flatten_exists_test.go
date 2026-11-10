package engine_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func seriesDB(t *testing.T) *engine.ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "series.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func seriesInts(t *testing.T, p *engine.ReadOnlyPager, q string) []int64 {
	t.Helper()
	_, rows, err := p.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var out []int64
	for _, r := range rows {
		for _, v := range r {
			out = append(out, v.I)
		}
	}
	return out
}

func explainOps(t *testing.T, p *engine.ReadOnlyPager, q string) string {
	t.Helper()
	_, rows, err := p.Query("EXPLAIN " + q)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", q, err)
	}
	var ops []string
	for _, r := range rows {
		ops = append(ops, string(r[1].S))
	}
	return " " + strings.Join(ops, " ") + " "
}

// TestFlattenSeriesBesideSeries: a subquery or CTE over generate_series is
// flattened beside another generate_series as C flattens it, so "FROM t, t"
// reads the series pair's native loop (#109).
func TestFlattenSeriesBesideSeries(t *testing.T) {
	p := seriesDB(t)
	for _, q := range []string{
		`WITH t AS (SELECT value FROM generate_series(1,1000)) SELECT count(*) FROM t, t`,
		`WITH t AS (SELECT value FROM generate_series(1,1000)) SELECT count(*) FROM t a, t b`,
		`SELECT count(*) FROM (SELECT value FROM generate_series(1,1000)) a, (SELECT value FROM generate_series(1,1000)) b`,
		`SELECT count(*) FROM generate_series(1,1000) a, (SELECT value FROM generate_series(1,1000)) b`,
	} {
		if got := seriesInts(t, p, q); len(got) != 1 || got[0] != 1000000 {
			t.Errorf("%s = %v", q, got)
		}
		if ops := explainOps(t, p, q); !strings.Contains(ops, " SegProgram ") {
			t.Errorf("%s: not the series pair loop:%s", q, ops)
		}
	}
	if got := seriesInts(t, p, `WITH t AS (SELECT value FROM generate_series(1,30)) SELECT count(*), sum(a.value * b.value) FROM t a, t b`); len(got) != 2 || got[0] != 900 || got[1] != 465*465 {
		t.Errorf("sum over the pair = %v", got)
	}
	for _, q := range []string{
		`WITH t AS (SELECT value FROM generate_series(1,5)) SELECT t.value FROM t, t`,
		`WITH t AS (SELECT value FROM generate_series(1,5)) SELECT count(*) FROM t, t WHERE value > 2`,
	} {
		if _, _, err := p.Query(q); err == nil || !strings.Contains(err.Error(), "ambiguous column name") {
			t.Errorf("%s: %v, want an ambiguous column", q, err)
		}
	}
}

// TestCorrelatedSeriesExists: an EXISTS over a generate_series fed from the
// outer row runs inside the outer program, its scan kept and filtered again
// per row, and answers as running it per row does.
func TestCorrelatedSeriesExists(t *testing.T) {
	p := seriesDB(t)
	q := `SELECT count(*) FROM generate_series(1,10000) t WHERE EXISTS (SELECT 1 FROM generate_series(1,10000) t1 WHERE t.value = t1.value)`
	if got := seriesInts(t, p, q); len(got) != 1 || got[0] != 10000 {
		t.Fatalf("%s = %v", q, got)
	}
	if ops := explainOps(t, p, q); strings.Contains(ops, " Exists ") || !strings.Contains(ops, " Gosub ") {
		t.Errorf("the EXISTS was not inlined:%s", ops)
	}
	primes, squares := 0, 0
	for n := 1; n <= 50; n++ {
		prime := n > 1
		for d := 2; d*d <= n; d++ {
			prime = prime && n%d != 0
		}
		if prime {
			primes++
		}
		for r := 1; r*r <= n; r++ {
			if r*r == n {
				squares++
			}
		}
	}
	for _, tc := range []struct {
		q    string
		want []int64
	}{
		{`SELECT count(*) FROM generate_series(1,50) t WHERE EXISTS (SELECT 1 FROM generate_series(1, t.value) s WHERE s.value * s.value = t.value)`, []int64{int64(squares)}},
		{`SELECT count(*) FROM generate_series(2,50) t WHERE NOT EXISTS (SELECT 1 FROM generate_series(2, t.value - 1) s WHERE t.value % s.value = 0)`, []int64{int64(primes)}},
		{`SELECT t.value FROM generate_series(1,30) t WHERE EXISTS (SELECT 1 FROM generate_series(t.value, 100, t.value) s WHERE s.value = 60) ORDER BY 1`, []int64{1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30}},
		{`SELECT count(*) FROM generate_series(1,30) t WHERE EXISTS (SELECT 1 FROM generate_series(30,1,-1) s WHERE s.value = t.value + 25)`, []int64{5}},
		{`SELECT count(*) FROM generate_series(1,30) t WHERE EXISTS (SELECT 1 FROM generate_series(1,30,2) s WHERE s.value = t.value)`, []int64{15}},
		{`SELECT count(*) FROM generate_series(1,30) t WHERE EXISTS (SELECT 1 FROM generate_series(1,30) s WHERE s.value = t.value AND s.value = abs(-7))`, []int64{1}},
		{`SELECT count(*) FROM (SELECT 5 v UNION ALL SELECT 7 UNION ALL SELECT NULL) t WHERE EXISTS (SELECT 1 FROM generate_series(1,10) s WHERE s.value = t.v)`, []int64{2}},
		{`SELECT a.value, (SELECT count(*) FROM generate_series(1, a.value) s WHERE s.value % 2 = 0) FROM generate_series(1,6) a ORDER BY 1`, []int64{1, 0, 2, 1, 3, 1, 4, 2, 5, 2, 6, 3}},
		{`SELECT count(*) FROM generate_series(1,20) a WHERE EXISTS (SELECT 1 FROM generate_series(1,20) b WHERE b.value = a.value * 2 AND EXISTS (SELECT 1 FROM generate_series(1,40) c WHERE c.value = b.value + a.value))`, []int64{10}},
	} {
		for run := 0; run < 2; run++ {
			if got := seriesInts(t, p, tc.q); !equalInt64s(got, tc.want) {
				t.Errorf("run %d: %s = %v, want %v", run, tc.q, got, tc.want)
			}
		}
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
