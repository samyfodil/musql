package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// This file tests that ORDER BY ... LIMIT returns the correct rows in the
// correct order, including tie-breaking by insertion order.
func TestColumnarOrderLimitMatchesTheSorter(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, lo INTEGER, hi INTEGER)`}
	rng := rand.New(rand.NewSource(17))
	for i := 1; i <= 900; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d)`, i, rng.Intn(5), rng.Intn(100000)-50000))
	}
	p := newSegPair(t, stmts...)

	queries := []struct {
		sql        string
		wantServed bool
	}{
		{`SELECT id, hi FROM t ORDER BY hi DESC, id DESC LIMIT 20`, true},
		{`SELECT id, hi FROM t ORDER BY hi ASC, id ASC LIMIT 20`, true},
		{`SELECT id, lo FROM t ORDER BY lo DESC LIMIT 20`, true},
		{`SELECT id, lo FROM t ORDER BY lo ASC LIMIT 7`, true},
		{`SELECT lo, hi FROM t ORDER BY lo ASC, hi DESC LIMIT 30`, true},
		{`SELECT id FROM t ORDER BY id DESC LIMIT 1`, true},
		// LIMIT past end: emit every row in order.
		{`SELECT id, hi FROM t ORDER BY hi DESC LIMIT 100000`, true},
		// Shapes it must not serve.
		{`SELECT id, hi FROM t ORDER BY hi DESC`, false},                       // no LIMIT
		{`SELECT id, hi FROM t ORDER BY hi DESC LIMIT 20 OFFSET 5`, false},     // OFFSET
		{`SELECT id, hi FROM t WHERE hi > 0 ORDER BY hi DESC LIMIT 20`, false}, // a WHERE
		{`SELECT id, hi FROM t ORDER BY hi + 1 DESC LIMIT 20`, false},          // an expression key
		{`SELECT DISTINCT lo FROM t ORDER BY lo LIMIT 5`, false},               // DISTINCT
		// Served, but by the AGGREGATE recogniser next door, not this one --
		// count(*) with no predicate is answered from the segments' row counts
		// and the ORDER BY over a single row is trivially satisfied. Both
		// recognisers bump the same counter, so "served" here does not say
		// which; the row comparison above is what checks the answer.
		{`SELECT count(*) FROM t ORDER BY 1 LIMIT 20`, true},
	}

	want := make([][]string, len(queries))
	for i, q := range queries {
		want[i] = typedRows(p.mustPlain(q.sql))
	}
	for i, q := range queries {
		ResetSegFilterCountersForTest()
		got := typedRows(p.mustFast(q.sql))
		served, _ := SegFilterCountersForTest()
		if len(got) != len(want[i]) {
			t.Errorf("%s: %d rows columnar, %d sorter", q.sql, len(got), len(want[i]))
			continue
		}
		for r := range got {
			if got[r] != want[i][r] {
				t.Errorf("%s row %d:\n columnar %s\n sorter   %s", q.sql, r, got[r], want[i][r])
				break
			}
		}
		if JITEnabled() && q.wantServed && served == 0 {
			t.Errorf("%s: the fast path never ran, so this compares the sorter with itself", q.sql)
		}
		if JITEnabled() && !q.wantServed && served != 0 {
			t.Errorf("%s: the fast path served a shape it must not", q.sql)
		}
	}
}

// TestColumnarOrderLimitReadsEveryColumnType is what this test became.
//
// It was written as ...DeclinesUnreadableColumns, and the three cases below are
// the ones it recorded as unreadable: a TEXT key, a NULL-bearing integer key,
// and a TEXT output column. All three are SERVED now, through
// segOrderLimitValues, which reads any column and any physical type via
// segment.Value and delegates ordering to compareValuesCollatedEnc rather than
// implementing it.
//
// The assertion inverts with the capability: they must now be served AND match
// the sorter. A decline here is no longer the safe direction, it is a lost fast
// path -- and the shape it costs is the one that measured 1.48x SLOWER than C
// SQLite, "ORDER BY payload DESC, id DESC LIMIT 20", the last read workload in
// the benchmark that C won.
func TestColumnarOrderLimitReadsEveryColumnType(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT, n INTEGER)`}
	for i := 1; i <= 200; i++ {
		n := fmt.Sprintf("%d", i*3)
		if i%17 == 0 {
			n = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,'s%d',%s)`, i, i%9, n))
	}
	p := newSegPair(t, stmts...)
	for _, sql := range []string{
		`SELECT id, s FROM t ORDER BY s DESC, id DESC LIMIT 10`, // TEXT key and output
		`SELECT id, n FROM t ORDER BY n DESC, id DESC LIMIT 10`, // NULL-bearing key
		`SELECT s, n FROM t ORDER BY id LIMIT 10`,               // TEXT output
	} {
		wantRows := typedRows(p.mustPlain(sql))
		ResetSegFilterCountersForTest()
		gotRows := typedRows(p.mustFast(sql))
		served, _ := SegFilterCountersForTest()
		if JITEnabled() && served == 0 {
			t.Errorf("%s: declined -- the Value path reads every column type, so this is a lost fast path", sql)
		}
		if len(gotRows) != len(wantRows) {
			t.Fatalf("%s: %d rows vs %d", sql, len(gotRows), len(wantRows))
		}
		for i := range gotRows {
			if gotRows[i] != wantRows[i] {
				t.Errorf("%s row %d: %q vs %q", sql, i, gotRows[i], wantRows[i])
				break
			}
		}
	}
}
