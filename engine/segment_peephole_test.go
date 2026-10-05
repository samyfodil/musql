package engine

import (
	"fmt"
	"testing"
)

// The peephole must not change query answers.
func TestPeepholeAnswersIdentically(t *testing.T) {
	stmts := []string{`CREATE TABLE t(a INTEGER, b INTEGER, c TEXT, n INTEGER)`,
		// Index to test with OpAutoIndexOrder.
		`CREATE INDEX idx_t_b ON t(b)`}
	for i := 0; i < 900; i++ {
		n := fmt.Sprintf("%d", i%7)
		if i%31 == 0 {
			n = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,'s%d',%s)`, i-450, i%13, i, n))
	}
	p := newSegPair(t, stmts...)

	queries := []string{
		// Peephole optimizations with predicates.
		`SELECT count(*) FROM t WHERE a > 0 AND b <> 3`,
		`SELECT count(*) FROM t WHERE a >= -450 AND b <= 5`,
		`SELECT count(*) FROM t WHERE a < 0 AND b = 7`,
		`SELECT count(*) FROM t WHERE a <= -1 AND b >= 2`,
		`SELECT count(*) FROM t WHERE a = 0 AND b <> 0`,
		`SELECT count(*) FROM t WHERE a <> 0 AND b < 4`,
		`SELECT count(*) FROM t WHERE a > 0`,
		`SELECT count(*) FROM t WHERE b = 7`,
		// No predicates, count from segments.
		`SELECT count(*) FROM t`,
		// sum() with and without filters.
		`SELECT sum(a) FROM t WHERE a > 0 AND b <> 3`,
		`SELECT sum(b) FROM t WHERE a > 0`,
		`SELECT sum(a) FROM t`,
		// Filter that matches nothing.
		`SELECT sum(a) FROM t WHERE a > 100000`,
		// Indexed column equality.
		`SELECT count(*) FROM t WHERE b = 3`,
		`SELECT count(*) FROM t WHERE b = 3 AND a > 0`,

		// Neighbours it must REFUSE, each for its own reason. Whether it
		// refuses is asserted next door in TestPeepholeRecognisesOnlyTheRightShape;
		// what is asserted HERE is that the answer is the same either way,
		// which is the property that matters and the one a loose match breaks.
		//
		// three predicates:
		`SELECT count(*) FROM t WHERE a > 0 AND b <> 3 AND n < 4`,
		// a NULL-bearing column:
		`SELECT count(*) FROM t WHERE n > 0 AND b <> 3`,
		// not integers:
		`SELECT count(*) FROM t WHERE c > 'a' AND b <> 3`,
		// OR, not a conjunction:
		`SELECT count(*) FROM t WHERE a > 0 OR b <> 3`,
		// not count(*):
		`SELECT sum(a) FROM t WHERE a > 0 AND b <> 3`,
		// count(col), not count(*):
		`SELECT count(a) FROM t WHERE a > 0 AND b <> 3`,
		// grouped:
		`SELECT b, count(*) FROM t WHERE a > 0 AND b <> 3 GROUP BY b ORDER BY b`,
		// a LIMIT:
		`SELECT count(*) FROM t WHERE a > 0 AND b <> 3 LIMIT 1`,
		// DISTINCT:
		`SELECT count(DISTINCT b) FROM t WHERE a > 0 AND b <> 3`,
	}

	// The plain loop, with no segment peephole ...
	want := make([][]string, len(queries))
	for i, q := range queries {
		want[i] = typedRows(p.mustPlain(q))
	}
	// ... and production, where the guard can serve.
	for i, q := range queries {
		got := typedRows(p.mustFast(q))
		if len(got) != len(want[i]) {
			t.Fatalf("%s: %d rows columnar, %d plain", q, len(got), len(want[i]))
		}
		for r := range got {
			if got[r] != want[i][r] {
				t.Fatalf("%s row %d:\n columnar %s\n plain    %s", q, r, got[r], want[i][r])
			}
		}
	}
}

// Non-vacuity: the recogniser must actually fire on the shape it is for, and
// must not fire on the neighbours. Without this the test above would pass over
// a peephole that never did anything.
func TestPeepholeRecognisesOnlyTheRightShape(t *testing.T) {
	if !JITEnabled() {
		t.Skip("the peephole only runs where the JIT does")
	}
	stmts := []string{`CREATE TABLE t(a INTEGER, b INTEGER, c TEXT)`, `CREATE INDEX idx_t_b ON t(b)`}
	for i := 0; i < 10; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,'x')`, i, i%3))
	}
	rp, err := Open(newSegPair(t, stmts...).path)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()

	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{`SELECT count(*) FROM t WHERE a > 1 AND b <> 2`, true},
		// One predicate, and a BOUND PARAMETER, are both matched now. The
		// parameter is the one that matters: `count(*) ... WHERE col > ?` is
		// what a prepared statement looks like, and matching only OpInteger
		// meant the recogniser covered the hand-written case and missed the
		// real one.
		{`SELECT count(*) FROM t WHERE a > 1`, true},
		{`SELECT count(*) FROM t WHERE a > ?`, true},
		{`SELECT count(*) FROM t WHERE a > ? AND b <> ?`, true},
		// No predicates: the row count is segment metadata, not a scan.
		{`SELECT count(*) FROM t`, true},
		// An INDEXED column. This compiles an OpAutoIndexOrder between the open
		// and the Rewind, which the recogniser declined outright until it
		// learned that the opcode only permutes ORDER -- and a count does not
		// care about order. Before that, having an index on the column was
		// enough to lose the fast path.
		{`SELECT count(*) FROM t WHERE b = ?`, true},
		{`SELECT count(*) FROM t WHERE b = ? AND a > ?`, true},
		// sum(<column>) is answered from the segments too now, so this moved
		// from "must refuse" to "must fire". It is still the NARROW sum: one
		// aggregate, no DISTINCT, no GROUP BY, an integer column.
		{`SELECT sum(a) FROM t WHERE a > 1 AND b <> 2`, true},
		{`SELECT sum(a) FROM t`, true},
		{`SELECT sum(a) FROM t WHERE b = ?`, true},
		// ...but not with DISTINCT, and not over an expression.
		{`SELECT sum(DISTINCT a) FROM t WHERE a > 1`, false},
		{`SELECT sum(a + b) FROM t WHERE a > 1`, false},
		{`SELECT avg(a) FROM t WHERE a > 1 AND b <> 2`, false},
		{`SELECT count(a) FROM t WHERE a > 1 AND b <> 2`, false},
		{`SELECT count(*) FROM t WHERE a > 1 OR b <> 2`, false},
		{`SELECT count(*) FROM t WHERE a > 1 AND b <> 2 AND a < 9`, false},
		{`SELECT b, count(*) FROM t WHERE a > 1 AND b <> 2 GROUP BY b`, false},
	} {
		stmt, err := ParseSelect(tc.sql)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.sql, err)
		}
		prog, err := compileSelectScan(rp, stmt, nil)
		if err != nil {
			t.Fatalf("%s: compile: %v", tc.sql, err)
		}
		fired := false
		for _, in := range prog.Insns {
			if in.Op == OpSegFilterCount {
				fired = true
			}
		}
		if fired != tc.want {
			t.Errorf("%s: peephole fired=%v, want %v", tc.sql, fired, tc.want)
		}
	}
}

// The BOUND PARAMETER path, differentially, because nothing else here can see
// it: the corpus differ has no way to bind parameters (it replays SQL text), so
// a bug reachable only through "?" passes every other gate in this repo.
//
// It is also the path most likely to be wrong, because it is the one where the
// comparison value does not exist until the statement runs. The peephole
// records the REGISTER; OpSegFilterCount reads it. A plan that captured the
// wrong register, or read it before OpVariable filled it, would answer a
// confident number computed against zero.
func TestPeepholeParameterBoundsMatchTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t(a INTEGER, b INTEGER)`}
	for i := 0; i < 500; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d)`, i-250, i%7))
	}
	p := newSegPair(t, stmts...)

	cases := []struct {
		sql  string
		args []Value
	}{
		{`SELECT count(*) FROM t WHERE a > ?`, []Value{{Typ: Int, I: 0}}},
		{`SELECT count(*) FROM t WHERE a > ?`, []Value{{Typ: Int, I: -250}}},
		{`SELECT count(*) FROM t WHERE a > ?`, []Value{{Typ: Int, I: 1000}}},
		{`SELECT count(*) FROM t WHERE a <= ?`, []Value{{Typ: Int, I: -1}}},
		{`SELECT count(*) FROM t WHERE b = ?`, []Value{{Typ: Int, I: 3}}},
		{`SELECT count(*) FROM t WHERE a > ? AND b <> ?`, []Value{{Typ: Int, I: 0}, {Typ: Int, I: 2}}},
		{`SELECT count(*) FROM t WHERE a < ? AND b >= ?`, []Value{{Typ: Int, I: 100}, {Typ: Int, I: 4}}},
		// A bound the kernel cannot take must DECLINE to the loop, not be
		// coerced: NULL compares false against everything, and a TEXT bound is
		// greater than every integer under SQLite's class ordering. Getting
		// either wrong is a wrong answer that is also faster.
		{`SELECT count(*) FROM t WHERE a > ?`, []Value{{Typ: Null}}},
		{`SELECT count(*) FROM t WHERE a > ?`, []Value{{Typ: Text, S: []byte("x")}}},
		{`SELECT count(*) FROM t WHERE a > ? AND b <> ?`, []Value{{Typ: Null}, {Typ: Int, I: 2}}},
	}

	want := make([]int64, len(cases))
	for i, c := range cases {
		want[i] = p.mustPlain(c.sql, c.args...)[0][0].I
	}
	for i, c := range cases {
		// Whether the fast path is EXPECTED to serve: a NON-NULL bound on every
		// predicate. Asserting this is the whole point -- without it the test
		// compares the loop against itself and passes no matter what, which is
		// exactly what it did while OpSegFilterCount was declining every
		// statement because it read a register the loop had not filled yet.
		//
		// It used to say "an INTEGER bound on every predicate", and that was a
		// statement about the kernel rather than about the answer: the block path
		// compares through compareValues, which is the same comparator the loop
		// uses, so a REAL, TEXT or BLOB bound is compared correctly -- including
		// across storage classes, where SQLite orders INTEGER below TEXT. NULL is
		// the one that still declines, because its three-valued logic is not the
		// same question as ordering.
		wantServed := JITEnabled()
		for _, a := range c.args {
			if a.Typ == Null {
				wantServed = false
			}
		}
		ResetSegFilterCountersForTest()
		rows := p.mustFast(c.sql, c.args...)
		served, declined := SegFilterCountersForTest()
		if got := rows[0][0].I; got != want[i] {
			t.Errorf("%s %v: columnar %d, plain %d", c.sql, c.args, got, want[i])
		}
		if wantServed && served == 0 {
			t.Errorf("%s %v: the JIT never served it (served=%d declined=%d); "+
				"the answer above came from the loop", c.sql, c.args, served, declined)
		}
		if !wantServed && served != 0 {
			t.Errorf("%s %v: the JIT served a bound it cannot compare", c.sql, c.args)
		}
	}

	// Non-vacuity: the same prepared shape must give DIFFERENT answers for
	// different bindings. A plan that froze the first binding would pass every
	// comparison above and still be broken for every caller that reuses a
	// statement -- which is the bug this whole register indirection exists to
	// avoid, and one this repo has shipped before (see the LIMIT ? plan-cache
	// regression).
	// ONE session for all five: a frozen binding lives in its plan cache.
	n, err := OpenWrite(p.path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	seen := map[int64]bool{}
	for _, bound := range []int64{-250, -100, 0, 100, 249} {
		_, rows, qerr := n.Query(`SELECT count(*) FROM t WHERE a > ?`, []Value{{Typ: Int, I: bound}})
		if qerr != nil {
			t.Fatal(qerr)
		}
		seen[rows[0][0].I] = true
	}
	if len(seen) != 5 {
		t.Fatalf("five different bounds gave %d distinct counts; a binding is being frozen", len(seen))
	}
}
