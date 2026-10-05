// This file tests RANGE frames with numeric offset bounds, including edge cases
// with non-numeric keys and NULL rows.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// rangeOffsetFrames is the bound grid tested with different fixtures.
var rangeOffsetFrames = []string{
	"RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING",
	"RANGE BETWEEN 2 PRECEDING AND 1 FOLLOWING",
	"RANGE BETWEEN UNBOUNDED PRECEDING AND 1 FOLLOWING",
	"RANGE BETWEEN 1 PRECEDING AND UNBOUNDED FOLLOWING",
	"RANGE BETWEEN CURRENT ROW AND 2 FOLLOWING",
	"RANGE BETWEEN 2 PRECEDING AND CURRENT ROW",
	"RANGE BETWEEN 0 PRECEDING AND 0 FOLLOWING",
	"RANGE BETWEEN 1 FOLLOWING AND 2 FOLLOWING",
	"RANGE BETWEEN 2 PRECEDING AND 1 PRECEDING",
	"RANGE BETWEEN 1 PRECEDING AND 2 PRECEDING",
	"RANGE BETWEEN 2 FOLLOWING AND 1 FOLLOWING",
	"RANGE BETWEEN 100 FOLLOWING AND UNBOUNDED FOLLOWING",
	"RANGE BETWEEN UNBOUNDED PRECEDING AND 100 PRECEDING",
	"RANGE BETWEEN 100 PRECEDING AND UNBOUNDED FOLLOWING",
	"RANGE BETWEEN UNBOUNDED PRECEDING AND 100 FOLLOWING",
	"RANGE BETWEEN 100 FOLLOWING AND 200 FOLLOWING",
	"RANGE BETWEEN 200 PRECEDING AND 100 PRECEDING",
	// A REAL offset against INTEGER keys, and the shorthand form.
	"RANGE BETWEEN 1.5 PRECEDING AND 0.5 FOLLOWING",
	"RANGE 1.5 PRECEDING",
}

// rangeOffsetOrderings is the other axis: direction x explicit NULLS
// placement. The NULLS clause is what makes rule 2 above observable.
var rangeOffsetOrderings = []string{
	"a", "a DESC",
	"a NULLS FIRST", "a NULLS LAST",
	"a DESC NULLS FIRST", "a DESC NULLS LAST",
}

// rangeOffsetSweep runs every (ordering x frame) pair over one fixture. The
// aggregate is sum+count so an EMPTY frame (sum NULL, count 0) is told apart
// from a frame holding only NULL contributions, and the outer ORDER BY is on
// the payload column so the comparison is row-for-row.
func rangeOffsetSweep(t *testing.T, name string, fixture []string) {
	t.Helper()
	probes := make([]string, 0, len(rangeOffsetOrderings)*len(rangeOffsetFrames))
	for _, ord := range rangeOffsetOrderings {
		for _, fr := range rangeOffsetFrames {
			probes = append(probes, fmt.Sprintf(
				`SELECT b, sum(b) OVER w, count(*) OVER w FROM t1 WINDOW w AS (ORDER BY %s %s) ORDER BY b`, ord, fr))
		}
	}
	driverParity(t, name, append(append([]string(nil), fixture...), probes...))
}

// TestWindowRangeOffsetParity sweeps the bound grid over each key-type mix.
func TestWindowRangeOffsetParity(t *testing.T) {
	// window1.test 19.0: "Test RANGE <expr> PRECEDING/FOLLOWING when there are
	// string, blob and NULL values in the dataset". Its own 19.2.1 expectation,
	// {1 3, 2 6, 3 9, 4 12, 5 9, a 6, b 7, c 8, d 9, e 10}, is the peer-group
	// degeneracy: 'a' +/- 1 is meaningless, so each TEXT key frames alone.
	rangeOffsetSweep(t, "range-offset-text", []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES (1,1),(2,2),(3,3),(4,4),(5,5),('a',6),('b',7),('c',8),('d',9),('e',10)`,
	})

	// window1.test 20.0 adds the NULL keys to that mix.
	rangeOffsetSweep(t, "range-offset-text-null", []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES (NULL,100),(NULL,101),(1,1),(2,2),(3,3),(4,4),(5,5),('a',6),('b',7),('c',8),('d',9),('e',10)`,
	})

	// Every storage class at once, including a TEXT that LOOKS numeric and a
	// BLOB: it is the value's TYPE that decides, so '3' frames alone rather
	// than pairing with 2.5.
	rangeOffsetSweep(t, "range-offset-all-types", []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES (1,1),(2.5,2),('3',3),(x'34',4),(NULL,5),('4x',6)`,
	})

	// window8.test 4.0: INTEGER keys with a NULL block and one peer group --
	// the fixture its 4.2.x/4.3.x cases use to pin NULLS FIRST/LAST.
	rangeOffsetSweep(t, "range-offset-int-null", []string{
		`CREATE TABLE t1(a INTEGER, b INTEGER)`,
		`INSERT INTO t1 VALUES (NULL,1),(NULL,2),(NULL,3),(10,4),(10,5)`,
	})

	// window8.test 3.0: REAL keys, a two-row peer group, no NULLs.
	rangeOffsetSweep(t, "range-offset-real", []string{
		`CREATE TABLE t1(a REAL, b INTEGER)`,
		`INSERT INTO t1 VALUES (5,10),(10,20),(13,26),(13,26),(15,30),(20,40),(22,80),(30,90)`,
	})

	// A partition whose keys are ALL NULL: every frame is the one peer group,
	// and an inverted bound pair does NOT empty it.
	rangeOffsetSweep(t, "range-offset-all-null", []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES (NULL,1),(NULL,2),(NULL,3)`,
	})

	// REAL keys ONE ULP apart at 1e16, where the offset arithmetic rounds:
	// this is the fixture that pins WHICH side of the comparison the offset is
	// added to (see rangeKeys.offsetBound), because "key+1" rounds up on the
	// PRECEDING side and down on the FOLLOWING side. Also carries +/-1e300 so
	// an offset can overflow toward an infinite limit.
	rangeOffsetSweep(t, "range-offset-ulp", []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES (1e16,1),(9999999999999998.0,2),(9999999999999996.0,3),
		 (1.0000000000000002e16,4),(1e300,5),(-1e300,6)`,
	})

	// INTEGER keys at the int64 extremes: key +/- offset overflows, and both
	// engines fall back to REAL rather than wrapping.
	rangeOffsetSweep(t, "range-offset-int-extremes", []string{
		`CREATE TABLE t1(a INTEGER, b INTEGER)`,
		`INSERT INTO t1 VALUES (-9223372036854775808,1),(-9223372036854775807,2),(0,3),
		 (9223372036854775806,4),(9223372036854775807,5)`,
	})

	// INFINITE keys with a FINITE offset stay supported (an infinite OFFSET is
	// the one declined sub-case -- see TestWindowRangeInfiniteOffsetDeclined).
	rangeOffsetSweep(t, "range-offset-inf-keys", []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES (-9e999,1),(1,2),(2,3),(9e999,4),(NULL,5)`,
	})
}

// TestWindowRangeOffsetExtras covers what the sweep's single window shape
// leaves out: PARTITION BY (each partition gets its own key run), EXCLUDE
// punching a hole in a RANGE frame, the frame-reading value functions, a
// COLLATE'd TEXT key (whose peers are the collation's), an expression key, and
// an offset big enough to overflow int64 arithmetic.
func TestWindowRangeOffsetExtras(t *testing.T) {
	driverParity(t, "range-offset-extras", []string{
		`CREATE TABLE t1(p, a, b)`,
		`INSERT INTO t1 VALUES (1,NULL,1),(1,NULL,2),(2,1,3),(2,2,4),(3,'x',5),(3,'y',6),(4,1,7),(4,NULL,8)`,
		`SELECT p, quote(a), sum(b) OVER (PARTITION BY p ORDER BY a RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t1 ORDER BY b`,
		`SELECT p, quote(a), sum(b) OVER (PARTITION BY p ORDER BY a NULLS LAST RANGE BETWEEN UNBOUNDED PRECEDING AND 1 FOLLOWING) FROM t1 ORDER BY b`,
		`SELECT p, quote(a), sum(b) OVER (PARTITION BY p ORDER BY a DESC NULLS FIRST RANGE BETWEEN 1 PRECEDING AND UNBOUNDED FOLLOWING) FROM t1 ORDER BY b`,

		`CREATE TABLE t2(a REAL, b INTEGER)`,
		`INSERT INTO t2 VALUES (5,10),(10,20),(13,26),(13,26),(15,30),(20,40),(22,80),(30,90)`,
		`SELECT b, sum(b) OVER (ORDER BY a RANGE BETWEEN 5 PRECEDING AND 5 FOLLOWING EXCLUDE CURRENT ROW) FROM t2 ORDER BY b`,
		`SELECT b, sum(b) OVER (ORDER BY a RANGE BETWEEN 5 PRECEDING AND 5 FOLLOWING EXCLUDE GROUP) FROM t2 ORDER BY b`,
		`SELECT b, sum(b) OVER (ORDER BY a RANGE BETWEEN 5 PRECEDING AND 5 FOLLOWING EXCLUDE TIES) FROM t2 ORDER BY b`,
		`SELECT b, first_value(b) OVER (ORDER BY a RANGE BETWEEN 5 PRECEDING AND 5 FOLLOWING) FROM t2 ORDER BY b`,
		`SELECT b, last_value(b) OVER (ORDER BY a RANGE BETWEEN 5 PRECEDING AND 5 FOLLOWING) FROM t2 ORDER BY b`,
		`SELECT b, nth_value(b,2) OVER (ORDER BY a RANGE BETWEEN 5 PRECEDING AND 5 FOLLOWING) FROM t2 ORDER BY b`,
		`SELECT b, group_concat(b) OVER (ORDER BY a RANGE BETWEEN 5 PRECEDING AND 5 FOLLOWING) FROM t2 ORDER BY b`,
		// int64 arithmetic overflows into REAL rather than wrapping.
		`SELECT b, sum(b) OVER (ORDER BY a RANGE BETWEEN 9223372036854775807 PRECEDING AND 9223372036854775807 FOLLOWING) FROM t2 ORDER BY b`,

		// A COLLATE NOCASE key: 'a' and 'A' are peers, so the degenerate frame
		// is the NOCASE peer group, not the single row.
		`CREATE TABLE t3(a TEXT COLLATE NOCASE, b INTEGER)`,
		`INSERT INTO t3 VALUES ('a',1),('A',2),('b',3),('B',4)`,
		`SELECT b, sum(b) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t3 ORDER BY b`,
		`SELECT b, sum(b) OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND 1 FOLLOWING) FROM t3 ORDER BY b`,

		// A TEXT-affinity column is TEXT keys -- every frame a peer group --
		// until an expression makes the key numeric.
		`CREATE TABLE t4(a TEXT, b INTEGER)`,
		`INSERT INTO t4 VALUES ('1',1),('2',2),('3',3),('10',4)`,
		`SELECT b, sum(b) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t4 ORDER BY b`,
		`SELECT b, sum(b) OVER (ORDER BY a+0 RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t4 ORDER BY b`,
		`SELECT b, sum(b) OVER (ORDER BY CAST(a AS INTEGER) RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t4 ORDER BY b`,
		`SELECT b, sum(b) OVER (ORDER BY -CAST(a AS INTEGER) RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t4 ORDER BY b`,
	})
}

// TestWindowFrameOffsetValueParity pins which OFFSET EXPRESSIONS a frame bound
// accepts. RANGE takes any non-negative NUMBER (window1.test section 22's own
// grid); ROWS and GROUPS take a non-negative INTEGER. Both apply NUMERIC
// affinity first, so '2' and '2.0' are fine and '' , '2.0x' and a BLOB are not
// -- x'3132' is rejected even though its bytes spell "12". Both also reject a
// FUNCTION CALL or a SUBQUERY, constant or not, while taking arithmetic, CAST
// and CASE.
func TestWindowFrameOffsetValueParity(t *testing.T) {
	var probes []string
	for _, off := range []string{
		"4.5", "NULL", "0.0", "0.1", "-0.1", "''", "'2.0'", "'2.0x'", "x'1234'",
		"x'3132'", "'1.2'", "-1", "1e2", "'  3 '", "2", "'2'", "(2)", "((2))",
		"1+1", "4-2", "2*1", "+2", "-(-2)", "2.5", "'2.5'", "2||''",
		"CAST(2 AS INTEGER)", "CAST('2' AS INTEGER)", "CASE WHEN 1 THEN 2 ELSE 3 END",
		"abs(-2)", "abs(2)", "max(2,1)", "length('ab')", "coalesce(2,3)",
		"nullif(2,3)", "(SELECT 2)", "(SELECT max(a) FROM t1)", "a",
	} {
		for _, mode := range []string{"ROWS", "RANGE", "GROUPS"} {
			probes = append(probes,
				fmt.Sprintf(`SELECT sum(b) OVER (ORDER BY a %s BETWEEN %s PRECEDING AND CURRENT ROW) FROM t1`, mode, off),
				fmt.Sprintf(`SELECT sum(b) OVER (ORDER BY a %s BETWEEN UNBOUNDED PRECEDING AND %s FOLLOWING) FROM t1`, mode, off))
		}
	}
	driverParity(t, "frame-offset-values", append([]string{
		`CREATE TABLE t1(a INTEGER, b INTEGER)`,
		`INSERT INTO t1 VALUES (1,1),(2,2),(3,3)`,
	}, probes...))
}

// TestWindowFrameFuzz is the randomized counterpart of the hand-built grids
// above: random key columns (every storage class, ties, NULLs), random
// orderings, random bounds in all three frame modes, random EXCLUDE and
// PARTITION BY, all compared against C SQLite. The seed is fixed so a
// failure is reproducible. It is what turned up "BETWEEN UNBOUNDED PRECEDING
// AND UNBOUNDED PRECEDING" (a syntax error in C SQLite that this engine was
// answering with an empty frame -- see checkWindowFrame).
func TestWindowFrameFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260726))
	vals := []string{"NULL", "0", "1", "2", "3", "-1", "1.5", "2.5", "'a'", "'b'", "'2'", "x'32'", "10", "-10", "0.0"}
	offs := []string{"0", "1", "2", "0.5", "1.5", "3", "10", "'1'", "2.0"}
	ords := []string{"a", "a DESC", "a NULLS FIRST", "a NULLS LAST", "a DESC NULLS FIRST", "a DESC NULLS LAST"}
	excl := []string{"", " EXCLUDE NO OTHERS", " EXCLUDE CURRENT ROW", " EXCLUDE GROUP", " EXCLUDE TIES"}
	// RANGE is over-weighted: it is the mode this file exists for.
	modes := []string{"RANGE", "RANGE", "RANGE", "ROWS", "GROUPS"}
	parts := []string{"", "PARTITION BY (b%2) ", "PARTITION BY (b%3) "}
	pick := func(ss []string) string { return ss[rng.Intn(len(ss))] }

	for iter := 0; iter < 40; iter++ {
		rows := make([]string, 3+rng.Intn(6))
		for i := range rows {
			rows[i] = fmt.Sprintf("(%s,%d)", pick(vals), i+1)
		}
		steps := []string{`CREATE TABLE tf(a, b)`, `INSERT INTO tf VALUES ` + strings.Join(rows, ",")}
		for q := 0; q < 24; q++ {
			off := pick(offs)
			bounds := []string{"UNBOUNDED PRECEDING", "CURRENT ROW", off + " PRECEDING", off + " FOLLOWING"}
			steps = append(steps, fmt.Sprintf(
				`SELECT b, sum(b) OVER w, count(*) OVER w, group_concat(b) OVER w, first_value(b) OVER w, last_value(b) OVER w`+
					` FROM tf WINDOW w AS (%sORDER BY %s %s BETWEEN %s AND %s%s) ORDER BY b`,
				pick(parts), pick(ords), pick(modes), pick(bounds), pick(bounds), pick(excl)))
		}
		driverParity(t, fmt.Sprintf("frame-fuzz-%d", iter), steps)
	}
}

// TestWindowRangeInfiniteOffsetDeclined pins the ONE RANGE-offset sub-case
// this engine still refuses: an INFINITE offset ("9e999 PRECEDING"). Real
// SQLite accepts it and then answers out of its NaN comparisons -- framing the
// -inf row with itself, and counting 6 rows in a 5-row table for one of the
// frames below -- which is a cursor position, not a span. See frameRangeOffset
// for the evidence. A decline is not a wrong answer; if someone reproduces
// SQLite's cursor behavior, this test SHOULD fail and its probes belong in the
// sweep above.
func TestWindowRangeInfiniteOffsetDeclined(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES (-9e999,1),(1,2),(2,3),(9e999,4),(NULL,5)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("fixture %q: %v", q, err)
		}
	}
	for _, q := range []string{
		`SELECT sum(b) OVER (ORDER BY a RANGE BETWEEN 9e999 PRECEDING AND 9e999 FOLLOWING) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY a RANGE BETWEEN 9e999 PRECEDING AND CURRENT ROW) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY a RANGE BETWEEN CURRENT ROW AND 9e999 FOLLOWING) FROM t1`,
		`SELECT sum(b) OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND 1e400 PRECEDING) FROM t1`,
	} {
		rows, qerr := db.Query(q)
		if qerr == nil {
			// Drain: this driver surfaces the decline only once rows are
			// fetched, and a silently-empty result would otherwise read as a
			// pass.
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Errorf("expected a decline, got a result: %s", q)
			continue
		}
		if !strings.Contains(qerr.Error(), "unsupported") {
			t.Errorf("expected an \"unsupported\" decline, got %v: %s", qerr, q)
		}
	}
}

// TestWindowFrameOffsetErrorText checks the offset errors' WORDING, which
// driverParity cannot (queryString folds every failure into "ERR"). Real
// SQLite says "number" for RANGE and "integer" for ROWS/GROUPS, and names the
// offending end -- verified directly against mattn/go-sqlite3.
func TestWindowFrameOffsetErrorText(t *testing.T) {
	for _, tc := range []struct{ sql, want string }{
		{`SELECT sum(b) OVER (ORDER BY a RANGE BETWEEN -1 PRECEDING AND CURRENT ROW) FROM t1`,
			"frame starting offset must be a non-negative number"},
		{`SELECT sum(b) OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND 'x' FOLLOWING) FROM t1`,
			"frame ending offset must be a non-negative number"},
		{`SELECT sum(b) OVER (ORDER BY a ROWS BETWEEN -1 PRECEDING AND CURRENT ROW) FROM t1`,
			"frame starting offset must be a non-negative integer"},
		{`SELECT sum(b) OVER (ORDER BY a GROUPS BETWEEN UNBOUNDED PRECEDING AND 2.5 FOLLOWING) FROM t1`,
			"frame ending offset must be a non-negative integer"},
	} {
		for _, drv := range []string{"sqlite", "sqlite3"} {
			db, oerr := sql.Open(drv, filepath.Join(t.TempDir(), "e.sqlite"))
			if oerr != nil {
				t.Fatal(oerr)
			}
			db.SetMaxOpenConns(1)
			for _, q := range []string{`CREATE TABLE t1(a INTEGER, b INTEGER)`, `INSERT INTO t1 VALUES (1,1),(2,2)`} {
				if _, err := db.Exec(q); err != nil {
					t.Fatalf("[%s] fixture %q: %v", drv, q, err)
				}
			}
			rows, err := db.Query(tc.sql)
			if err == nil {
				for rows.Next() {
				}
				err = rows.Err()
				rows.Close()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("[%s] %s\n  got %v\n  want an error containing %q", drv, tc.sql, err, tc.want)
			}
			db.Close()
		}
	}
}
