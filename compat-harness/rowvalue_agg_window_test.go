// Tests row-value probes against subqueries mixing aggregates with window functions.
// rowvalue_subquery_test.go's exhaustive grid, this file is narrow (the
// combination is rare) so a decline anywhere here is a regression, not
// something to tolerate. A handful of ADJACENT shapes this bucket does NOT
// cover -- two hoisted min/max calls in the same subquery, or an aggregate
// hoisted into an outer GROUP BY -- are pre-existing, separately-tracked
// declines unrelated to the RowExpr gap this file closes; they are asserted
// declined (not answered wrong) in TestRowValueAggWindowAdjacentDeclines
// below, so a future fix to either doesn't silently regress unnoticed.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var rawSchema = []string{
	`CREATE TABLE t0(c0)`,
	`CREATE TABLE t1(g, c0)`,
	`INSERT INTO t1 VALUES(1,2),(1,1),(2,0),(2,5)`,
}

type rawHarness struct {
	t   *testing.T
	edb *engine.Session
	c   *sql.DB
}

func newRAWHarness(t *testing.T) *rawHarness {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cdb.Close() })
	for _, s := range rawSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
	}
	return &rawHarness{t: t, edb: edb, c: cdb}
}

// exec runs a write statement (e.g. an INSERT to change t0's contents
// mid-test, exactly as window1.test's own 44.4.2 does) on both engines.
func (h *rawHarness) exec(s string) {
	h.t.Helper()
	if err := h.edb.Exec(s); err != nil {
		h.t.Fatalf("engine exec %s: %v", s, err)
	}
	if _, err := h.c.Exec(s); err != nil {
		h.t.Fatalf("cgo exec %s: %v", s, err)
	}
}

// cmpWant runs q against both engines and requires them to AGREE -- an engine
// decline is reported as a failure here (unlike rowvalue_subquery_test.go's
// tolerant cmp), because every case fed to it is expected to be answered. A
// FRESH SnapshotPager is taken per call (never cached) so a query always sees
// every exec() that ran before it, mid-test INSERTs included.
func (h *rawHarness) cmpWant(q string) {
	h.t.Helper()
	p, perr := h.edb.SnapshotPager()
	if perr != nil {
		h.t.Fatalf("SnapshotPager: %v", perr)
	}
	_, ev, eerr := p.QueryArgs(q, nil)
	cc, cr, cerr := cgoSelect(h.t, h.c, q, nil)
	switch {
	case eerr != nil && cerr != nil:
		return // both reject it (e.g. the ntile(0) argument error): agreement
	case eerr != nil:
		h.t.Errorf("[%s] engine declined what C SQLite answers: %v", q, eerr)
		return
	case cerr != nil:
		h.t.Errorf("[%s] engine accepted what C SQLite rejects: %v", q, cerr)
		return
	}
	eRows := engineRowsToStrings(ev)
	cols := make([]string, len(cc))
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
		h.t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
	}
}

// cmpDecline asserts the engine DECLINES q (errors) rather than answering it,
// for the pre-existing, separately-tracked adjacent limitations this file
// does not attempt to close -- see the package comment above.
func (h *rawHarness) cmpDecline(q string) {
	h.t.Helper()
	p, perr := h.edb.SnapshotPager()
	if perr != nil {
		h.t.Fatalf("SnapshotPager: %v", perr)
	}
	_, _, eerr := p.QueryArgs(q, nil)
	if eerr == nil {
		h.t.Errorf("[%s] expected a decline (pre-existing tracked limitation) but the engine answered it -- if this newly passes, tighten the scope comment instead of deleting the case", q)
	}
}

// TestRowValueAggWindowMined replays the exact statements mined from
// window1.test 44.2.2/44.3.2/44.4.2.
func TestRowValueAggWindowMined(t *testing.T) {
	h := newRAWHarness(t)
	h.cmpWant(`SELECT (0, 0) IN(SELECT MIN(c0), NTILE(0) OVER()) FROM t0`) // "argument of ntile must be a positive integer" on both sides
	h.cmpWant(`SELECT (0, 0) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`)
	h.exec(`INSERT INTO t0 VALUES(2), (1), (0)`)
	h.cmpWant(`SELECT (0, 1) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`)
}

// TestRowValueAggWindowVariants widens the mined bucket: different row-value
// arities, a swapped aggregate, a swapped window function, NOT IN, every
// row-sub-compare operator (not just IN), a NULL element on either side, and
// the "count(*) never hoists" control (no outer column named, so this stays a
// FROM-less aggregate+window query, not the aggregate-hoist retry -- it was
// already reachable before this fix and must stay that way).
func TestRowValueAggWindowVariants(t *testing.T) {
	h := newRAWHarness(t)
	h.exec(`INSERT INTO t0 VALUES(2), (1), (0)`)

	cases := []string{
		// three-column row value, still one hoisted aggregate
		`SELECT (0, 1, 5) IN(SELECT MIN(c0), NTILE(1) OVER(), 5) FROM t0`,
		`SELECT (0, 1, 6) IN(SELECT MIN(c0), NTILE(1) OVER(), 5) FROM t0`,
		// NOT IN
		`SELECT (0, 1) NOT IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 2) NOT IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		// swapped window function
		`SELECT (0, 1) IN(SELECT MIN(c0), ROW_NUMBER() OVER()) FROM t0`,
		`SELECT (0, 1) IN(SELECT MIN(c0), RANK() OVER()) FROM t0`,
		// swapped aggregate
		`SELECT (3, 1) IN(SELECT SUM(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (3, 1) IN(SELECT MAX(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (3, 1) IN(SELECT count(c0), NTILE(1) OVER()) FROM t0`,
		// every row-sub-compare operator, not just IN
		`SELECT (0, 1) = (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) == (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) != (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) <> (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) IS (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) IS NOT (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) < (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) <= (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) > (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) >= (SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		// mirrored operand order: subquery on the LEFT
		`SELECT (SELECT MIN(c0), NTILE(1) OVER()) = (0, 1) FROM t0`,
		`SELECT (SELECT MIN(c0), NTILE(1) OVER()) < (0, 1) FROM t0`,
		// NULL on either side of the probe
		`SELECT (NULL, 1) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, NULL) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		// empty t0 (the aggregate collapses to a NULL row, verified against
		// the SAME oracle-pinned "no rows" rule rowvalue_subquery_test.go
		// documents for the non-hoisted spelling)
		`SELECT (0, 1) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM (SELECT c0 FROM t0 WHERE 0)`,
		// count(*): no outer column named, so association keeps this a plain
		// FROM-less aggregate+window query (not the hoist retry) -- control
		// case pinning that the surrounding fix didn't disturb it.
		`SELECT (0, 0) IN (SELECT count(*), NTILE(1) OVER()) FROM t0`,
		`SELECT (SELECT count(*), NTILE(1) OVER()) IS NOT (0, 0) FROM t0`,
	}
	for _, q := range cases {
		h.cmpWant(q)
	}
}

// TestRowValueAggWindowAdjacentDeclines pins the limitations neighbouring this
// bucket, so a regression (silently answering one WRONG instead of declining
// it) would be caught here rather than passing unnoticed. One of the two has
// since been closed and is pinned by its ANSWER instead -- see below.
func TestRowValueAggWindowAdjacentDeclines(t *testing.T) {
	h := newRAWHarness(t)
	h.exec(`INSERT INTO t0 VALUES(2), (1), (0)`)
	// Two hoisted min/max calls in the same subquery.
	h.cmpDecline(`SELECT (0, 1, 5) IN(SELECT MIN(c0), NTILE(1) OVER(), MAX(c0)) FROM t0`)
	// An aggregate hoisted into an outer GROUP BY is SERVED now -- the anchor
	// guard no longer counts a subquery that reads nothing the grouping key
	// does not already fix (engine's anchorObservable). Compared rather than
	// declined, so it is the ANSWER that is pinned.
	h.cmpWant(`SELECT g, (0, 1) IN (SELECT MIN(c0), NTILE(1) OVER()) FROM t1 GROUP BY g`)
	h.cmpWant(`SELECT g, (0, 1) IN (SELECT MIN(c0), NTILE(1) OVER()) FROM t1 GROUP BY g ORDER BY g`)
}
