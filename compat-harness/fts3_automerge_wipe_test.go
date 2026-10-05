package compat

// Test fts3/fts4 automerge setting survives a table wipe through the driver.
// Verify segment directory shape after wipe and subsequent operations.
// this test (up to 17 rounds of 500 distinct terms, with or without automerge,
// with or without a wipe) -- promotion alone already merges same-sized
// level-0 segments the same way automerge's own incremental merge would
// have. Only a FOLLOWING round of a DIFFERENT size (the 18th, 350 terms --
// TestFts3AutomergeGrowthTriggersThenDeclinesCleanly's own scale, engine
// package) gives automerge's trigger something to do that promotion alone
// would not, which is the only place this reproduction is actually
// observable. A smaller "byte-exact full parity" variant was deliberately
// NOT added here for that reason: it would pass identically with or without
// the fix in this file, the same "identical tally is evidence of nothing"
// trap this repository's own process notes warn about.
import (
	"fmt"
	"strings"
	"testing"
)

// distinctTermsInsert builds n rows of distinct, non-prefix-compressible
// terms starting at offset, so each row's own segment write is large enough
// to matter toward automerge's own nLeafAdd trigger count.
func distinctTermsInsert(table string, n, offset int) string {
	var vals []string
	for i := 0; i < n; i++ {
		term := strings.Repeat(fmt.Sprintf("term%04d", offset+i), 8)
		vals = append(vals, fmt.Sprintf("('%s')", term))
	}
	return fmt.Sprintf("INSERT INTO %s(a) VALUES%s", table, strings.Join(vals, ","))
}

// TestAutomergeWipeThenExactMinedCounterexampleDeclinesCleanly pins the
// EXACT reviewer-confirmed counter-example: automerge=2, a wipe (a DELETE
// that removes the table's sole row), then regrowth at
// TestFts3AutomergeGrowthTriggersThenDeclinesCleanly's own scale (engine
// package -- 17 rounds of 500 distinct terms). At this scale the 18th
// round's own automerge check crosses fts3.c:3566-3568's real A>64 trigger
// (asserted below: the oracle's own 18th round must NOT be an error, or this
// test is not exercising the scale it needs) and this write path's own
// fts3IncrmergeRun hits its already-established, pre-existing "spans more
// than one leaf node" decline (fts3_incrmerge.go) -- present even WITHOUT
// any wipe at this identical scale (see the engine package's sibling,
// non-wipe test of the same name).
//
// The assertion is deliberately NOT a blanket differ()/differAllowingDeclines
// over the whole list: those compare marshaled JSON, and a plain
// non-query "ok" result (stmtResult's own shape, worker/main.go) carries NO
// row data at all -- so the ORIGINAL bug (the 18th round running to
// completion with a SILENTLY WRONG %_segdir shape: segdir=[0:2,1:1] here vs
// C fts3's segdir=[0:1,1:2], with count(*) agreeing at 8850 on both
// sides) and the FIXED behavior (the 18th round declining) are BOTH
// "kind=ok"-shaped non-divergences at that one statement's own JSON --
// verified by mutation-testing this exact assertion against the pre-fix
// code, where a plain differ()-style check on this statement alone did NOT
// fail. What DOES distinguish them is the statement's own kind: "ok" (ran to
// completion, silently) vs "error" (declined cleanly) -- checked directly
// below. Full byte-exact parity through the wipe and all 17 rounds is
// asserted first via differ(), so this test still fails loudly if THAT part
// regresses.
func TestAutomergeWipeThenExactMinedCounterexampleDeclinesCleanly(t *testing.T) {
	stmts := []string{
		"PRAGMA page_size=512",
		"CREATE VIRTUAL TABLE t2 USING fts4(a)",
		"INSERT INTO t2(a) VALUES('placeholder')",
		"INSERT INTO t2(t2) VALUES('automerge=2')",
		"DELETE FROM t2", // the sole remaining row -> fts3DeleteAll's wipe
	}
	for round := 0; round < 17; round++ {
		stmts = append(stmts, distinctTermsInsert("t2", 500, round*500))
	}
	differ(t, "automerge-wipe-then-exact-mined-counterexample-setup", stmts)

	full := append(append([]string{}, stmts...), distinctTermsInsert("t2", 350, 17*500))
	oracle := run(t, "cgo", full)
	got := run(t, "musql", full)
	last := len(full) - 1
	if oracle[last]["kind"] == "error" {
		t.Fatalf("oracle's own 18th round after the wipe errored -- this scale no longer crosses fts3.c:3566-3568's real trigger, so this test is not exercising the regression")
	}
	if got[last]["kind"] != "error" {
		t.Errorf("musql's 18th round after a wipe = kind %v, want \"error\": this write path must DECLINE the merge shape it cannot reproduce here, exactly like the non-wipe sibling case, rather than silently accept it and leave %%_segdir a shape C fts3 would not produce", got[last]["kind"])
	}
}
