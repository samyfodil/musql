package engine

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// sum(<column>) over a filter, answered from the segments, against the plain
// loop over the same rows (segPair).
func TestColumnarSumMatchesTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t(a INTEGER, v INTEGER)`}
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 700; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d)`, i-350, rng.Intn(2000)-1000))
	}
	p := newSegPair(t, stmts...)

	cases := []struct {
		sql  string
		args []Value
	}{
		{`SELECT sum(v) FROM t`, nil},
		{`SELECT sum(v) FROM t WHERE a > ?`, []Value{{Typ: Int, I: 0}}},
		{`SELECT sum(v) FROM t WHERE a > ?`, []Value{{Typ: Int, I: -350}}},
		{`SELECT sum(a) FROM t WHERE v < ?`, []Value{{Typ: Int, I: 0}}},
		{`SELECT sum(v) FROM t WHERE a > ? AND a < ?`, []Value{{Typ: Int, I: -10}, {Typ: Int, I: 10}}},
		{`SELECT sum(v) FROM t WHERE a > ?`, []Value{{Typ: Int, I: 100000}}},
	}
	for _, c := range cases {
		want := p.mustPlain(c.sql, c.args...)
		ResetSegFilterCountersForTest()
		got := p.mustFast(c.sql, c.args...)
		served, _ := SegFilterCountersForTest()
		if got[0][0].Typ != want[0][0].Typ || got[0][0].I != want[0][0].I {
			t.Errorf("%s %v: columnar {%v %d}, loop {%v %d}", c.sql, c.args,
				got[0][0].Typ, got[0][0].I, want[0][0].Typ, want[0][0].I)
		}
		if JITEnabled() && served == 0 {
			t.Errorf("%s %v: the fast path never ran; this compares the loop with itself", c.sql, c.args)
		}
	}
}

// An integer sum that overflows raises "integer overflow" in C (sumStep's
// latch): the compiled path raises it where it overflows, without going back
// to the loop.
func TestColumnarSumOverflowsAsTheLoop(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE t(a INTEGER, v INTEGER)`,
		fmt.Sprintf(`INSERT INTO t VALUES(1,%d)`, int64(math.MaxInt64)),
		fmt.Sprintf(`INSERT INTO t VALUES(2,%d)`, int64(math.MaxInt64)))
	_, _, werr := p.plain(`SELECT sum(v) FROM t`)
	ResetSegFilterCountersForTest()
	_, _, gerr := p.fast(`SELECT sum(v) FROM t`)
	served, declined := SegFilterCountersForTest()
	if werr == nil || gerr == nil || werr.Error() != gerr.Error() {
		t.Fatalf("loop err=%v, columnar err=%v", werr, gerr)
	}
	if served != 0 || declined != 0 {
		t.Errorf("served=%d declined=%d: the compiled run should raise the error itself", served, declined)
	}
}
