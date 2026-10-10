package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// segProgShapes are bodies the program recogniser lowers. Every one of them
// must survive the rewrite unchanged when the guard DECLINES -- see the test
// below for why that is the interesting direction.
var segProgShapes = []string{
	`SELECT count(*) FROM t WHERE k IN (1,3,5)`,
	`SELECT count(*) FROM t WHERE k NOT IN (1,3)`,
	`SELECT count(*) FROM t WHERE id IN (1,2,3)`,
	`SELECT count(*) FROM t WHERE k IN (1,2) OR k = 4`,
	`SELECT count(*) FROM t WHERE NOT (v > 10)`,
	`SELECT count(*) FROM t WHERE v + 1 > 10`,
	`SELECT count(*) FROM t WHERE v * 2 < k`,
	`SELECT count(*) FROM t WHERE v - k > 0`,
	`SELECT count(*) FROM t WHERE v BETWEEN k - 50 AND 50`, // literal BETWEEN is the count kernel's now (segment_between_test.go)
	`SELECT count(*) FROM t WHERE v > 10 OR k = 0`,
	`SELECT count(*) FROM t WHERE v > 10 AND k <> 3 AND v < 100`,
	`SELECT sum(v) FROM t WHERE k IN (2,4)`,
	`SELECT sum(v) FROM t WHERE v > 0 OR k = 1`,
	`SELECT sum(v + k) FROM t WHERE v > 0`,
	`SELECT avg(v) FROM t WHERE v > 0`,
	`SELECT avg(v) FROM t WHERE v > 100000`, // empty: NULL, not 0
	`SELECT total(v) FROM t WHERE v > 0`,
	`SELECT total(v) FROM t WHERE v > 100000`, // empty: 0.0, not NULL
	`SELECT count(v) FROM t WHERE v > 0`,
	`SELECT count(v) FROM t WHERE v > 100000`,
	`SELECT sum(v) FROM t WHERE v > 100000`, // empty: NULL
	`SELECT min(v) FROM t`,
	`SELECT max(v) FROM t`,
	`SELECT min(v) FROM t WHERE v > 0`,
	`SELECT max(v) FROM t WHERE v < 0`,
	`SELECT min(v) FROM t WHERE v > 100000`, // empty: NULL, not the seed
	`SELECT max(v) FROM t WHERE v > 100000`,
	`SELECT min(v + k) FROM t WHERE k IN (1,3)`,
	`SELECT max(id) FROM t WHERE k = 2`,
	`SELECT count(*) FROM t WHERE -v > 10`,
	`SELECT count(*) FROM t WHERE v & 1 = 1`,
	`SELECT count(*) FROM t WHERE v | 1 > 10`,
	`SELECT sum(v) FROM t WHERE (v & 3) | k > 1`,
	`SELECT min(v) FROM t WHERE -k > -3`,
	// Multiple aggregates in one statement share one lowered body.
	`SELECT min(v), max(v) FROM t`,
	`SELECT count(*), sum(v) FROM t WHERE v > 10`,
	`SELECT min(v), max(v), count(*) FROM t WHERE k < 4`,
	`SELECT sum(v), avg(v), count(v) FROM t WHERE v > 0`,
	`SELECT count(*), min(v), max(v), sum(v), avg(v) FROM t`,
	`SELECT max(v), min(v) FROM t WHERE k IN (1,3)`,
	`SELECT total(v), count(*) FROM t WHERE v > 100000`, // empty: 0.0 and 0
	// IS NULL over the NULL INDICATOR block (segment_nullidx.go). This
	// fixture's columns are all non-null, so every indicator is zeros -- the
	// honest answer, and still one that has to be RIGHT.
	`SELECT count(*) FROM t WHERE v IS NULL`,
	`SELECT count(*) FROM t WHERE v IS NOT NULL`,
	`SELECT count(*) FROM t WHERE v IS NULL OR k = 1`,
	`SELECT count(*) FROM t WHERE v IS NOT NULL AND v > 10`,
	`SELECT min(v), max(v) FROM t WHERE k IS NOT NULL`,
}

// segProgStmts is the fixture's table and rows.
func segProgStmts() []string {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER)`}
	rng := rand.New(rand.NewSource(9))
	for i := 1; i <= 400; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d)`, i, rng.Intn(8), rng.Intn(400)-200))
	}
	return stmts
}

// segProgScalar renders q's single row from ask: every column, because a
// multi-aggregate statement answers several and comparing only the first would
// let the second differ unnoticed.
func segProgScalar(t *testing.T, ask func(string, ...Value) ([]string, [][]Value, error), q string) string {
	t.Helper()
	_, rows, err := ask(q)
	if err != nil {
		return "err: " + err.Error()
	}
	if len(rows) != 1 {
		t.Fatalf("%s: want one row, got %d", q, len(rows))
	}
	out := ""
	for _, v := range rows[0] {
		out += fmt.Sprintf("|%v/%d/%v", v.Typ, v.I, v.F)
	}
	return out
}

// TestSegProgRewritePreservesDeclinedProgram verifies that when a guard
// declines, the rewritten program produces the same answer as unrewritten.
func TestSegProgRewritePreservesDeclinedProgram(t *testing.T) {
	p := newSegPair(t, `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER)`)
	p.delta(segProgStmts()[1:]...)
	for _, q := range segProgShapes {
		jitEnabled = false
		want := segProgScalar(t, p.fast, q)
		jitEnabled = jit0
		ResetSegFilterCountersForTest()
		got := segProgScalar(t, p.fast, q)
		// Every row of this table is in the delta. The guards used to decline
		// over one; they merge the delta now (segDeltaSplit and friends), so a
		// guard may serve, and whatever it answers must be the loop's answer.
		if got != want {
			t.Errorf("rewrite changed the answer over a table whose rows are in the delta\n  %s\n  rewritten=%s unrewritten=%s", q, got, want)
		}
	}
	jitEnabled = jit0
}

// TestSegProgServedMatchesDeclined verifies that when a guard serves,
// the compiled code produces the same answer as bytecode.
func TestSegProgServedMatchesDeclined(t *testing.T) {
	if !jit0 {
		t.Skip("JIT unavailable")
	}
	p := newSegPair(t, segProgStmts()...)
	servedAny := false
	for _, q := range segProgShapes {
		want := segProgScalar(t, p.plain, q)
		ResetSegFilterCountersForTest()
		got := segProgScalar(t, p.fast, q)
		served, _ := SegFilterCountersForTest()
		if served > 0 {
			servedAny = true
		}
		if got != want {
			t.Errorf("compiled program disagrees (served=%d)\n  %s\n  jit=%s vdbe=%s", served, q, got, want)
		}
		if served == 0 {
			t.Logf("NOT SERVED %s", q)
		}
	}
	if !servedAny {
		t.Error("no shape was served by a compiled program -- the recogniser stopped matching")
	}
}

// jit0 is jitEnabled as the process started, so a test that toggles it can put
// it back without asserting what the environment chose.
var jit0 = jitEnabled
