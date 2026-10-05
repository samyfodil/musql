// This file gates locking_mode on WAL databases in exclusive mode,
// comparing connection state against pager state.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// pragmaR24WalCase is one test program.
type pragmaR24WalCase struct {
	name  string
	stmts []string
	// declined records when musql cannot classify a statement the oracle answers.
	declined bool
}

var pragmaR24WalCases = []pragmaR24WalCase{
	// ---- the five mined corpus segments, prefix for prefix ----

	// wal2.test#7: the WAL is opened by a top-level SELECT *before* exclusive
	// mode is entered, so its wal-index is a real -shm file and C SQLite
	// leaves the mode freely. SERVED since round 25: a query's text reaches the
	// write session at ReadOnlyPager.QueryArgs, which resolves the provisional
	// unknown SnapshotPager deposits -- and it does so on the TEXT, so this case
	// works even though musql then DECLINES this particular SELECT.
	{"wal2-7-select-opens-the-wal-first", []string{
		`PRAGMA journal_mode = wal`,
		`SELECT * FROM sqlite_master`,
		`PRAGMA locking_mode = exclusive`,
		`BEGIN`,
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`COMMIT`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// wal2.test#11: the WAL is opened by CREATE TABLE *under* exclusive, so
	// the wal-index is heap memory and the pager REFUSES -- main stays
	// exclusive while the connection default goes normal.
	{"wal2-11-wal-opened-under-exclusive", []string{
		`PRAGMA auto_vacuum = 0`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`CREATE TABLE t2(a, b)`,
		`INSERT INTO t2 VALUES('I', 'II')`,
		`PRAGMA journal_mode`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// exclusive.test#4: exclusive FIRST, then WAL, and nothing in between ever
	// opens the WAL -- so there is no wal-index at all and the leave is free.
	{"exclusive-4-wal-never-opened", []string{
		`PRAGMA locking_mode = EXCLUSIVE`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA locking_mode = NORMAL`,
	}, false},

	// walnoshm.test#0: like wal2.test#7, opened by a top-level SELECT first --
	// and the second "PRAGMA journal_mode = WAL" in it must NOT reset the state,
	// since an assignment that would not change the mode never opens a new Wal.
	{"walnoshm-0-select-opens-the-wal-first", []string{
		`CREATE TABLE t1(x, y)`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`PRAGMA journal_mode = WAL`,
		`SELECT * FROM t1`,
		`PRAGMA locking_mode = exclusive`,
		`PRAGMA journal_mode = WAL`,
		`INSERT INTO t1 VALUES(3, 4)`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// walfault.test#16: opened by a write statement before exclusive.
	{"walfault-16-write-opens-the-wal-first", []string{
		`PRAGMA auto_vacuum = 0`,
		`PRAGMA journal_mode = WAL`,
		`BEGIN`,
		`CREATE TABLE abc(a PRIMARY KEY)`,
		`INSERT INTO abc VALUES(1)`,
		`COMMIT`,
		`PRAGMA locking_mode = exclusive`,
		`BEGIN`,
		`INSERT INTO abc VALUES(2)`,
		`COMMIT`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// ---- the classifier's own boundary, both sides of it ----

	// Nothing at all between the two setters: still not opened.
	{"nothing-between", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// A measured NO statement between them leaves it unopened...
	{"a-no-statement-between", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`PRAGMA synchronous`,
		`PRAGMA database_list`,
		`PRAGMA journal_mode`,
		`BEGIN`,
		`COMMIT`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// ...and a measured YES one opens it, in heap memory.
	{"a-yes-statement-between", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`PRAGMA user_version`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// ---- the QUERY classifier's own boundary (round 25) ----

	// A query that reads a real table under exclusive mode opens the Wal in heap
	// memory, exactly like a write does: the leave is refused and the two values
	// disagree.
	{"a-query-under-exclusive-opens-it-in-heap", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`SELECT * FROM t`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// ...and the same query BEFORE exclusive mode gives it a real -shm file, so
	// the leave is free for that Wal's whole life.
	{"a-query-before-exclusive-opens-it-in-shm", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`SELECT * FROM t`,
		`PRAGMA locking_mode = exclusive`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// A query the classifier does not know is UNKNOWN, and hardens the state
	// rather than being guessed either way -- a deliberate over-refusal.
	// "SELECT * FROM (SELECT 1)" is measured NO against the oracle.
	{"an-unclassifiable-query-hardens-it", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`SELECT * FROM (SELECT 1)`,
		`PRAGMA locking_mode = normal`,
	}, true},

	// A CTE may SHADOW a real table, so a query with one is never classified
	// from its FROM item's name -- also an over-refusal (the oracle answers
	// "normal": nothing here reads the real t).
	{"a-cte-shadowing-a-table-is-not-classified", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`WITH t(a) AS (VALUES(1)) SELECT * FROM t`,
		`PRAGMA locking_mode = normal`,
	}, true},

	// A WRONG ANSWER round 24 shipped, found in round 25 and fixed there. The
	// statement that creates the session's FIRST temp object is itself a
	// TEMP-database statement -- measured NO, like every other one -- but musql
	// classifies a statement before running it, and before this one runs no temp
	// object exists, so it looked like an ordinary CREATE and was called YES.
	// That reported the WAL as opened in heap memory and answered "exclusive"
	// here, where the oracle answers "normal" -- which is what this case now
	// asserts the oracle still does. The fix (mainReadTxnAfter) turns the wrong
	// answer into an over-refusal rather than into the right one: telling
	// "CREATE TEMP TABLE tt" from "CREATE TABLE t2" after the fact would mean
	// re-resolving the target, the same line the before-the-fact check draws.
	{"create-temp-table-under-exclusive-does-not-open-it", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`CREATE TEMP TABLE tt(a)`,
		`PRAGMA locking_mode = normal`,
	}, true},

	// The documented escape hatch: leaving WAL mode CLOSES the Wal, which
	// clears the heap-memory flag with it.
	{"leaving-wal-mode-first-unsticks-it", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`PRAGMA user_version`,
		`PRAGMA journal_mode = delete`,
		`PRAGMA locking_mode = normal`,
	}, false},

	// Re-entering WAL mode starts a NEW Wal, unopened again.
	{"re-entering-wal-mode-resets-it", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`PRAGMA user_version`,
		`PRAGMA journal_mode = delete`,
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = normal`,
	}, false},
}

// pragmaR24OracleLockingMode replays stmts against C SQLite on one
// connection and returns (bare getter, main. getter) afterwards.
func pragmaR24OracleLockingMode(t *testing.T, stmts []string) (dflt, main string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one logical connection: locking_mode is per-connection
	for i, s := range stmts {
		rows, qerr := db.Query(s)
		if qerr != nil {
			t.Fatalf("oracle stmt #%d %q: %v", i, s, qerr)
		}
		for rows.Next() {
		}
		if rerr := rows.Err(); rerr != nil {
			rows.Close()
			t.Fatalf("oracle stmt #%d %q: %v", i, s, rerr)
		}
		rows.Close()
	}
	one := func(q string) string {
		var v string
		if err := db.QueryRow(q).Scan(&v); err != nil {
			t.Fatalf("oracle %q: %v", q, err)
		}
		return v
	}
	return one(`PRAGMA locking_mode`), one(`PRAGMA main.locking_mode`)
}

// pragmaR24EngineLockingMode replays stmts ENGINE-DIRECT and returns the same
// two values, plus whether the LAST statement was declined. The getters are
// read through a read snapshot, which is where SnapshotPager deposits the two
// values the write session holds -- and it is done only at the END, because a
// top-level snapshot is itself what makes the wal-index state UNKNOWN.
func pragmaR24EngineLockingMode(t *testing.T, stmts []string) (dflt, main string, declined bool) {
	t.Helper()
	db, err := engine.Create(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Discard()
	isQuery := func(s string) bool {
		up := strings.ToUpper(strings.TrimSpace(s))
		return strings.HasPrefix(up, "SELECT") || strings.HasPrefix(up, "WITH") ||
			strings.HasPrefix(up, "VALUES")
	}
	for i, s := range stmts {
		var eerr error
		if isQuery(s) {
			// Routed exactly as TestTCLCorpus routes a query -- SnapshotPager
			// then QueryArgs -- which is the very path that carries no
			// statement text into the write session.
			var qp *engine.ReadOnlyPager
			if qp, eerr = db.SnapshotPager(); eerr == nil {
				_, _, eerr = qp.QueryArgs(s, nil)
				qp.Close()
			}
		} else {
			_, _, eerr = db.ExecArgs(s, nil)
		}
		if eerr != nil {
			if i == len(stmts)-1 {
				return "", "", true
			}
			if isQuery(s) {
				// A query this engine declines for its OWN reasons (wal2.test#7's
				// "SELECT * FROM sqlite_master" is refused over the rootpage
				// column) still went through SnapshotPager, which is the whole
				// point here: the top-level snapshot is what makes the wal-index
				// state unknown, whether or not the query then succeeds. Skip it
				// exactly as TestTCLCorpus does.
				continue
			}
			t.Fatalf("engine stmt #%d %q: %v", i, s, eerr)
		}
	}
	p, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	defer p.Close()
	one := func(q string) string {
		_, rows, qerr := p.QueryArgs(q, nil)
		if qerr != nil {
			t.Fatalf("engine %q: %v", q, qerr)
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Fatalf("engine %q: want one row of one column, got %v", q, rows)
		}
		return string(rows[0][0].S)
	}
	return one(`PRAGMA locking_mode`), one(`PRAGMA main.locking_mode`), false
}

// TestPragmaR24DeferFKBoundaryIsNotTheWalBoundary records, with the oracle
// answering live, why "PRAGMA defer_foreign_keys=ON" OUTSIDE a transaction is
// still declined even though the wal-index state above now tracks a boundary
// that looks like the same one.
//
// It is not the same one. The WAL opens at a real OP_Transaction on main;
// SQLITE_DeferFKs is cleared by sqlite3VdbeHalt on the autocommit COMMIT path,
// which needs only p->bIsReader. "PRAGMA journal_mode" satisfies the second and
// not the first, so it leaves the mode leavable AND clears the flag -- and an
// implementation that reused mainReadTxnOf's table for both would keep the flag
// where C SQLite drops it, which is a wrong answer rather than a gap.
// (Caught exactly that way: the first attempt at this reused the table, and the
// case below is what failed.)
//
// Whoever serves defer_foreign_keys next needs its OWN measured table over
// p->bIsReader. This test fails the moment the oracle stops agreeing with the
// premise, which is the only thing that would make that work unnecessary.
func TestPragmaR24DeferFKBoundaryIsNotTheWalBoundary(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
		`CREATE TABLE t3(u INTEGER PRIMARY KEY, v INTEGER REFERENCES t1(x))`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`INSERT INTO t3 VALUES(3,3)`,
		`PRAGMA defer_foreign_keys=ON`,
		`PRAGMA journal_mode`, // a mainReadTxnOf "NO" statement...
		`BEGIN`,
	} {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%q: %v", s, eerr)
		}
	}
	// ...and yet the flag is already gone, so the immediate foreign key bites
	// on the spot rather than at the COMMIT.
	if _, eerr := db.Exec(`DELETE FROM t1 WHERE x=3`); eerr == nil {
		t.Fatalf("the oracle no longer clears SQLITE_DeferFKs on a statement that does not open the WAL -- the two boundaries may have converged; re-measure before relying on this")
	}
	db.Exec(`ROLLBACK`)
}

func TestPragmaR24WalExclusiveLeaveMatchesCSQLite(t *testing.T) {
	for _, tc := range pragmaR24WalCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			wantDflt, wantMain := pragmaR24OracleLockingMode(t, tc.stmts)
			gotDflt, gotMain, declined := pragmaR24EngineLockingMode(t, tc.stmts)
			if declined != tc.declined {
				t.Fatalf("musql declined=%v, expected declined=%v (oracle: locking_mode=%s main.locking_mode=%s)",
					declined, tc.declined, wantDflt, wantMain)
			}
			if declined {
				return
			}
			if gotDflt != wantDflt || gotMain != wantMain {
				t.Errorf("DIVERGES after %v\n  cgo:    locking_mode=%s main.locking_mode=%s\n  musql: locking_mode=%s main.locking_mode=%s",
					tc.stmts, wantDflt, wantMain, gotDflt, gotMain)
			}
		})
	}
}
