// Gates PRAGMA optimize's session-tracked accept path, with persistent session
// state matching how TestTCLCorpus replays files.
package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// optExec runs one write statement.
func optExec(t *testing.T, db *Session, sqlText string) {
	t.Helper()
	if _, _, err := db.ExecArgs(sqlText, nil); err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
}

// optQuery runs one read statement, returning rows as space-joined strings.
func optQuery(t *testing.T, db *Session, sqlText string) []string {
	t.Helper()
	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pager.Close()
	_, rows, err := pager.QueryArgs(sqlText, nil)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, v := range row {
			switch v.Typ {
			case Text:
				cells[j] = string(v.S)
			case Int:
				cells[j] = strconv.FormatInt(v.I, 10)
			default:
				cells[j] = "?"
			}
		}
		out[i] = strings.Join(cells, "|")
	}
	return out
}

// TestPragmaOptimizeBusyTestSegment reproduces busy.test's own PRAGMA
// optimize scenario (~/.cache/musql/sqlite-353/test/busy.test lines 63-136,
// the "3.1"/"3.6"/"3.7" statements once the busy-handler/second-connection
// parts -- irrelevant to sqlite_stat1 -- are stripped, matching this
// bucket's own mined statement "busy.test#1"), against a SINGLE persistent
// *DB, and pins the exact result live-verified against the oracle this
// session (sqlite3 3.53.3, SOURCE_ID d4c0e51e...82c62):
//
//	CREATE TABLE t1(x); CREATE TABLE t2(y);
//	CREATE INDEX i1 ON t1(x); CREATE INDEX i2 ON t2(y);
//	INSERT INTO t1 VALUES(1); INSERT INTO t2 VALUES(1);
//	ANALYZE;
//	SELECT * FROM t1 WHERE x=1;   -- same connection, before PRAGMA optimize
//	SELECT * FROM t2 WHERE y=1;
//	WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<1000)
//	  INSERT INTO t1 SELECT i FROM s;   -- t1 now 1001 rows, >10x its stat1 count
//	PRAGMA optimize;
//	SELECT * FROM sqlite_stat1 ORDER BY tbl;
//
// Oracle: t1|i1|1001 1 (rewritten), t2|i2|1 1 (untouched -- t2 never grew).
func TestPragmaOptimizeBusyTestSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE t1(x)`,
		`CREATE TABLE t2(y)`,
		`CREATE INDEX i1 ON t1(x)`,
		`CREATE INDEX i2 ON t2(y)`,
		`INSERT INTO t1 VALUES(1)`,
		`INSERT INTO t2 VALUES(1)`,
		`ANALYZE`,
	} {
		optExec(t, db, s)
	}
	if got := optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`); len(got) != 2 || got[0] != "t1|i1|1 1" || got[1] != "t2|i2|1 1" {
		t.Fatalf("post-ANALYZE sqlite_stat1 = %v, want [t1|i1|1 1 t2|i2|1 1]", got)
	}

	// The WHERE queries that set maybeReanalyze for BOTH t1 and t2 -- this is
	// what distinguishes this scenario from a fresh connection with no query
	// history (TestPragmaR24OptimizeIsConnectionLocal's own "no-seek" half,
	// compat-harness).
	optQuery(t, db, `SELECT * FROM t1 WHERE x=1`)
	optQuery(t, db, `SELECT * FROM t2 WHERE y=1`)

	optExec(t, db, `WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<1000) INSERT INTO t1 SELECT i FROM s`)

	if _, _, err := db.ExecArgs(`PRAGMA optimize`, nil); err != nil {
		t.Fatalf("PRAGMA optimize: %v (expected accept -- t1 has a session-tracked WHERE match and moved >10x, t2 has a session-tracked WHERE match but did not move)", err)
	}

	got := optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`)
	want := []string{"t1|i1|1001 1", "t2|i2|1 1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("post-optimize sqlite_stat1 = %v, want %v", got, want)
	}

	// A SECOND, immediate PRAGMA optimize (busy.test's own follow-on
	// statement) must be a no-op: t1's baseline was refreshed by the first
	// call (refreshStat1Baseline, pragma_optimize_track.go), so the growth
	// check now finds it inside the window, and t2 never moved either.
	if _, _, err := db.ExecArgs(`PRAGMA optimize`, nil); err != nil {
		t.Fatalf("second PRAGMA optimize: %v (expected accept, no-op)", err)
	}
	got = optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("post-second-optimize sqlite_stat1 = %v, want %v (unchanged)", got, want)
	}
}

// TestPragmaOptimizeFreshSessionNeverQueriedStillDeclines pins the OTHER
// half of TestPragmaR24OptimizeIsConnectionLocal's own live-oracle proof
// (compat-harness): a table that moved >10x but that THIS *DB never itself
// queried must not be silently left stale under a false "accept" -- declined
// rather than guessed, per execOptimize's own "!maybeReanalyzed" fallback
// (pragma.go). This is the scenario a naive "no session history -> skip"
// design would get wrong (see that function's own doc comment for the
// driver angle this guards against too).
func TestPragmaOptimizeFreshSessionNeverQueriedStillDeclines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noseek.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i ON t(a)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3)`,
		`ANALYZE`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t SELECT i%7, i FROM s`,
	} {
		optExec(t, db, s)
	}
	// No SELECT ... WHERE a=... runs here at all -- unlike the busy-test-
	// segment case above.
	_, _, err = db.ExecArgs(`PRAGMA optimize`, nil)
	if err == nil {
		t.Fatalf("PRAGMA optimize unexpectedly accepted a table that moved >10x with no query this *DB ever ran against it")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("PRAGMA optimize: got %v, want an errVDBEUnsupported decline", err)
	}
	got := optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`)
	if len(got) != 1 || got[0] != "t|i|3 1" {
		t.Fatalf("sqlite_stat1 after the declined PRAGMA optimize = %v, want unchanged [t|i|3 1] (a decline must leave nothing written)", got)
	}
}

// TestPragmaOptimizeNotNullColumnIsNullDoesNotMarkReanalyze is a fix-forward
// regression pin for a review-caught wrong answer in the ORIGINAL version of
// this bucket's fix (isSeekKeyCandidate/maybeReanalyzeConjunctMatches used to
// accept "col IS NULL" as a seek-key match against ANY indexed column, with
// no NOT NULL check at all).
//
// where.c:613-626 (indexColumnNotNull) and its guard at the
// whereLoopAddBtreeIndex call site (where.c:3291-3294: "if( (eOp==WO_ISNULL
// || (pTerm->wtFlags&TERM_VNULL)!=0) && indexColumnNotNull(...) ) continue")
// mean a NOT NULL column can NEVER take the WO_ISNULL branch -- "continue"
// skips the bldFlags1 update that ORs TF_MaybeReanalyze into pTab->tabFlags
// entirely, so "WHERE a IS NULL" against a NOT NULL indexed column
// contributes NOTHING towards TF_MaybeReanalyze, exactly like it would for a
// column with no index at all.
//
// Verified live against the oracle (sqlite3 3.53.3,
// /tmp/musql-sqlite3-build/sqlite3): seeded to 3000 rows and ANALYZEd
// (sqlite_stat1 "3000 1"), `SELECT * FROM t1 WHERE a IS NULL` then a shrink
// to 1 row leaves a following PRAGMA optimize a complete no-op -- stat1
// stays "3000 1" -- because condition 4c (TF_MaybeReanalyze) is false and
// the stat1 row already exists (4b false too), so pragma.c's own "(4b OR
// 4c) AND (5a OR 5b)" rule is false regardless of how far the table moved.
//
// The unfixed engine wrongly marked t1 maybe-reanalyzed from this WHERE
// query, then (since t1 truly did shrink far outside the growth window)
// PRAGMA optimize accepted and rewrote sqlite_stat1 to "1 1" -- a BYTE-WRONG
// answer, not a decline. With the NOT NULL guard
// (columnSideMatchesNullableCandidate, pragma_optimize_track.go) restored,
// maybeReanalyzed(t1) is correctly false, so execOptimize's own
// "!maybeReanalyzed" fallback (pragma.go) takes over: t1's actual row count
// (1) is far outside stat1's on-disk baseline (3000), so it cannot prove the
// growth check negative and DECLINES cleanly instead -- safe (nothing
// written) rather than wrong, per this engine's own "never wrong" rule. This
// is a real, measured cost (C SQLite serves this case; musql now
// declines it) rather than a full match, but declining is what this
// engine's own architecture (see execOptimize's own doc comment on
// driver's throwaway-*DB session model) already does for every
// "unmarked but moved far" table -- it is not a NEW gap this fix opens.
func TestPragmaOptimizeNotNullColumnIsNullDoesNotMarkReanalyze(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notnull_isnull.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE t1(a INTEGER NOT NULL, b INT)`,
		`CREATE INDEX i1 ON t1(a)`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t1 SELECT i, i FROM s`,
		`ANALYZE`,
	} {
		optExec(t, db, s)
	}
	if got := optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`); len(got) != 1 || got[0] != "t1|i1|3000 1" {
		t.Fatalf("post-ANALYZE sqlite_stat1 = %v, want [t1|i1|3000 1]", got)
	}

	// The exact regressed shape: IS NULL against a NOT NULL indexed column.
	optQuery(t, db, `SELECT * FROM t1 WHERE a IS NULL`)

	// Shrink drastically -- if maybeReanalyze were (wrongly) set, this would
	// trip the growth check and the table would get rewritten.
	optExec(t, db, `DELETE FROM t1 WHERE rowid > 1`)

	if _, _, err := db.ExecArgs(`PRAGMA optimize`, nil); err == nil {
		t.Fatalf("PRAGMA optimize unexpectedly accepted -- 'a IS NULL' against NOT NULL column 'a' must not mark t1 maybe-reanalyzed")
	} else if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("PRAGMA optimize: got %v, want an errVDBEUnsupported decline", err)
	}

	got := optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`)
	if len(got) != 1 || got[0] != "t1|i1|3000 1" {
		t.Fatalf("sqlite_stat1 after the declined PRAGMA optimize = %v, want UNCHANGED [t1|i1|3000 1] (the regression rewrote this to t1|i1|1 1)", got)
	}
}

// TestPragmaOptimizeNotNullColumnIsNullWithinWindowIsANoOp is the same
// column-shape and WHERE-shape as the test above, but with the table's
// growth kept INSIDE the 10x window (condition 5b false) -- so PRAGMA
// optimize must be a full, silent ACCEPT (matching the oracle exactly, not
// merely a safe decline), since neither condition 4 nor condition 5 holds
// regardless of whether "a IS NULL" ever marked t1. Verified live against
// the oracle: 100 rows, ANALYZE ("100 1"), the same IS NULL query, then 50
// more rows (150 total, well inside the window) -- PRAGMA optimize writes
// nothing on either engine.
func TestPragmaOptimizeNotNullColumnIsNullWithinWindowIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notnull_isnull_noop.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE t1(a INTEGER NOT NULL, b INT)`,
		`CREATE INDEX i1 ON t1(a)`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<100) INSERT INTO t1 SELECT i, i FROM s`,
		`ANALYZE`,
	} {
		optExec(t, db, s)
	}
	before := optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`)
	if len(before) != 1 || before[0] != "t1|i1|100 1" {
		t.Fatalf("post-ANALYZE sqlite_stat1 = %v, want [t1|i1|100 1]", before)
	}

	optQuery(t, db, `SELECT * FROM t1 WHERE a IS NULL`)
	optExec(t, db, `WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<50) INSERT INTO t1 SELECT i+1000, i+1000 FROM s`)

	if _, _, err := db.ExecArgs(`PRAGMA optimize`, nil); err != nil {
		t.Fatalf("PRAGMA optimize unexpectedly declined: %v (want a silent accept, matching the oracle)", err)
	}
	after := optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`)
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("sqlite_stat1 = %v, want unchanged %v (matches oracle: within the growth window, nothing is rewritten)", after, before)
	}
}

// TestPragmaOptimizeNotNullColumnEqualsNullStillMarksReanalyze guards the
// fix above against over-narrowing: indexColumnNotNull's guard
// (where.c:3291-3294) applies ONLY to eOp==WO_ISNULL / TERM_VNULL, never to
// a literal-NULL "=" comparison, which compiles to plain WO_EQ. So
// "WHERE a = NULL" against a NOT NULL column must still mark t1
// maybe-reanalyzed and PRAGMA optimize must still rewrite its stat1 row,
// exactly like "WHERE a = 5" would.
//
// Verified live against the oracle with the IDENTICAL setup as
// TestPragmaOptimizeNotNullColumnIsNullDoesNotMarkReanalyze (3000 rows,
// ANALYZE, shrink to 1 row): "a = NULL" DOES cause PRAGMA optimize to
// rewrite sqlite_stat1 to "1 1", unlike "a IS NULL" on the same column and
// same shrink, which leaves it at "3000 1". Narrowing BOTH shapes -- a
// surface reading of indexColumnNotNull's own name might suggest doing so --
// would itself be a new wrong answer: declining a table C SQLite actually
// reanalyzes.
func TestPragmaOptimizeNotNullColumnEqualsNullStillMarksReanalyze(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notnull_eqnull.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE t1(a INTEGER NOT NULL, b INT)`,
		`CREATE INDEX i1 ON t1(a)`,
		`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3000) INSERT INTO t1 SELECT i, i FROM s`,
		`ANALYZE`,
	} {
		optExec(t, db, s)
	}

	optQuery(t, db, `SELECT * FROM t1 WHERE a = NULL`)
	optExec(t, db, `DELETE FROM t1 WHERE rowid > 1`)

	if _, _, err := db.ExecArgs(`PRAGMA optimize`, nil); err != nil {
		t.Fatalf("PRAGMA optimize unexpectedly declined: %v (want an accept -- 'a = NULL' still marks t1 maybe-reanalyzed, matching the oracle)", err)
	}
	got := optQuery(t, db, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`)
	if len(got) != 1 || got[0] != "t1|i1|1 1" {
		t.Fatalf("sqlite_stat1 after optimize = %v, want [t1|i1|1 1] (matches the oracle: '=NULL' still triggers reanalyze)", got)
	}
}

// TestPragmaOptimizeFreshSessionUpToDateStatsIsNoOp is Stage 0's own Finding
// 2a regression test (reviewer 3's repro, table_load.go's lazy-load design):
// tableMissingStat1Index (pragma_optimize_track.go) used to read
// sqlite_stat1.rows DIRECTLY, with no ensureTableLoaded gate of its own at
// all -- not an error-swallowing bug, a completely missing one. A FRESH *DB
// session (exactly driver's own documented autocommit-per-statement
// model, and exactly SQLite's own recommended "run PRAGMA optimize just
// before closing each connection" usage) running ONLY "PRAGMA optimize"
// against a table whose sqlite_stat1 is already fully current used to see
// sqlite_stat1 as an EMPTY map (never loaded), wrongly concluding the
// index's own stat1 entry was missing and unconditionally re-ANALYZE-ing it
// -- a spurious DELETE+INSERT into sqlite_stat1 where C SQLite (and this
// engine, once sqlite_stat1 is eager-loaded like sqlite_sequence already
// was, see writer_open.go's eagerLoad) does nothing at all. Verified as a
// byte-for-byte no-op: the file PRAGMA optimize touches nothing must be
// IDENTICAL before and after.
func TestPragmaOptimizeFreshSessionUpToDateStatsIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat1_noop.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	optExec(t, db, `CREATE TABLE t(a)`)
	optExec(t, db, `CREATE INDEX i ON t(a)`)
	for i := 0; i < 50; i++ {
		optExec(t, db, `INSERT INTO t VALUES (`+strconv.Itoa(i)+`)`)
	}
	optExec(t, db, `ANALYZE`)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := fileAndDelta(t, path)

	db2, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	// This session's ONLY statement -- sqlite_stat1 is this session's first
	// (and only) touch of anything, exactly the shape that exposed the bug.
	if _, _, err := db2.ExecArgs(`PRAGMA optimize`, nil); err != nil {
		t.Fatalf("PRAGMA optimize: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := fileAndDelta(t, path)
	if !bytes.Equal(before, after) {
		t.Fatalf("PRAGMA optimize on a fresh session rewrote the file (want a byte-identical no-op: index i's sqlite_stat1 row was already current, and nothing in this session's single statement should have touched it) -- before=%d bytes, after=%d bytes (file and delta)", len(before), len(after))
	}
}

// fileAndDelta is a database's committed bytes: the segment file and the delta
// beside it, where every commit that is not a rewrite lands.
func fileAndDelta(t *testing.T, path string) []byte {
	t.Helper()
	var all []byte
	for _, p := range []string{path, segDeltaPath(path)} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	return all
}
