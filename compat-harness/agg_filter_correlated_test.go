package compat

// This file tests correlated references in aggregate FILTER and window PARTITION BY.
//     in both passes that matter (3.53.3, manifest d4c0e51e...782c62):
//     resolve.c:1352 resolves the FILTER's names inside the same NameContext
//     walk (which is what bumps pOuterNC->nRef and so sets
//     pItem->fg.isCorrelated at resolve.c:1953-1955), select.c:6544 analyses it
//     for aggregate references, and walker.c:31 is where the generic expression
//     walk reaches it at all. A plain aggregate's FILTER lives on y.pWin under
//     EP_WinFunc even with no OVER clause, which is why all three find it.
//
//  2. compileScanWindow DID bind its operands, including PARTITION BY -- but
//     THREW THE RESULT AWAY, on the reasoning that an unbindable reference
//     "simply surfaces as a run-time 'no such column', which is an error,
//     never a wrong answer". These statements are the counterexample.
//     It now declines, which lets the write path take the route that CAN
//     deliver the reference and leaves the read path with a compile-time
//     failure instead of a guaranteed run-time one. An AMBIGUOUS name is
//     excepted and still runs, because there the run-time arm words the error
//     the way SQLite words it.
//
// The WRITE asymmetry is the tell that named the second root: on the read path
// the enclosing scan holds o in a live cursor scope and the bind succeeds, so
// only FILTER failed there; inside an UPDATE the target row lives in REGISTERS
// (compiler.regScopes), which bindOuterAggRef cannot walk and OpOuterColumn
// cannot read, so the bind fails and both spellings failed.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var aggFilterCorrelatedSchema = []string{
	"CREATE TABLE o(a,b)",
	"CREATE TABLE inr(x)",
	"INSERT INTO o VALUES(1,NULL),(2,NULL)",
	"INSERT INTO inr VALUES(1),(2),(2)",
}

// TestAggFilterCorrelatedRead pins the read half. Each query returns one value
// per o row in rowid order, so the comparison is exact without an ORDER BY.
func TestAggFilterCorrelatedRead(t *testing.T) {
	for _, q := range []string{
		// The FILTER root. o.a is correlated, and the FILTER is the only place
		// it appears.
		"SELECT (SELECT count(*) FILTER (WHERE x = o.a) FROM inr) FROM o",
		"SELECT (SELECT sum(x) FILTER (WHERE x = o.a) FROM inr) FROM o",
		"SELECT (SELECT count(*) FILTER (WHERE x <> o.a) FROM inr) FROM o",
		// ...beside a correlated ARGUMENT, which already worked: the two must
		// not disagree about which mechanism marks the compile correlated.
		"SELECT (SELECT sum(x + o.a) FILTER (WHERE x = o.a) FROM inr) FROM o",
		// ...and with a GROUP BY, where the FILTER is per row and the group's
		// own key is not the correlated thing.
		"SELECT (SELECT count(*) FILTER (WHERE x = o.a) FROM inr GROUP BY x LIMIT 1) FROM o",
		// The window operands, which the read path already answered -- controls
		// that must not move now that a failed bind declines.
		"SELECT (SELECT sum(x) OVER (PARTITION BY o.a) FROM inr LIMIT 1) FROM o",
		"SELECT (SELECT sum(x) OVER (ORDER BY o.a) FROM inr LIMIT 1) FROM o",
		"SELECT (SELECT sum(x) FROM inr WINDOW w AS (ORDER BY o.a)) FROM o",
		"SELECT (SELECT count(*) OVER () FILTER (WHERE x = o.a) FROM inr LIMIT 1) FROM o",
		// The plain correlated WHERE, the shape that always worked.
		"SELECT (SELECT count(*) FROM inr WHERE x = o.a) FROM o",
		// EXISTS and IN spellings of the same FILTER.
		"SELECT EXISTS (SELECT count(*) FILTER (WHERE x = o.a) FROM inr HAVING count(*) > 0) FROM o",
		"SELECT o.a IN (SELECT count(*) FILTER (WHERE x = o.a) FROM inr) FROM o",
	} {
		if !differ(t, "aggfiltercorr", append(append([]string(nil), aggFilterCorrelatedSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestAggFilterCorrelatedWrite pins the write half, where BOTH roots showed.
// Each case resets b first so the statements are independent, and reads the
// table back so a silently-skipped write is a divergence rather than a pass.
func TestAggFilterCorrelatedWrite(t *testing.T) {
	for _, q := range []string{
		"UPDATE o SET b = (SELECT count(*) FILTER (WHERE x = o.a) FROM inr)",
		"UPDATE o SET b = (SELECT sum(x) FILTER (WHERE x = o.a) FROM inr)",
		"UPDATE o SET b = (SELECT sum(x) OVER (PARTITION BY o.a) FROM inr LIMIT 1)",
		"UPDATE o SET b = (SELECT sum(x) OVER (ORDER BY o.a) FROM inr LIMIT 1)",
		"UPDATE o SET b = (SELECT sum(x) FROM inr WINDOW w AS (ORDER BY o.a))",
		"UPDATE o SET b = (SELECT count(*) FROM inr WHERE x = o.a)",
		"DELETE FROM o WHERE (SELECT count(*) FILTER (WHERE x = o.a) FROM inr) > 1",
	} {
		stmts := append(append([]string(nil), aggFilterCorrelatedSchema...),
			"UPDATE o SET b = NULL", q, "SELECT a, b FROM o ORDER BY a")
		if !differ(t, "aggfiltercorrwrite", stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
