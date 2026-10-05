// Parity tests for window functions, frames, and named windows.
// The shared fixture has a tie in ORDER BY values to distinguish frame modes.
package compat

import "testing"

// windowFixture is the shared setup: see this file's package comment for why
// the a=3 tie is load-bearing.
var windowFixture = []string{
	`CREATE TABLE t(a,b)`,
	`INSERT INTO t VALUES(1,'A'),(2,'B'),(3,'C'),(3,'D'),(5,'E'),(7,'F')`,
}

func windowParity(t *testing.T, name string, probes []string) {
	t.Helper()
	driverParity(t, name, append(append([]string(nil), windowFixture...), probes...))
}

// TestWindowFrameParity covers frame modes, bounds, and exclusion options.
func TestWindowFrameParity(t *testing.T) {
	windowParity(t, "frame-modes", []string{
		// ROWS counts physical rows; RANGE counts ORDER BY VALUES; GROUPS
		// counts peer groups. Over the fixture these give visibly different
		// answers, which is the point of comparing them here.
		`SELECT b, sum(a) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a GROUPS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM t ORDER BY b`,
		// The shorthand form ("ROWS <bound>") means BETWEEN <bound> AND CURRENT ROW.
		`SELECT b, sum(a) OVER (ORDER BY a ROWS 2 PRECEDING) FROM t ORDER BY b`,
		// A frame with no ORDER BY at all frames by physical position.
		`SELECT sum(a) OVER (ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t`,
		// An EMPTY frame is not an error: sum() answers NULL, count() 0.
		`SELECT b, sum(a) OVER (ORDER BY a ROWS BETWEEN 3 FOLLOWING AND 4 FOLLOWING) FROM t ORDER BY b`,
		`SELECT b, count(a) OVER (ORDER BY a ROWS BETWEEN 3 FOLLOWING AND 4 FOLLOWING) FROM t ORDER BY b`,
		// Frames interact with PARTITION BY and with a non-numeric aggregate.
		`SELECT b, sum(a) OVER (PARTITION BY a%2 ORDER BY a ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, group_concat(b) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t ORDER BY b`,
	})

	windowParity(t, "frame-exclude", []string{
		`SELECT b, sum(a) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE NO OTHERS) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE GROUP) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE TIES) FROM t ORDER BY b`,
	})

	windowParity(t, "frame-errors", []string{
		// Each is C SQLite's own wording; the first two surface only once
		// rows are fetched, the last two at prepare time.
		`SELECT sum(a) OVER (ORDER BY a ROWS BETWEEN -1 PRECEDING AND CURRENT ROW) FROM t`,
		`SELECT sum(a) OVER (ORDER BY a RANGE BETWEEN 'x' PRECEDING AND CURRENT ROW) FROM t`,
		`SELECT sum(a) OVER (ORDER BY a ROWS BETWEEN CURRENT ROW AND 1 PRECEDING) FROM t`,
		`SELECT sum(a) OVER (ORDER BY a,b RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t`,
		// A function that does not READ the frame does not validate it either.
		`SELECT b, row_number() OVER (ORDER BY a ROWS BETWEEN -1 PRECEDING AND CURRENT ROW) FROM t ORDER BY b`,
	})
}

// TestWindowNamedParity covers WINDOW clause inheritance and refinement.
func TestWindowNamedParity(t *testing.T) {
	windowParity(t, "named-windows", []string{
		`SELECT b, sum(a) OVER w FROM t WINDOW w AS (ORDER BY a) ORDER BY b`,
		`SELECT b, sum(a) OVER w FROM t WINDOW w AS (PARTITION BY a%2 ORDER BY a) ORDER BY b`,
		// A reference may add the ORDER BY its base lacks...
		`SELECT b, sum(a) OVER (w ORDER BY a) FROM t WINDOW w AS (PARTITION BY a%2) ORDER BY b`,
		// ...or add a FRAME to a base that has the ordering.
		`SELECT b, sum(a) OVER (w ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t WINDOW w AS (ORDER BY a) ORDER BY b`,
		// A named window may itself reference another one.
		`SELECT b, sum(a) OVER w2 FROM t WINDOW w AS (ORDER BY a), w2 AS (w ROWS 1 PRECEDING) ORDER BY b`,
		// Two calls sharing one window.
		`SELECT b, sum(a) OVER w, count(*) OVER w FROM t WINDOW w AS (ORDER BY a) ORDER BY b`,
		`SELECT b, row_number() OVER w FROM t WINDOW w AS (ORDER BY a) ORDER BY b`,
		// The three errors, with SQLite's own wording.
		`SELECT b, sum(a) OVER nope FROM t WINDOW w AS (ORDER BY a)`,
		`SELECT b, sum(a) OVER (w PARTITION BY a) FROM t WINDOW w AS (ORDER BY a)`,
		`SELECT b, sum(a) OVER (w ORDER BY a) FROM t WINDOW w AS (ORDER BY a)`,
	})
}

// TestWindowValueFuncParity covers window value functions and their arity/argument rules.
func TestWindowValueFuncParity(t *testing.T) {
	windowParity(t, "ntile", []string{
		`SELECT b, ntile(3) OVER (ORDER BY a) FROM t ORDER BY b`,
		`SELECT b, ntile(4) OVER (ORDER BY a) FROM t ORDER BY b`,  // uneven: larger groups first
		`SELECT b, ntile(10) OVER (ORDER BY a) FROM t ORDER BY b`, // more groups than rows
		`SELECT b, ntile(1) OVER (ORDER BY a) FROM t ORDER BY b`,
		`SELECT b, ntile(2) OVER (PARTITION BY a%2 ORDER BY a) FROM t ORDER BY b`,
		`SELECT b, ntile(3) OVER () FROM t ORDER BY b`,
		`SELECT b, ntile(0) OVER (ORDER BY a) FROM t`,
		`SELECT b, ntile(NULL) OVER (ORDER BY a) FROM t`,
		`SELECT b, ntile(3,4) OVER (ORDER BY a) FROM t`,
	})
	windowParity(t, "lead-lag", []string{
		`SELECT b, lead(b) OVER (ORDER BY a) FROM t ORDER BY b`,
		`SELECT b, lag(b,2,'z') OVER (ORDER BY a) FROM t ORDER BY b`,
		`SELECT b, lead(b,0) OVER (ORDER BY a) FROM t ORDER BY b`,  // offset 0 is the row itself
		`SELECT b, lead(b,-1) OVER (ORDER BY a) FROM t ORDER BY b`, // negative reverses direction
		`SELECT b, lag(b,0,'z') OVER (ORDER BY a) FROM t ORDER BY b`,
		`SELECT b, lead(b,1,'zz') OVER (ORDER BY a) FROM t ORDER BY b`,
		`SELECT b, lead(b) OVER (PARTITION BY a%2 ORDER BY a) FROM t ORDER BY b`,
		// The frame is IGNORED by lead/lag.
		`SELECT b, lead(b) OVER (ORDER BY a ROWS BETWEEN CURRENT ROW AND CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, lead(b,1,2,3) OVER (ORDER BY a) FROM t`,
	})
	windowParity(t, "frame-value-funcs", []string{
		`SELECT b, first_value(b) OVER (ORDER BY a) FROM t ORDER BY b`,
		`SELECT b, last_value(b) OVER (ORDER BY a) FROM t ORDER BY b`, // default frame ends at the last PEER
		`SELECT b, first_value(b) OVER () FROM t ORDER BY b`,
		`SELECT b, first_value(b) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, last_value(b) OVER (ORDER BY a ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING) FROM t ORDER BY b`,
		`SELECT b, nth_value(b,2) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, nth_value(b,1) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t ORDER BY b`,
		// EXCLUDE moves which row is "first"/"last" in the frame.
		`SELECT b, first_value(b) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, last_value(b) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) FROM t ORDER BY b`,
		`SELECT b, last_value(b) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE GROUP) FROM t ORDER BY b`,
		// An empty frame answers NULL.
		`SELECT b, first_value(b) OVER (ORDER BY a ROWS BETWEEN 3 FOLLOWING AND 4 FOLLOWING) FROM t ORDER BY b`,
		`SELECT b, nth_value(b,0) OVER (ORDER BY a) FROM t`,
		`SELECT b, first_value() OVER (ORDER BY a) FROM t`,
	})
}

// TestWindowPartitionEmitOrder pins the ROW ORDER a partitioned window emits
// with no outer ORDER BY: C SQLite computes the window over ONE sorter
// keyed by (PARTITION BY, ORDER BY) and reads it back, so the partitions come
// out in KEY order, not in the order the scan first reached them.
func TestWindowPartitionEmitOrder(t *testing.T) {
	windowParity(t, "partition-emit-order", []string{
		`SELECT b, sum(a) OVER (PARTITION BY a%2 ORDER BY a) FROM t`,
		`SELECT b, sum(a) OVER (PARTITION BY a%2) FROM t`,
		`SELECT b, count(*) OVER (PARTITION BY b>'C' ORDER BY a DESC) FROM t`,
	})
}

// TestWindowRangeOffsetParityFixture is what used to be
// TestWindowRangeOffsetDeclined: the probes that pinned the ONE window shape
// this engine refused, a RANGE frame whose start or end is a numeric offset.
// They are compared against C SQLite cell by cell now that the span rule is
// implemented (engine/vdbe_window_frame.go's rangeKeys.offsetBound); the wider
// grid -- key types, NULLS FIRST/LAST, the runoff cases -- lives in
// vdbe_window_range_offset_test.go.
func TestWindowRangeOffsetParityFixture(t *testing.T) {
	windowParity(t, "range-offsets", []string{
		`SELECT b, sum(a) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t ORDER BY b`,
		`SELECT b, sum(a) OVER (ORDER BY a DESC RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM t ORDER BY b`,
		`SELECT sum(a) OVER (ORDER BY a RANGE BETWEEN 'x' PRECEDING AND CURRENT ROW) FROM t`,
		// Inverted bounds: an empty frame, so sum() is NULL and count() is 0.
		`SELECT sum(a) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 2 PRECEDING) FROM t`,
		`SELECT count(a) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 2 PRECEDING) FROM t`,
		`SELECT sum(a) OVER (ORDER BY a RANGE BETWEEN 2 FOLLOWING AND 1 FOLLOWING) FROM t`,
		`SELECT sum(a) OVER (ORDER BY a RANGE BETWEEN 6 FOLLOWING AND UNBOUNDED FOLLOWING) FROM t`,
		`SELECT sum(a) OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND 10 FOLLOWING) FROM t`,
	})
}
