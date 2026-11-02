package engine

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"
)

// TestDistinctAggregatesThroughTheProgramJIT: a DISTINCT count, sum, total or
// avg over integers is served by the program JIT, which hands each row's
// argument back (POpEmitVal) for Go to dedup, with the loop's answer -- over
// segments, beside a delta, and on parallel workers -- and the loop answers
// where a NULL column or the sum's magnitude guard declines.
func TestDistinctAggregatesThroughTheProgramJIT(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, n INTEGER, big INTEGER)`}
	rng := rand.New(rand.NewSource(7))
	for i := 1; i <= 3000; i++ {
		n := fmt.Sprint(rng.Intn(50))
		if i%41 == 0 {
			n = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d,%s,%d)`,
			i, rng.Intn(10), rng.Intn(500)-250, n, int64(math.MaxInt64)-int64(rng.Intn(3))))
	}
	p := newSegPair(t, stmts...)
	served := []string{
		`SELECT count(DISTINCT v) FROM t`,
		`SELECT count(DISTINCT v), sum(DISTINCT v), total(DISTINCT v), avg(DISTINCT v) FROM t`,
		`SELECT min(DISTINCT v), max(DISTINCT v), count(DISTINCT k) FROM t`,
		`SELECT count(DISTINCT v % 7), sum(DISTINCT k * 3) FROM t WHERE v > 0`,
		`SELECT count(DISTINCT id), sum(DISTINCT id) FROM t WHERE k < 3`,
		`SELECT sum(DISTINCT v), count(DISTINCT v) FROM t WHERE v > 100000`,
	}
	looped := []string{
		`SELECT count(DISTINCT n) FROM t`,
		`SELECT sum(DISTINCT big) FROM t`,
		`SELECT avg(DISTINCT big) FROM t`,
	}
	check := func(label string) {
		t.Helper()
		ResetSegFilterCountersForTest()
		for _, q := range served {
			p.same(t, label, q)
		}
		if s, _ := SegFilterCountersForTest(); JITEnabled() && s < int64(len(served)) {
			t.Errorf("%s: %d of %d statements served by the program JIT", label, s, len(served))
		}
		for _, q := range looped {
			p.same(t, label, q)
		}
	}
	check("segments")
	defer func(n int) { segWorkers = n }(segWorkers)
	segWorkers = 4
	check("workers")
	segWorkers = 1
	p.delta(`INSERT INTO t VALUES(5001, 1, 9999, 1, 1)`, `UPDATE t SET v = v + 1000 WHERE id % 97 = 0`, `DELETE FROM t WHERE id % 89 = 0`)
	check("delta")
}

// same requires q to answer, or fail, the same through the fast paths and the
// plain loop.
func (p *segPair) same(t *testing.T, label, q string) {
	t.Helper()
	_, fast, ferr := p.fast(q)
	_, plain, perr := p.plain(q)
	if fmt.Sprint(ferr) != fmt.Sprint(perr) {
		t.Errorf("%s: %s: error %v, loop %v", label, q, ferr, perr)
		return
	}
	if got, want := typedRows(fast), typedRows(plain); !slices.Equal(got, want) {
		t.Errorf("%s: %s: %v, loop %v", label, q, got, want)
	}
}

func TestInt64Set(t *testing.T) {
	var a, b int64Set
	for _, v := range []int64{3, 0, -7, math.MinInt64, 3, 0, 1 << 40} {
		a.add(v)
	}
	for i := range int64(1000) {
		b.add(i * 1024) // all land in one bucket of a small table but for the mixing
	}
	a.union(&b)
	if a.len() != 5+1000-1 { // 0 is in both
		t.Fatalf("len %d", a.len())
	}
	var got []int64
	a.each(func(v int64) bool { got = append(got, v); return true })
	if len(got) != a.len() || !slices.Contains(got, math.MinInt64) || !slices.Contains(got, 0) {
		t.Fatalf("each gave %d values", len(got))
	}
	n := 0
	if a.each(func(int64) bool { n++; return n < 3 }) || n != 3 {
		t.Fatalf("each did not stop: %d", n)
	}
}
