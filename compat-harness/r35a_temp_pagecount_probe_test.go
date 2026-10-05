// Tests PRAGMA temp.page_count and temp.freelist_count, which report the
// temp database's own page count. These pragmas return 0 for a temp database
// that has never held any object.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestR35ATempPageCountOracle asserts where the "still empty" state ENDS. The
// engine serves only the 0/0 rows; every other line here is what it declines,
// and is asserted so that a change in the oracle is reported as such rather
// than as a confusing engine failure.
func TestR35ATempPageCountOracle(t *testing.T) {
	cases := []struct {
		name           string
		stmts          []string
		pages, freelen int
	}{
		{"fresh", nil, 0, 0},
		{"after main table+rows", []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1),(2)`}, 0, 0},
		{"twice", []string{`PRAGMA temp.page_count`}, 0, 0},
		{"after sqlite_temp_master read", []string{`SELECT * FROM sqlite_temp_master`}, 0, 0},
		{"after integrity_check", []string{`CREATE TABLE t(a)`, `PRAGMA integrity_check`}, 0, 0},
		{"after order by", []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(2),(1)`, `SELECT * FROM t ORDER BY a`}, 0, 0},
		{"in a transaction", []string{`CREATE TABLE t(a)`, `BEGIN`, `INSERT INTO t VALUES(1)`}, 0, 0},
		// ...and the first temp object writes into it, permanently.
		{"after temp view", []string{`CREATE TABLE t(a)`, `CREATE TEMP VIEW v AS SELECT 1`}, 1, 0},
		{"after temp table", []string{`CREATE TEMP TABLE tt(a)`}, 2, 0},
		{"after temp table + row", []string{`CREATE TEMP TABLE tt(a)`, `INSERT INTO tt VALUES(1)`}, 2, 0},
		{"after temp table dropped", []string{`CREATE TEMP TABLE tt(a)`, `DROP TABLE tt`}, 2, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "x.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			for _, s := range tc.stmts {
				if _, eerr := db.Exec(s); eerr != nil {
					t.Logf("  (stmt %q: %v)", s, eerr)
				}
			}
			var n, fl int
			if qerr := db.QueryRow(`PRAGMA temp.page_count`).Scan(&n); qerr != nil {
				t.Fatalf("temp.page_count: %v", qerr)
			}
			if qerr := db.QueryRow(`PRAGMA temp.freelist_count`).Scan(&fl); qerr != nil {
				t.Fatalf("temp.freelist_count: %v", qerr)
			}
			if n != tc.pages || fl != tc.freelen {
				t.Errorf("temp.page_count=%d freelist=%d, want %d/%d -- r35aEmptyTempPragmaResult (engine/temp_schema.go) answers 0/0 for exactly the never-held-an-object state and must be re-derived if this moved", n, fl, tc.pages, tc.freelen)
			}
		})
	}
}

// TestR35ATempPageCountParity is the differential gate: the states the engine
// serves must AGREE with the oracle, column name and value.
func TestR35ATempPageCountParity(t *testing.T) {
	// main's own page_count is deliberately NOT verified here: engine.Create
	// writes page 1 immediately where "sqlite3 db test.db" leaves a 0-byte
	// file, so the two disagree on an EMPTY database for a reason that has
	// nothing to do with the temp one (pragma.test 14.1's own {0 0}).
	verify := []string{`PRAGMA temp.page_count`, `PRAGMA temp.freelist_count`}
	cases := []struct {
		name   string
		script []string
	}{
		{"nothing but main", []string{`CREATE TABLE abc(a, b, c)`}},
		{"pragma.test 14.2's schema", []string{`CREATE TABLE abc(a, b, c)`, `INSERT INTO abc VALUES(1,2,3)`}},
		{"a temp catalog read is not a write", []string{`CREATE TABLE abc(a)`, `PRAGMA temp.locking_mode`}},
		{"a sorter is not the temp database", []string{
			`CREATE TABLE t(a)`, `INSERT INTO t VALUES(2),(1)`}},
		{"after integrity_check", []string{`CREATE TABLE t(a)`, `PRAGMA integrity_check`}},
		{"after a temp_store change discarded the temp database", []string{
			`CREATE TEMP TABLE tt(a)`, `PRAGMA temp_store=2`}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			flLockstep(t, tc.name, tc.script, verify...)
		})
	}
}

// TestR35ATempPageCountAfterTempObject is the other side of the boundary: once
// the temp database has held an object its size is this engine's one file
// showing through, so the pragma is declined. This asserts that if it is ever
// ANSWERED, the answer must be the oracle's -- 2 pages for a temp table, and
// still 2 with one on the freelist after a DROP.
func TestR35ATempPageCountAfterTempObject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script []string
	}{
		{"a temp table", []string{`CREATE TEMP TABLE tt(a)`}},
		{"a temp table since dropped", []string{`CREATE TEMP TABLE tt(a)`, `DROP TABLE tt`}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			edb, cdb := r35aTwoEngines(t)
			for i, s := range tc.script {
				_, cerr := cdb.Exec(s)
				eerr := edb.Exec(s)
				if (eerr == nil) != (cerr == nil) {
					t.Fatalf("stmt #%d %s accept/reject disagrees\n  engine: %v\n  cgo: %v", i, s, eerr, cerr)
				}
			}
			p, perr := edb.SnapshotPager()
			if perr != nil {
				t.Fatalf("SnapshotPager: %v", perr)
			}
			defer p.Close()
			if _, _, qerr := p.QueryArgs(`PRAGMA temp.page_count`, nil); qerr != nil {
				return // the honest decline
			}
			flCompareQuery(t, tc.name, edb, cdb, `PRAGMA temp.page_count`)
			flCompareQuery(t, tc.name, edb, cdb, `PRAGMA temp.freelist_count`)
		})
	}
}

// TestR35ATempPageCountExecAcceptsNonEmpty gates the temp-catalog-own-header
// fix: past the empty-database boundary TestR35ATempPageCountAfterTempObject
// covers, "PRAGMA temp.page_count"/"freelist_count" used to decline
// UNCONDITIONALLY on EVERY path. They are pure GETTERS on the Exec path,
// though: Exec discards the row regardless of catalog (see execPragma's own
// "page_count","freelist_count" case, pragma.go, which does exactly that for
// the unqualified/main form already), so there is no VALUE this statement
// could hand back wrong there -- only whether it is ACCEPTED. C SQLite's
// own getter never rejects for any database state (pragma.c:663
// PragTyp_PAGE_COUNT unconditionally emits OP_Pagecount; pragma.c:2324
// PragTyp_HEADER_VALUE, which freelist_count maps to per its own doc comment
// at pragma.c:2299, unconditionally emits OP_ReadCookie for a read-only
// cookie), so this engine's Exec path now accepts unconditionally too --
// execPragma's own comment on this case has the full reasoning, including why
// the QUERY path (queryPragmaStmt) is NOT this and keeps its own separate
// decline (still covered, unchanged, by TestR35ATempPageCountAfterTempObject
// and TestTempFileScopedPragmasStillDecline in window_cte_temp_pragma_test.go).
//
// The two scripts below are the exact shapes the TCL corpus mines as
// temptable2.test#3 (statement 6 of segment 3: temptable2.test's own 4.1.1,
// ~/.cache/musql/sqlite-353/test/temptable2.test lines 91-103 -- a 10-row
// temp table plus an index on it, small enough that neither b-tree ever
// splits) and vacuum5.test#0 (statement 25 of segment 0: vacuum5.test's own
// ttemp, ~/.cache/musql/sqlite-353/test/vacuum5.test lines 33-34/107-109 -- a
// 1000-row plain temp table, which DOES split repeatedly). Both are mined as
// bare "PRAGMA temp.page_count;" statements with no expected-result string the
// miner can use (do_execsql_test's literal "{10 9}"/"$sizeTemp" are never
// compared -- see tcl_test.go's own package doc comment: this harness
// differentially replays against the LIVE oracle, not the TCL source's own
// hardcoded numbers, which in temptable2.test's case were generated under
// testfixture's -DSQLITE_DEFAULT_PAGE_SIZE=1024, not the 4096 both this
// harness's oracle and this engine actually run at), and PRAGMA is never
// classified as a query by the miner (tclIsQuery only recognizes
// SELECT/VALUES/WITH), so both are graded purely on Exec accept/reject --
// exactly what this test replays directly.
//
// One substitution from the original TCL text: randomblob(N) in place of
// zeroblob(N) below. Both scripts' real source sources their row content
// through a WITH-attached "INSERT ... SELECT randomblob(...)", which this
// engine deliberately declines on its OWN, unrelated grounds
// (insert_write.go's insertFromSelect: content from a nondeterministic
// function can never match an independently-seeded oracle) -- correctly
// excluded from the corpus tally as tclIsOutOfScope agreement, not counted
// against either statement here. zeroblob(N) is the same byte SIZE with
// deterministic content, so it reaches an identical page layout without
// tripping that orthogonal, already-known decline. The intervening "SELECT
// count(*) FROM t1" the real script also runs is dropped: it is a QUERY, not
// an Exec statement, and edb.Exec (like the corpus harness's own
// tclSafeExecArgs) is the write-path dispatch only -- the mined corpus
// replays it through SnapshotPager+QueryArgs, a separate call this narrow
// probe has no need to reproduce.
func TestR35ATempPageCountExecAcceptsNonEmpty(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script []string
	}{
		{"temptable2.test 4.1.1: 10-row temp table + index", []string{
			`PRAGMA main.cache_size = 10`,
			`PRAGMA temp.cache_size = 10`,
			`CREATE TEMP TABLE t1(a, b)`,
			`CREATE INDEX i1 ON t1(a, b)`,
			`WITH x(i) AS ( SELECT 1 UNION ALL SELECT i+1 FROM x WHERE i<10 ) INSERT INTO t1 SELECT zeroblob(100), zeroblob(100) FROM x`,
		}},
		{"vacuum5.test ttemp: 1000-row plain temp table (splits repeatedly)", []string{
			`CREATE TABLE main.t1(a,b)`,
			`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<1000) INSERT INTO t1(a,b) SELECT x, zeroblob(1000) FROM c`,
			`CREATE TEMP TABLE ttemp(x,y)`,
			`INSERT INTO ttemp SELECT * FROM t1`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			edb, cdb := r35aTwoEngines(t)
			for i, s := range tc.script {
				_, cerr := cdb.Exec(s)
				eerr := edb.Exec(s)
				if (eerr == nil) != (cerr == nil) {
					t.Fatalf("setup stmt #%d %s accept/reject disagrees\n  engine: %v\n  cgo: %v", i, s, eerr, cerr)
				}
			}
			for _, q := range []string{`PRAGMA temp.page_count`, `PRAGMA temp.freelist_count`} {
				_, cerr := cdb.Exec(q)
				eerr := edb.Exec(q)
				if cerr != nil {
					t.Fatalf("%s: oracle rejected it (%v) -- the premise of this test (a getter that never errors) is gone", q, cerr)
				}
				if eerr != nil {
					t.Errorf("%s: engine declined, want acceptance (Exec discards the row -- see execPragma's own comment on this case): %v", q, eerr)
				}
			}
		})
	}
}

// r35aTwoEngines opens one fresh engine.DB and one fresh cgo connection, each
// on its own file -- flLockstep's setup, split out for the tests here that
// have to drive the two sides themselves.
func r35aTwoEngines(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, cerr := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if cerr != nil {
		t.Fatalf("sql.Open: %v", cerr)
	}
	cdb.SetMaxOpenConns(1)
	t.Cleanup(func() { cdb.Close() })
	return edb, cdb
}
