// Test multi-trunk freelist chain built through both engines.
// Verify page_count, freelist_count, and integrity_check match C SQLite.
// layout the two engines end up with are not expected to match even where
// their counts do.
package compat

import (
	"fmt"
	"testing"
)

// TestFreelistMultiTrunkCgoFixturePops builds a chain of ~130 pages (comfortably
// past the 120-leaf write cap at page_size=512, so the chain is genuinely
// multi-trunk) via one large overflow chain, deleted by ROWID (never a bare
// WHERE-less DELETE -- see engine/freelist_pop_test.go's own
// buildOverflowFreelistFixture doc comment for the pre-existing, unrelated
// truncate-optimization gap that would otherwise leak the chain as
// "page N: never used"), then pops across FIVE separate growth batches,
// checking page_count/freelist_count/integrity_check against cgo after
// every one.
func TestFreelistMultiTrunkCgoFixturePops(t *testing.T) {
	const pageSize = 512
	const overflowPages = 130 // > freelistTrunkWriteCap(512) == 120 -- genuinely multi-trunk
	overflowLen := (pageSize - 4) * overflowPages

	base := []string{
		fmt.Sprintf(`PRAGMA page_size=%d`, pageSize),
		`CREATE TABLE t(b)`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, y TEXT)`,
		fmt.Sprintf(`INSERT INTO t VALUES(zeroblob(%d))`, overflowLen),
		`DELETE FROM t WHERE rowid = 1`,
	}
	read := []string{`PRAGMA page_count`, `PRAGMA freelist_count`, `PRAGMA integrity_check`}

	differ(t, "freelist/multitrunk-built", append(append([]string{}, base...), read...))

	// Five separate growth batches, checked after EVERY one -- each batch
	// on its own autocommit statement forces at least one leaf split
	// (60 rows comfortably exceeds one page's worth at this page size),
	// popping at least one page per batch.
	stmts := append([]string{}, base...)
	for i := 0; i < 5; i++ {
		start := i * 60
		stmts = append(stmts, fbeSeries(60, "INSERT INTO u", fmt.Sprintf("%d+value,'r'||value", start)))
		stmts = append(stmts, read...)
	}
	differ(t, "freelist/multitrunk-popped-across-5-batches", stmts)
}

// TestFreelistCgoBuiltGrowShrinkGrow is design doc section I.3's own named
// shape, built literally: "via cgo building the initial freelist, then a
// real musql session popping across several statements" -- the multi-trunk
// chain above is built the SAME way (a real overflow chain, deleted by
// rowid), but this test adds the GROW/shrink/grow sequence on top, checked
// against the cgo oracle after every phase (not merely musql's own
// CheckStructuralIntegrity, which engine/freelist_pop_test.go's own
// TestPopFreelistPageGrowShrinkGrow already does with a musql-built
// fixture -- this is the cross-engine counterpart of that same hazard:
// design doc section H3/musql-stage2-mechanism-design.md's F.2, "a
// structurally-odd-but-query-correct tree exercised as input to the next
// mutation").
//
// Each phase re-replays the FULL statement history through both engines
// from scratch (differ()'s own, established shape throughout this module --
// musql's autocommit-per-statement model makes a from-scratch replay of a
// deterministic statement sequence equivalent to one continuous session, so
// this is not a weaker check, just a costlier one), so "checked after every
// statement" here means after every PHASE (a batch of inserts, the shrink,
// the regrow) -- the same per-batch granularity
// TestPopFreelistPageMultiTrunkPageSizeMatrix and this file's own
// multitrunk-popped-across-5-batches case already use, for the identical
// reason: checking after literally every one of hundreds of individual rows
// would multiply this test's already-nontrivial subprocess cost with no
// additional coverage (a batch's own splits/pops are exercised regardless
// of how finely the checks are interleaved).
func TestFreelistCgoBuiltGrowShrinkGrow(t *testing.T) {
	const pageSize = 512
	const overflowPages = 130
	overflowLen := (pageSize - 4) * overflowPages
	read := []string{`PRAGMA page_count`, `PRAGMA freelist_count`, `PRAGMA integrity_check`}

	stmts := []string{
		fmt.Sprintf(`PRAGMA page_size=%d`, pageSize),
		`CREATE TABLE t(b)`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, y TEXT)`,
		fmt.Sprintf(`INSERT INTO t VALUES(zeroblob(%d))`, overflowLen),
		// WHERE rowid = 1, deliberately NOT a bare "DELETE FROM t": a
		// WHERE-less DELETE hits a pre-existing, Stage-3b-unrelated
		// truncate-optimization gap -- see this module's own
		// buildOverflowFreelistFixture-equivalent note in
		// engine/freelist_pop_test.go for the full argument. cgo's own
		// real freePage2 pushes build the INITIAL multi-trunk chain here.
		`DELETE FROM t WHERE rowid = 1`,
	}
	check := func(label string) {
		t.Helper()
		differ(t, label, append(append([]string{}, stmts...), read...))
	}
	check("freelist/grow-shrink-grow: cgo-built initial chain")

	// grow: 60 rows, comfortably forcing multiple splits/pops at this page
	// size.
	for i := 0; i < 60; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO u VALUES(%d, 'row%d')`, i, i))
	}
	check("freelist/grow-shrink-grow: after grow")

	// shrink: delete the second half by explicit id predicate (never a
	// bare WHERE-less DELETE -- see above). 3b never frees anything, so
	// this must not move page_count/freelist_count on EITHER engine.
	stmts = append(stmts, `DELETE FROM u WHERE id >= 30`)
	check("freelist/grow-shrink-grow: after shrink")

	// grow again: re-insert past the shrink point, exercising the ODD tree
	// shape shrink left behind (partially split, then partially drained)
	// as INPUT to a fresh incremental pop sequence.
	for i := 30; i < 90; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO u VALUES(%d, 'row%d')`, i, i))
	}
	check("freelist/grow-shrink-grow: after grow again")
}
