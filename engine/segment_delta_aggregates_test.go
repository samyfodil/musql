package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestColumnarAggregatesMergeTheDelta: the compiled-program aggregates
// (segRun, through segDeltaSplit) and the filtered sum
// (segFilterSumTable, through segDeltaSumCorrection) answer over the
// segments with the delta merged, rather than declining to the row loop once
// a table has been written. Each delta state below -- rows added, replaced,
// removed, a NULL and a REAL written over an integer column -- must give the
// loop's answer.
func TestColumnarAggregatesMergeTheDelta(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w INTEGER)`}
	for i := 1; i <= 4000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %d, %d)`, i, rng.Intn(10), rng.Intn(2_000_001)-1_000_000, rng.Intn(100)))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT count(*), sum(v) FROM t`,
		`SELECT count(*) FROM t`,
		`SELECT sum(v) FROM t`,
		`SELECT min(v), max(v) FROM t`,
		`SELECT avg(w), total(v) FROM t`,
		`SELECT sum(v) FROM t WHERE v > 0`,
		`SELECT sum(w) FROM t WHERE k = 3`,
		`SELECT count(*) FROM t WHERE v > 0 AND k <> 2`,
		`SELECT sum(v * 2 + w) FROM t WHERE k < 5`,
		`SELECT max(id), min(id), count(id) FROM t`,
		`SELECT sum(v) FROM t WHERE v > ?`,
		// The INTEGER PRIMARY KEY: a delta row stores it as NULL and its rowid
		// carries the value, so neither a predicate on it nor a sum of it may be
		// answered from the stored column.
		`SELECT count(*) FROM t WHERE id > 100`,
		`SELECT sum(v) FROM t WHERE id >= 3000`,
		`SELECT sum(id) FROM t`,
		// A FILTER is per aggregate: the other aggregate must still see the row.
		`SELECT sum(abs(v)) FILTER (WHERE 0), count(*) FROM t`,
		`SELECT sum(v) FILTER (WHERE k = 3), count(*), sum(w) FROM t`,
		// Several aggregates in one pass.
		`SELECT min(v), max(v), sum(w), count(*) FROM t WHERE k <> 3`,
		`SELECT max(v * k - w), min(v + w * 2 - k), avg(v) FROM t`,
		`SELECT count(v), total(w), max(w), min(k) FROM t WHERE v > ?`,
		`SELECT min(DISTINCT k), max(v), sum(v) FROM t WHERE w < 50`,
		`SELECT sum(v * 922337203685), count(*) FROM t WHERE k = 1`,
	}
	check := func(label string) {
		t.Helper()
		for _, q := range queries {
			var args []Value
			if q == `SELECT sum(v) FROM t WHERE v > ?` || q == `SELECT count(v), total(w), max(w), min(k) FROM t WHERE v > ?` {
				args = []Value{{Typ: Int, I: 250000}}
			}
			want := fmt.Sprint(typedRows(p.mustPlain(q, args...)))
			got := fmt.Sprint(typedRows(p.mustFast(q, args...)))
			if got != want {
				t.Errorf("%s %s:\n fast  %s\n plain %s", label, q, got, want)
			}
		}
	}
	runs := segOnePassRuns.Load()
	check("segments")
	if segOnePassRuns.Load() == runs {
		t.Fatal("no statement's aggregates were answered in one pass")
	}
	// A table whose rows are all in the delta: no segment to correct, so a
	// correction alone answers.
	p.delta(`CREATE TABLE u(id INTEGER PRIMARY KEY, v INTEGER)`, `INSERT INTO u VALUES (1, 5)`, `INSERT INTO u VALUES (2, 6)`)
	for _, q := range []string{
		`SELECT count(*) FROM u WHERE id > 0`, `SELECT sum(id) FROM u`, `SELECT sum(v) FROM u WHERE id > 1`,
		`SELECT count(*), sum(v), min(v), max(id) FROM u`,
	} {
		want := fmt.Sprint(typedRows(p.mustPlain(q)))
		if got := fmt.Sprint(typedRows(p.mustFast(q))); got != want {
			t.Errorf("delta-only %s:\n fast  %s\n plain %s", q, got, want)
		}
	}
	p.delta(`UPDATE t SET v = v + 1 WHERE id = 7`)
	check("one update")
	p.delta(`INSERT INTO t VALUES (9001, 3, 999, 1), (9002, 4, -5, 2)`, `DELETE FROM t WHERE id IN (1, 2, 4000)`)
	check("added and removed")
	p.delta(`UPDATE t SET v = -v WHERE id % 13 = 0`, `DELETE FROM t WHERE id % 29 = 0`)
	check("many")
	p.delta(`UPDATE t SET w = NULL WHERE id = 10`)
	check("a NULL")
	p.delta(`UPDATE t SET v = 2.5 WHERE id = 11`)
	check("a REAL")
}

// TestFilteredSumTextBounds: a filtered sum whose bounds are TEXT or REAL is
// answered from the segments a batch at a time, as the count is, and gives
// the loop's answer in every delta state; a TEXT equality is left to the
// loop, which finds its rows by the equality index.
func TestFilteredSumTextBounds(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, v INTEGER, r REAL, b BLOB)`}
	for i := 1; i <= 4000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'n%04d', %d, %d.5, x'%04x')`, i, rng.Intn(5000), rng.Intn(2_000_001)-1_000_000, rng.Intn(100), rng.Intn(65536)))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT sum(v) FROM t WHERE name > 'n2500'`,
		`SELECT sum(v) FROM t WHERE name <= 'n0100'`,
		`SELECT sum(v) FROM t WHERE name <> 'n0042'`,
		`SELECT sum(v) FROM t WHERE name > 'n2500' AND v > 0`,
		`SELECT sum(v) FROM t WHERE v < 0 AND name < 'n1000' AND name >= 'n0500'`,
		`SELECT sum(v) FROM t WHERE name > ?`,
		`SELECT sum(v) FROM t WHERE name > 'zzz'`,
		`SELECT sum(v) FROM t WHERE name >= ''`,
		`SELECT sum(v) FROM t WHERE name > 'n25'`,
		`SELECT sum(v) FROM t WHERE r > 50.0`,
		`SELECT sum(v) FROM t WHERE r > 50`,
		`SELECT sum(v) FROM t WHERE v > 2.5`,
		`SELECT sum(v) FROM t WHERE b > x'8000'`,
		`SELECT sum(v) FROM t WHERE name = ?`,
		`SELECT sum(v) FROM t WHERE name = 'n0042' AND v > 0`,
	}
	check := func(label string) {
		t.Helper()
		for _, q := range queries {
			var args []Value
			if q == `SELECT sum(v) FROM t WHERE name > ?` || q == `SELECT sum(v) FROM t WHERE name = ?` {
				args = []Value{{Typ: Text, S: []byte("n2500")}}
			}
			want := fmt.Sprint(typedRows(p.mustPlain(q, args...)))
			got := fmt.Sprint(typedRows(p.mustFast(q, args...)))
			if got != want {
				t.Errorf("%s %s:\n fast  %s\n plain %s", label, q, got, want)
			}
		}
	}
	served := func(q string, args ...Value) bool {
		t.Helper()
		s0, _ := SegFilterCountersForTest()
		p.mustFast(q, args...)
		s1, _ := SegFilterCountersForTest()
		return s1 > s0
	}
	if !served(`SELECT sum(v) FROM t WHERE name > 'n2500'`) || !served(`SELECT sum(v) FROM t WHERE name > ?`, Value{Typ: Text, S: []byte("n2500")}) {
		t.Fatal("a TEXT range was not answered from the segments")
	}
	if served(`SELECT sum(v) FROM t WHERE name = ?`, Value{Typ: Text, S: []byte("n2500")}) {
		t.Fatal("a TEXT equality was scanned")
	}
	check("segments")
	p.delta(`UPDATE t SET name = 'n9999' WHERE id = 7`, `INSERT INTO t VALUES (9001, 'n3000', 999, 1.5, x'00')`, `DELETE FROM t WHERE id IN (1, 2, 4000)`)
	check("added and removed")
	p.delta(`UPDATE t SET name = NULL WHERE id = 10`)
	check("a NULL")
	p.delta(`UPDATE t SET name = 5 WHERE id = 11`)
	check("an INTEGER in a TEXT column")
}
