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

// An integer sum that overflows switches to REAL in C (sumStep's overflow arm);
// the columnar path cannot follow it, so it must decline to the loop.
func TestColumnarSumDeclinesOnOverflow(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE t(a INTEGER, v INTEGER)`,
		fmt.Sprintf(`INSERT INTO t VALUES(1,%d)`, int64(math.MaxInt64)),
		fmt.Sprintf(`INSERT INTO t VALUES(2,%d)`, int64(math.MaxInt64)))
	_, want, werr := p.plain(`SELECT sum(v) FROM t`)
	ResetSegFilterCountersForTest()
	_, got, gerr := p.fast(`SELECT sum(v) FROM t`)
	served, declined := SegFilterCountersForTest()
	if served != 0 {
		t.Fatalf("the fast path answered an overflowing sum (served=%d)", served)
	}
	if JITEnabled() && declined == 0 {
		t.Fatal("the guard was never reached, so this proves nothing")
	}
	// Whatever the loop does -- a value or an error -- the columnar path must
	// do the same, because it declined to the same loop.
	if (werr == nil) != (gerr == nil) {
		t.Fatalf("loop err=%v, columnar err=%v", werr, gerr)
	}
	if werr == nil && (got[0][0].Typ != want[0][0].Typ || got[0][0].F != want[0][0].F) {
		t.Errorf("columnar {%v %v}, loop {%v %v}", got[0][0].Typ, got[0][0].F, want[0][0].Typ, want[0][0].F)
	}
}
