package engine

// Automerge=N caching across wipes and rebuilds. When the last row is deleted,
// %_stat's automerge row is cleared, but the connection's cached value must survive.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFts3AutomergeCacheSurvivesWipe(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('placeholder')`,
		`INSERT INTO t(t) VALUES('automerge=2')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := db.fts3ReadAutoincrmerge("t"); got != 2 {
		t.Fatalf("fts3ReadAutoincrmerge before wipe = %d, want 2", got)
	}
	if err := db.Exec(`DELETE FROM t`); err != nil { // the sole row -> fts3DeleteAll
		t.Fatalf("DELETE FROM t: %v", err)
	}
	if present, _ := fts3AutomergeStatRow2(t, db, "t"); present {
		t.Fatalf("%%_stat row id=2 still present after wipe -- this write path's own DELETE ALL %%_stat should have cleared it")
	}
	if got := db.fts3ReadAutoincrmerge("t"); got != 2 {
		t.Errorf("fts3ReadAutoincrmerge after wipe = %d, want 2 (the connection-lifetime cache, C fts3's own p->nAutoincrmerge, must survive the wipe)", got)
	}
}

func TestFts3AutomergeCacheSurvivesRebuild(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('placeholder')`,
		`INSERT INTO t(t) VALUES('automerge=2')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if present, _ := fts3AutomergeStatRow2(t, db, "t"); present {
		t.Fatalf("%%_stat row id=2 still present after rebuild -- fts3DoRebuild's own DELETE ALL %%_stat should have cleared it")
	}
	if got := db.fts3ReadAutoincrmerge("t"); got != 2 {
		t.Errorf("fts3ReadAutoincrmerge after rebuild = %d, want 2 (sticky, like the wipe case above)", got)
	}
}

// TestFts3AutomergeCommandOverwritesAnAlreadyResolvedCache: a SECOND
// automerge= command, issued after the value has already been resolved
// (cached) by an earlier flush, must win -- exactly like real
// fts3DoAutoincrmerge sets p->nAutoincrmerge directly (fts3_write.c:5193) in
// the SAME call that writes %_stat, never merely leaving the old cached
// value in place until some later re-resolve.
func TestFts3AutomergeCommandOverwritesAnAlreadyResolvedCache(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('placeholder')`,
		`INSERT INTO t(t) VALUES('automerge=2')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := db.fts3ReadAutoincrmerge("t"); got != 2 { // resolves and caches 2
		t.Fatalf("fts3ReadAutoincrmerge = %d, want 2", got)
	}
	if err := db.Exec(`INSERT INTO t(t) VALUES('automerge=4')`); err != nil {
		t.Fatalf("automerge=4: %v", err)
	}
	if got := db.fts3ReadAutoincrmerge("t"); got != 4 {
		t.Errorf("fts3ReadAutoincrmerge after a second automerge= command = %d, want 4 (must overwrite the already-cached 2)", got)
	}
}

// TestFts3AutomergeCacheDoesNotLeakAcrossDropAndRecreate is a regression
// this fix's own cache could otherwise introduce (caught while hardening
// this file, not part of the mined counter-example): DROP TABLE disconnects
// C fts3's Fts3Table instance entirely (sqlite3_vtab xDisconnect), so a
// table recreated under the SAME name gets a BRAND NEW one back, its own
// p->nAutoincrmerge freshly 0xff "unknown" -- fts3.c:1434 -- with no memory
// of whatever the dropped table's automerge=N was. drop_write.go's
// DropTable deletes this table's entry from db.fts3AutomergeCache in the
// same fts3 branch that drops its shadow tables, specifically so a stale
// resolved value can never leak into a same-named table created afterward
// (verified live: without that delete, a freshly created table -- no %_stat
// row at all yet -- kept answering the DROPPED table's last automerge=4).
func TestFts3AutomergeCacheDoesNotLeakAcrossDropAndRecreate(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('placeholder')`,
		`INSERT INTO t(t) VALUES('automerge=4')`,
		`DROP TABLE t`,
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := db.fts3ReadAutoincrmerge("t"); got != 0 {
		t.Errorf("fts3ReadAutoincrmerge on a freshly recreated table = %d, want 0 (the dropped table's cached automerge=4 must not leak into it)", got)
	}
}

// TestFts3AutomergeWipeThenGrowthTriggersThenDeclinesCleanly is
// TestFts3AutomergeGrowthTriggersThenDeclinesCleanly's own scale (17 rounds
// of 500 terms, then an 18th of 350 -- the exact scale that engine's own
// comment already proved crosses fts3.c:3566-3568's A>64 trigger for real),
// with a wipe inserted before growth starts and automerge=N never re-issued
// afterward. Before the fix in this file, growth after the wipe silently
// saw automerge as "off" (the fresh %_stat read found the row gone) and ran
// to completion with the WRONG %_segdir shape -- level=0:2/level=1:1 here vs
// C fts3's level=0:1/level=1:2, an oracle-confirmed live divergence, with
// row counts agreeing (8850) so no ordinary count/MATCH query would reveal
// it. With the fix, automerge correctly stays "on" through the wipe, so the
// 18th round's own leaf count now crosses the SAME real trigger the non-wipe
// sibling test above already established -- and hits the SAME pre-existing,
// already-accepted "spans more than one leaf node" decline
// (fts3_incrmerge.go) rather than silently producing a wrong shape. This is
// the correct outcome per this repository's own "never wrong" rule: a clean
// decline, not a byte-shape divergence that count(*)/MATCH can't see.
func TestFts3AutomergeWipeThenGrowthTriggersThenDeclinesCleanly(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	// page_size=512 keeps the run fast: fts3 sizes its leaves from it.
	if err := db.Exec(`PRAGMA page_size=512`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('placeholder')`,
		`INSERT INTO t(t) VALUES('automerge=2')`,
		`DELETE FROM t`, // the sole remaining row -> fts3DeleteAll's wipe
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for round := 0; round < 17; round++ {
		sqlText, args := fts3AutomergeBigInsertTerms(500)
		if _, _, err := db.ExecArgs(sqlText, args); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := p.QueryArgs(`SELECT count(*) FROM t`, nil)
	p.Close()
	if err != nil {
		t.Fatal(err)
	}
	beforeCount := rows[0][0].I

	sqlText, args := fts3AutomergeBigInsertTerms(350)
	n, _, err := db.ExecArgs(sqlText, args)
	if err == nil {
		t.Fatalf("18th INSERT after a wipe (should trigger automerge into an unported merge shape, exactly like the non-wipe case): want an error, got n=%d", n)
	}
	if !strings.Contains(err.Error(), "spanning more than one leaf node") {
		t.Errorf("18th INSERT after a wipe error = %q, want it to name the multi-leaf-spill decline (fts3_incrmerge.go)", err.Error())
	}

	// A declined statement leaves NOTHING behind: same row count, still
	// internally consistent -- exactly like the non-wipe sibling test.
	if err := db.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Fatalf("integrity-check after the declined 18th INSERT: %v", err)
	}
	p2, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows2, err := p2.QueryArgs(`SELECT count(*) FROM t`, nil)
	p2.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := rows2[0][0].I; got != beforeCount {
		t.Errorf("row count after the declined 18th INSERT (post-wipe) = %d, want unchanged %d", got, beforeCount)
	}
}
