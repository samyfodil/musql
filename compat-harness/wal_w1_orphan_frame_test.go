// This file tests orphaned WAL frame detection.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// w1NumTables is how many distinct tables (== distinct pages) the aborted
// transaction touches. More than musql's injected-failure threshold (see
// below) and more than SQLite's cache_size=2, so at least one is left as a
// genuine on-disk orphan for both engines.
const w1NumTables = 8

func w1Stmt(t *testing.T, db *sql.DB, sqlText string, args ...any) {
	t.Helper()
	if _, err := db.Exec(sqlText, args...); err != nil {
		t.Fatalf("Exec(%q): %v", sqlText, err)
	}
}

func w1BuildBaseline(t *testing.T, db *sql.DB) {
	t.Helper()
	w1Stmt(t, db, "PRAGMA journal_mode=WAL")
	w1Stmt(t, db, "PRAGMA wal_autocheckpoint=0") // nothing must fold the WAL back mid-test
	for i := 0; i < w1NumTables; i++ {
		w1Stmt(t, db, fmt.Sprintf("CREATE TABLE t%d(v TEXT)", i))
		w1Stmt(t, db, fmt.Sprintf("INSERT INTO t%d VALUES('orig-%d')", i, i))
	}
}

// w1ReadAll returns table i's single value, for every table, in one string
// (order-stable) for easy comparison/logging.
func w1ReadAll(t *testing.T, db *sql.DB) []string {
	t.Helper()
	got := make([]string, w1NumTables)
	for i := 0; i < w1NumTables; i++ {
		var v string
		if err := db.QueryRow(fmt.Sprintf("SELECT v FROM t%d", i)).Scan(&v); err != nil {
			t.Fatalf("SELECT v FROM t%d: %v", i, err)
		}
		got[i] = v
	}
	return got
}

// w1AssertAllOriginal fails if any table has drifted from its original value.
func w1AssertAllOriginal(t *testing.T, phase string, got []string) {
	t.Helper()
	for i, v := range got {
		want := fmt.Sprintf("orig-%d", i)
		if v != want {
			t.Fatalf("%s: t%d.v = %q, want %q (unchanged baseline)", phase, i, v, want)
		}
	}
}

// TestW1OrphanFrameNotResurrectedByNextCommit is the repro itself, run
// against both engines. It is written to FAIL on musql before the W1 fix
// and PASS on both engines after it (see the outcome recorded in
// musql-stage5-design-and-w1-candidate.md's own companion note for the
// actual run this produced).
func TestW1OrphanFrameNotResurrectedByNextCommit(t *testing.T) {
	t.Run("cgo", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "w1.db")
		db, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatalf("sql.Open(sqlite3): %v", err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		w1BuildBaseline(t, db)

		// Construct the orphan on a SECOND connection to the same file,
		// exactly as wal_read_test.go's proven "uncommitted trailing
		// frame" scenario does: cache_size=2 forces a spill of dirty pages
		// into the WAL well before COMMIT, then ROLLBACK leaves those
		// spilled, valid, checksummed frames physically on disk.
		second, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatalf("sql.Open(sqlite3) second: %v", err)
		}
		second.SetMaxOpenConns(1)
		w1Stmt(t, second, "PRAGMA cache_size=2")
		w1Stmt(t, second, "BEGIN")
		for i := 0; i < w1NumTables; i++ {
			w1Stmt(t, second, fmt.Sprintf("UPDATE t%d SET v='ORPHAN-%d'", i, i))
		}
		// Do NOT commit: abandon the transaction exactly as a crashed or
		// errored writer would. ROLLBACK (rather than just closing the
		// connection) is the proven, documented mechanism -- C SQLite's
		// sqlite3WalUndo restores the in-memory header but never rewrites
		// the WAL file's bytes, so whatever spilled stays physically in
		// place (see this file's own package doc comment).
		w1Stmt(t, second, "ROLLBACK")
		if err := second.Close(); err != nil {
			t.Fatalf("second.Close: %v", err)
		}

		before := w1ReadAll(t, db)
		w1AssertAllOriginal(t, "cgo: before the follow-up commit", before)

		// One further, ordinary, UNRELATED commit: a brand-new table,
		// touching none of t0..t7's pages.
		w1Stmt(t, db, "CREATE TABLE tfinal(v TEXT)")
		w1Stmt(t, db, "INSERT INTO tfinal VALUES('final')")

		after := w1ReadAll(t, db)
		w1AssertAllOriginal(t, "cgo: after the follow-up commit", after)
		t.Logf("cgo: before=%v after=%v (orphan correctly never resurfaced)", before, after)
	})

	// musql's half was the SQLite-format WAL writer, which the engine no longer
	// executes through (AGENTS.md Rule 3). The same hazard on the format it does
	// write -- an orphaned delta batch and the next commit -- is
	// engine/segment_delta_orphan_test.go.
}
