package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// Tests that compiled WHERE clause programs produce the same results as the
// loop interpreter, with various combinations of OR, BETWEEN, and NULL handling.
func TestCompiledProgramMatchesTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, s TEXT, n INTEGER)`}
	rng := rand.New(rand.NewSource(5))
	for i := 1; i <= 800; i++ {
		n := fmt.Sprintf("%d", rng.Intn(40))
		if i%37 == 0 {
			n = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d,'s%d',%s)`,
			i, rng.Intn(10), rng.Intn(2000)-1000, i%13, n))
	}
	p := newSegPair(t, stmts...)

	cases := []struct {
		sql  string
		args []Value
	}{
		{`SELECT count(*) FROM t WHERE v > ? OR k = ?`, []Value{{Typ: Int, I: 0}, {Typ: Int, I: 3}}},
		{`SELECT count(*) FROM t WHERE v > ? OR k = ?`, []Value{{Typ: Int, I: 5000}, {Typ: Int, I: 3}}},
		{`SELECT count(*) FROM t WHERE v BETWEEN ? AND ?`, []Value{{Typ: Int, I: -100}, {Typ: Int, I: 100}}},
		{`SELECT count(*) FROM t WHERE v > ? AND k <> ? AND id < ?`,
			[]Value{{Typ: Int, I: -500}, {Typ: Int, I: 4}, {Typ: Int, I: 400}}},
		{`SELECT count(*) FROM t WHERE v > 0 AND k < 5`, nil},
		{`SELECT count(*) FROM t WHERE k = 1 OR k = 2 OR k = 3`, nil},
		{`SELECT sum(v) FROM t WHERE v > ? OR k = ?`, []Value{{Typ: Int, I: 500}, {Typ: Int, I: 1}}},
		{`SELECT sum(v) FROM t WHERE v > ?`, []Value{{Typ: Int, I: 100000}}}, // matches nothing -> NULL
		// Must still be correct when a column cannot be read columnar.
		{`SELECT count(*) FROM t WHERE n > ?`, []Value{{Typ: Int, I: 10}}},
		{`SELECT count(*) FROM t WHERE s > ?`, []Value{{Typ: Text, S: []byte("s5")}}},
	}

	want := make([][][]Value, len(cases))
	for i, c := range cases {
		want[i] = p.mustPlain(c.sql, c.args...)
	}
	servedAny := 0
	for i, c := range cases {
		ResetSegFilterCountersForTest()
		_, got, gerr := p.fast(c.sql, c.args...)
		if gerr != nil {
			t.Fatalf("%s columnar: %v", c.sql, gerr)
		}
		served, _ := SegFilterCountersForTest()
		servedAny += int(served)
		if len(got) != len(want[i]) {
			t.Errorf("%s: %d rows, want %d", c.sql, len(got), len(want[i]))
			continue
		}
		for r := range got {
			for j := range got[r] {
				g, w := got[r][j], want[i][r][j]
				if g.Typ != w.Typ || g.I != w.I {
					t.Errorf("%s %v: columnar {%v %d}, loop {%v %d}",
						c.sql, c.args, g.Typ, g.I, w.Typ, w.I)
				}
			}
		}
	}
	if JITEnabled() && servedAny == 0 {
		t.Fatal("nothing was served by a fast path, so this compares the loop with itself")
	}
}
