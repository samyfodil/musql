// This file gates PRAGMA optimize's side effect (what sqlite_stat1 holds
// afterwards) against C SQLite, for the cases musql accepts and those it
// declines. It cannot use differ(): a Query of PRAGMA optimize declines on the
// read side. musql decides per table from schema shape and WHERE-term shape
// (markMaybeReanalyze); 2+-table joins stay declined.
package compat

import (
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// pragmaR24RunBoth replays stmts on both engines and checks that:
//
//   - each PRAGMA optimize is accepted or declined as wantDeclined says, and
//     the oracle accepts it;
//   - up to the first decline, every other statement's cells match;
//   - each decline is either load-bearing (the oracle's run returned rows or
//     changed something visible) or listed in conservative.
//
// Comparing past the first decline would be meaningless.
func pragmaR24RunBoth(t *testing.T, name string, stmts []string, wantDeclined, conservative map[int]bool) {
	t.Helper()
	cgo := runWithDSN(t, "cgo", filepath.Join(t.TempDir(), "o.db"), stmts)
	mush := runWithDSN(t, "musql", filepath.Join(t.TempDir(), "m.db"), stmts)
	if len(cgo) != len(stmts) || len(mush) != len(stmts) {
		t.Fatalf("[%s] worker returned %d/%d results for %d statements", name, len(cgo), len(mush), len(stmts))
	}
	firstDecline, oracleActed := -1, false
	for i, s := range stmts {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "PRAGMA OPTIMIZE") {
			declined := vjKinds(mush)[i] == "error"
			if declined != wantDeclined[i] {
				t.Errorf("[%s] stmt #%d %q: musql declined=%v, expected declined=%v", name, i, s, declined, wantDeclined[i])
			}
			if k := vjKinds(cgo)[i]; k == "error" {
				t.Fatalf("[%s] stmt #%d %q: the ORACLE refused it -- this test's premise is wrong", name, i, s)
			}
			if declined && firstDecline < 0 {
				firstDecline = i
				// DEBUG mode returns rows, which alone justifies the decline.
				if r, _ := cgo[i]["rows"].([]any); len(r) > 0 {
					oracleActed = true
				}
			}
			continue
		}
		if firstDecline >= 0 {
			if vjCells(mush[i:i+1]) != vjCells(cgo[i:i+1]) {
				oracleActed = true
			}
			continue
		}
		if got, want := vjCells(mush[i:i+1]), vjCells(cgo[i:i+1]); got != want {
			t.Errorf("[%s] stmt #%d %q DIVERGES\n  cgo:    %s\n  musql: %s", name, i, s, want, got)
		}
	}
	if firstDecline >= 0 && oracleActed == conservative[firstDecline] {
		if oracleActed {
			t.Errorf("[%s] the decline at stmt #%d (%q) is listed as a deliberate over-refusal, but C SQLite DID act on it -- it is load-bearing after all",
				name, firstDecline, stmts[firstDecline])
		} else {
			t.Errorf("[%s] the decline at stmt #%d (%q) is not load-bearing: C SQLite returned no rows for it and nothing after it differs, so musql could have served it",
				name, firstDecline, stmts[firstDecline])
		}
	}
}

// TestPragmaR24OptimizeSideEffects covers accepts and declines. Every case
// reads sqlite_stat1 and sqlite_master back, so an accept that should have
// rewritten statistics fails here.
func TestPragmaR24OptimizeSideEffects(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
		// declined: which PRAGMA optimize statements musql must refuse.
		// conservative: refusals C SQLite does not need (deliberate over-refusals).
		declined     map[int]bool
		conservative map[int]bool
	}{
		// busy.test's PRAGMA optimize segment. The first three follow an ANALYZE
		// with nothing changed, so nothing is written. The last two follow a 200-row
		// insert, where t1's row really is rewritten.
		{"busy-test-segment", []string{
			`CREATE TABLE t1(x)`,
			`CREATE TABLE t2(y)`,
			`CREATE TABLE t3(z)`,
			`CREATE INDEX i1 ON t1(x)`,
			`CREATE INDEX i2 ON t2(y)`,
			`INSERT INTO t1 VALUES(1)`,
			`INSERT INTO t2 VALUES(1)`,
			`ANALYZE`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
			`SELECT * FROM t1 WHERE x=1`,
			`SELECT * FROM t2 WHERE y=1`,
			`PRAGMA optimize`,
			`PRAGMA optimize`,
			`PRAGMA optimize`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
			`SELECT name FROM sqlite_master ORDER BY name`,
			`PRAGMA schema_version`,
			`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<200) INSERT INTO t1 SELECT i FROM s`,
			`PRAGMA optimize`,
			`PRAGMA optimize`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
		}, map[int]bool{11: false, 12: false, 13: false, 18: false, 19: false}, nil},

		// A never-analyzed index is analyzed with no query at all, creating
		// sqlite_stat1.
		{"never-analyzed-index-is-accepted", []string{
			`CREATE TABLE t(a)`,
			`CREATE INDEX i ON t(a)`,
			`INSERT INTO t VALUES(1),(2),(3)`,
			`PRAGMA optimize`,
			`SELECT name FROM sqlite_master ORDER BY name`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
		}, map[int]bool{3: false}, nil},

		// ...but on an empty table that branch writes nothing.
		{"never-analyzed-index-on-an-empty-table", []string{
			`CREATE TABLE t(a)`,
			`CREATE INDEX i ON t(a)`,
			`PRAGMA optimize`,
			`SELECT name FROM sqlite_master ORDER BY name`,
		}, map[int]bool{2: false}, nil},

		// A table with no index is never a candidate, so this is a no-op.
		{"no-index-table", []string{
			`CREATE TABLE t(a)`,
			`INSERT INTO t VALUES(1)`,
			`ANALYZE`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
			`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<50) INSERT INTO t SELECT i FROM s`,
			`PRAGMA optimize`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
		}, map[int]bool{5: false}, nil},

		// The mask forms. With 0x02 clear optimize does nothing, so it is accepted;
		// 0x01 is DEBUG mode, whose rows this engine cannot compute.
		{"mask-forms", []string{
			`CREATE TABLE t(a)`,
			`CREATE INDEX i ON t(a)`,
			`INSERT INTO t VALUES(1),(2),(3)`,
			`PRAGMA optimize(0)`,
			`PRAGMA optimize(1)`,
			`PRAGMA optimize=0`,
			`PRAGMA optimize(0x10)`,
			`SELECT name FROM sqlite_master ORDER BY name`,
			`PRAGMA optimize(0x03)`,
			`SELECT name FROM sqlite_master ORDER BY name`,
		}, map[int]bool{3: false, 4: false, 5: false, 6: false, 8: true}, nil},

		// Hand-seeded statistics differ from what ANALYZE computes, so an optimize
		// that reached them would overwrite them.
		{"hand-seeded-stats", []string{
			`CREATE TABLE t(a)`,
			`CREATE INDEX i ON t(a)`,
			`INSERT INTO t VALUES(1),(2),(3)`,
			`ANALYZE`,
			`UPDATE sqlite_stat1 SET stat='90000 3' WHERE idx='i'`,
			`SELECT * FROM t WHERE a=2`,
			`PRAGMA optimize`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
		}, map[int]bool{6: false}, nil},

		// Current statistics plus an index-using query: nothing is written, since
		// the table has not moved.
		{"current-stats-after-an-index-seek", []string{
			`CREATE TABLE t(a,b)`,
			`CREATE INDEX i ON t(a)`,
			`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
			`ANALYZE`,
			`SELECT * FROM t WHERE a=2`,
			`PRAGMA optimize`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
			`PRAGMA schema_version`,
			`PRAGMA freelist_count`,
		}, map[int]bool{5: false}, nil},

		// A table above the analysis floor (2000 rows) is declined even with current
		// stats: optimize's limit can make its ANALYZE sample.
		{"above-the-analysis-floor-is-declined", []string{
			`CREATE TABLE t(a,b)`,
			`CREATE INDEX i ON t(a)`,
			`WITH s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<2500) INSERT INTO t SELECT n%7, n FROM s`,
			`ANALYZE`,
			`PRAGMA optimize`,
			`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
		}, map[int]bool{4: true}, map[int]bool{4: true}},

		// An empty database: nothing to analyze, nothing created.
		{"empty-database", []string{
			`PRAGMA optimize`,
			`SELECT name FROM sqlite_master ORDER BY name`,
		}, map[int]bool{0: false}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pragmaR24RunBoth(t, "optimize/"+tc.name, tc.stmts, tc.declined, tc.conservative)
		})
	}
}

// TestPragmaR24OptimizeIsConnectionLocal checks that PRAGMA optimize's answer
// depends on the connection, not just the file: the reanalyze flag is set in
// memory by the query planner. Two connections on identically built files, one
// running an index seek first, must get different sqlite_stat1 results.
func TestPragmaR24OptimizeIsConnectionLocal(t *testing.T) {
	build := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i ON t(a)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`ANALYZE`,
		// Grow the table past optimize's 10x threshold so a re-ANALYZE writes a
		// different row.
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t SELECT i%7, i FROM s`,
	}
	readStat1 := `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`

	dsnA := filepath.Join(t.TempDir(), "seek-then-optimize.db")
	runWithDSN(t, "cgo", dsnA, build)
	// A fresh connection: an index seek first, then PRAGMA optimize.
	afterSeek := runWithDSN(t, "cgo", dsnA, []string{
		`SELECT b FROM t WHERE a=2 LIMIT 1`,
		`PRAGMA optimize`,
		readStat1,
	})

	dsnB := filepath.Join(t.TempDir(), "optimize-only.db")
	runWithDSN(t, "cgo", dsnB, build)
	// Another fresh connection on an identical file: PRAGMA optimize with no
	// prior query.
	noSeek := runWithDSN(t, "cgo", dsnB, []string{
		`PRAGMA optimize`,
		readStat1,
	})

	seekStat := vjCells(afterSeek[2:3])
	noSeekStat := vjCells(noSeek[1:2])
	if seekStat == noSeekStat {
		t.Fatalf("oracle no longer disagrees with itself across connections on identical on-disk state (both read %s) -- PRAGMA optimize's real rule may have changed; re-read pragma.c's PragTyp_OPTIMIZE and where.c's TF_MaybeReanalyze sites before touching execOptimize's decline", seekStat)
	}
	// The seeking connection re-analyzed: its row count moved from "3 1".
	// Cells are type-tagged ("T:<value>").
	if strings.Contains(seekStat, `T:3 1`) {
		t.Errorf("expected the seeking connection's PRAGMA optimize to re-ANALYZE t (row count moved off the original 3-row snapshot), got %s", seekStat)
	}
	// The other connection's stat1 is unchanged.
	if !strings.Contains(noSeekStat, `T:3 1`) {
		t.Errorf("expected the non-seeking connection's PRAGMA optimize to leave stat1 at its stale 3-row snapshot (\"3 1\"), got %s", noSeekStat)
	}
}
