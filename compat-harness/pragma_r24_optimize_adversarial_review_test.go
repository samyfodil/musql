// Adversarial tests for PRAGMA optimize across schema shapes and connection
// boundaries.
package compat

import (
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// pragmaAdvBuildThenReopen builds identical schema and data against separate
// databases, then reopens each with a fresh connection and runs after-phase
// statements.
func pragmaAdvBuildThenReopen(t *testing.T, build, after []string) (cgoAfter, mushAfter []map[string]any) {
	t.Helper()
	dsnCgo := filepath.Join(t.TempDir(), "cgo.db")
	dsnMush := filepath.Join(t.TempDir(), "mush.db")
	cgoBuild := runWithDSN(t, "cgo", dsnCgo, build)
	mushBuild := runWithDSN(t, "musql", dsnMush, build)
	if len(cgoBuild) != len(build) || len(mushBuild) != len(build) {
		t.Fatalf("build phase: worker returned wrong result count (cgo=%d mush=%d want=%d)", len(cgoBuild), len(mushBuild), len(build))
	}
	for i, s := range build {
		if vjKinds(cgoBuild)[i] == "error" {
			t.Fatalf("build stmt #%d %q: ORACLE refused it -- premise is wrong", i, s)
		}
		if vjKinds(mushBuild)[i] == "error" {
			t.Fatalf("build stmt #%d %q: musql refused it -- premise is wrong", i, s)
		}
	}
	cgoAfter = runWithDSN(t, "cgo", dsnCgo, after)
	mushAfter = runWithDSN(t, "musql", dsnMush, after)
	if len(cgoAfter) != len(after) || len(mushAfter) != len(after) {
		t.Fatalf("after phase: worker returned wrong result count (cgo=%d mush=%d want=%d)", len(cgoAfter), len(mushAfter), len(after))
	}
	return cgoAfter, mushAfter
}

// pragmaAdvCheckDeclines verifies that musql's accept/decline verdicts match
// expected declines and non-declined statements match the oracle.
func pragmaAdvCheckDeclines(t *testing.T, name string, after []string, cgoAfter, mushAfter []map[string]any, wantDeclined map[int]bool) {
	t.Helper()
	for i, s := range after {
		if want, ok := wantDeclined[i]; ok {
			declined := vjKinds(mushAfter)[i] == "error"
			if declined != want {
				t.Errorf("[%s] stmt #%d %q: musql declined=%v, want declined=%v", name, i, s, declined, want)
			}
			if vjKinds(cgoAfter)[i] == "error" {
				t.Fatalf("[%s] stmt #%d %q: the ORACLE refused it -- premise is wrong", name, i, s)
			}
			continue
		}
		if got, want := vjCells(mushAfter[i:i+1]), vjCells(cgoAfter[i:i+1]); got != want {
			t.Errorf("[%s] stmt #%d %q DIVERGES\n  cgo:    %s\n  musql: %s", name, i, s, want, got)
		}
	}
}

// TestPragmaAdvReopenDropsSessionFlag verifies that in-memory session flags
// are not persisted and do not affect PRAGMA optimize across a reopen.
func TestPragmaAdvReopenDropsSessionFlag(t *testing.T) {
	build := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i ON t(a)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`ANALYZE`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t SELECT i%7, i FROM s`,
		`SELECT b FROM t WHERE a=2 LIMIT 1`, // sets TF_MaybeReanalyze on THIS connection only
	}
	after := []string{`PRAGMA optimize`, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`}
	cgoAfter, mushAfter := pragmaAdvBuildThenReopen(t, build, after)

	cgoStat := vjCells(cgoAfter[1:2])
	mushDeclined := vjKinds(mushAfter)[0] == "error"
	t.Logf("oracle stat1 after reopened PRAGMA optimize: %s", cgoStat)
	t.Logf("musql declined reopened PRAGMA optimize: %v", mushDeclined)

	if !mushDeclined {
		t.Fatalf("musql accepted a reopened-connection PRAGMA optimize over stale stats -- must decline since it cannot prove a no-op")
	}
	if !strings.Contains(cgoStat, `T:3 1`) {
		t.Errorf("oracle rewrote stat1 on a fresh reopened connection with no prior query (got %s, want stale T:3 1) -- TF_MaybeReanalyze may survive a reopen after all, re-read pragma.c/where.c before trusting the decline's premise", cgoStat)
	}
}

// 2. Same file, but the FIRST connection runs PRAGMA optimize itself right
// after the seek (same-connection accept), and a SECOND fresh connection
// reopens and re-reads stat1. Confirms musql's write (when it does accept)
// persists correctly across a reopen.
func TestPragmaAdvAcceptedWriteSurvivesReopen(t *testing.T) {
	build := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i ON t(a)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`ANALYZE`,
		`SELECT * FROM t WHERE a=2`,
		`PRAGMA optimize`,
	}
	readStat1 := []string{`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`}
	cgoBuild, mushBuild := pragmaAdvBuildThenReopen(t, build, readStat1)
	// pragmaAdvBuildThenReopen already asserts every BUILD statement (which
	// includes the PRAGMA optimize) succeeded on both engines -- so the
	// accept path is already confirmed; readStat1 is the reopened re-check.
	if got, want := vjCells(mushBuild), vjCells(cgoBuild); got != want {
		t.Errorf("stat1 diverges after reopen following an accepted PRAGMA optimize\n  cgo:    %s\n  musql: %s", want, got)
	}
}

// 3. WITHOUT ROWID table with a stale PK-as-index stat, reopened fresh (no
// seek at all).
func TestPragmaAdvWithoutRowidStaleNoSeek(t *testing.T) {
	build := []string{
		`CREATE TABLE t(a,b,c PRIMARY KEY) WITHOUT ROWID`,
		`INSERT INTO t VALUES(1,1,1),(2,2,2),(3,3,3)`,
		`ANALYZE`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t SELECT i%7, i, i+10000 FROM s`,
	}
	after := []string{`PRAGMA optimize`, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`}
	cgoAfter, mushAfter := pragmaAdvBuildThenReopen(t, build, after)
	pragmaAdvCheckDeclines(t, "without-rowid-stale-no-seek", after, cgoAfter, mushAfter, map[int]bool{0: true})
}

// 4. Partial index, stale after growth, reopened fresh (no seek).
func TestPragmaAdvPartialIndexStaleNoSeek(t *testing.T) {
	build := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i ON t(a) WHERE a IS NOT NULL`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`ANALYZE`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t SELECT i%7, i FROM s`,
	}
	after := []string{`PRAGMA optimize`, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`}
	cgoAfter, mushAfter := pragmaAdvBuildThenReopen(t, build, after)
	pragmaAdvCheckDeclines(t, "partial-index-stale-no-seek", after, cgoAfter, mushAfter, map[int]bool{0: true})
}

// 5. Expression index, fresh (matching) stats, a seek that uses the
// expression index -- confirms the accept path still agrees for the
// expression-index stat shape specifically (same-connection, then reopened
// verification of the write).
func TestPragmaAdvExpressionIndexFreshAfterSeek(t *testing.T) {
	build := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i ON t(a+b, b)`,
		`INSERT INTO t VALUES(1,1),(2,2),(1,1)`,
		`ANALYZE`,
		`SELECT b FROM t WHERE a+b=2`,
		`PRAGMA optimize`,
	}
	readStat1 := []string{`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`}
	cgoBuild, mushBuild := pragmaAdvBuildThenReopen(t, build, readStat1)
	if got, want := vjCells(mushBuild), vjCells(cgoBuild); got != want {
		t.Errorf("stat1 diverges: cgo=%s musql=%s", want, got)
	}
}

// 6. Multi-table JOIN driving whereCheckIfBloomFilterIsUseful (where.c:6637),
// the OTHER TF_MaybeReanalyze site (not whereLoopAddBtreeIndex): grow one
// table past threshold, then reopen fresh with NO prior query at all before
// PRAGMA optimize.
func TestPragmaAdvBloomFilterJoinReopenNoSeek(t *testing.T) {
	build := []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(b)`,
		`CREATE TABLE t3(c)`,
		`CREATE INDEX i1 ON t1(a)`,
		`CREATE INDEX i2 ON t2(b)`,
		`CREATE INDEX i3 ON t3(c)`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`INSERT INTO t2 VALUES(1),(2),(3)`,
		`INSERT INTO t3 VALUES(1),(2),(3)`,
		`ANALYZE`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t1 SELECT i%7 FROM s`,
	}
	after := []string{`PRAGMA optimize`, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`}
	cgoAfter, mushAfter := pragmaAdvBuildThenReopen(t, build, after)
	pragmaAdvCheckDeclines(t, "bloom-join-reopen-no-seek", after, cgoAfter, mushAfter, map[int]bool{0: true})
}

// 7. UNIQUE index full-key seek does NOT set bldFlags1==SQLITE_BLDF1_INDEXED
// (where.c:4294's comment: only a non-unique index, or a non-full-key use of
// a unique index, matters for stats). A connection doing ONLY a full-key
// unique-index seek before PRAGMA optimize, same connection, should behave
// like "no seek at all". musql must decline regardless (stale stats); the
// interesting check is whether the oracle's own carve-out holds live.
func TestPragmaAdvUniqueIndexFullKeySeekNoReanalyze(t *testing.T) {
	build := []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`ANALYZE`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t SELECT i+10, i FROM s`,
		`SELECT b FROM t WHERE a=2`, // full-key seek on the UNIQUE index
	}
	after := []string{`PRAGMA optimize`, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`}
	cgoAfter, mushAfter := pragmaAdvBuildThenReopen(t, build, after)

	if vjKinds(cgoAfter)[0] == "error" {
		t.Fatalf("oracle itself errored on PRAGMA optimize -- premise broken")
	}
	if vjKinds(mushAfter)[0] != "error" {
		t.Fatalf("musql ACCEPTED PRAGMA optimize over stats it cannot prove match a fresh ANALYZE (table grew far past the 10x threshold) -- this is the dangerous direction, a possible wrong answer")
	}
	cgoStat := vjCells(cgoAfter[1:2])
	t.Logf("oracle stat1 after unique full-key seek + optimize: %s", cgoStat)
	if strings.Contains(cgoStat, `T:3 1`) {
		t.Logf("CONFIRMED empirically: a full-key UNIQUE-index seek does NOT set TF_MaybeReanalyze (stat1 still stale after optimize) -- matches where.c:4294's comment")
	} else {
		t.Logf("a full-key UNIQUE-index seek DID cause a reanalyze (stat1=%s) -- where.c:4294's carve-out may not apply the way expected", cgoStat)
	}
}

// 8. DESC-key index, stale after reopen, no seek: computeStat1Rows claims to
// sort ascending regardless of a key's DESC flag when computing distinct
// counts. Confirms this doesn't accidentally make musql ACCEPT a stale
// stat1 as matching.
func TestPragmaAdvDescIndexStaleNoSeek(t *testing.T) {
	build := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i ON t(a DESC, b)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`ANALYZE`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t SELECT i%7, i FROM s`,
	}
	after := []string{`PRAGMA optimize`, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`}
	cgoAfter, mushAfter := pragmaAdvBuildThenReopen(t, build, after)
	pragmaAdvCheckDeclines(t, "desc-index-stale-no-seek", after, cgoAfter, mushAfter, map[int]bool{0: true})
}
