// Tests WAL mode combined with exclusive locking mode.
package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// walExclusiveSequences are run step by step against BOTH engines, with every
// probe below compared cell for cell after each step. A step marked decline
// must be refused by this engine and is then NOT replayed against the oracle,
// which is what keeps the two sides in the same mode -- the probes after it
// prove that they are.
//
// The probes themselves are part of the setup, not just the check: reading
// "SELECT count(*) FROM t" after every step OPENS the WAL. So a sequence that
// enters WAL mode BEFORE going exclusive has its wal-index in shared memory by
// the time the setter runs, which leaves the oracle unpinned there; only
// exclusive-first sequences reach the state the decline exists for. Both shapes
// are kept, and the difference is called out step by step, because the decline
// covers both -- this engine cannot tell them apart, which is the whole point.
var walExclusiveSequences = map[string][]lockingModeStep{
	"wal-then-exclusive": {
		{sql: `PRAGMA journal_mode = wal`},
		{sql: `PRAGMA locking_mode = exclusive`},
		{sql: `PRAGMA locking_mode = EXCLUSIVE`}, // re-entering changes nothing
		{sql: `PRAGMA locking_mode = xyz`},       // unrecognized: a pure query
		{sql: `INSERT INTO t VALUES(1)`},
		{sql: `INSERT INTO t VALUES(2)`},
		// CONSERVATIVE here: the WAL was opened by the read probe after step 0,
		// while the connection was still normal, so the oracle's wal-index is in
		// shared memory and it would let go. This engine has no way to know that
		// and declines anyway -- a gap, never a wrong answer.
		{sql: `PRAGMA locking_mode = normal`, why: "served since round 25: walIndexKind knows the WAL was opened BEFORE exclusive here, so C SQLite lets go"},
		{sql: `PRAGMA main.locking_mode = normal`, why: "same bit, qualified"},
		{sql: `PRAGMA locking_mode = exclusive`},
	},
	"exclusive-then-wal": {
		{sql: `PRAGMA locking_mode = exclusive`},
		{sql: `PRAGMA journal_mode = wal`},
		{sql: `INSERT INTO t VALUES(1)`},
		// NECESSARY here: exclusive came first, so the WAL was opened under it
		// and the oracle's wal-index is in heap memory. Serving this answers
		// "normal" where C SQLite answers "exclusive" -- verified by removing
		// the decline, which makes the probe below mismatch on main.locking_mode.
		{sql: `PRAGMA locking_mode = normal`, why: "the oracle is pinned here (WAL opened under exclusive); the setter still answers exclusive and the stored default still moves"},
	},
	// Leaving WAL mode CLOSES the Wal, which clears the heap-memory flag -- so
	// from there the setter is knowable again and is served. All five spellings
	// of "leave WAL" were verified to behave this way; delete is the one this
	// engine implements as a real checkpoint-and-drop.
	"leaving-wal-makes-it-leavable-again": {
		{sql: `PRAGMA journal_mode = wal`},
		{sql: `PRAGMA locking_mode = exclusive`},
		{sql: `INSERT INTO t VALUES(1)`},
		{sql: `PRAGMA journal_mode = delete`},
		{sql: `PRAGMA locking_mode = normal`},
		{sql: `PRAGMA locking_mode`},
		{sql: `INSERT INTO t VALUES(2)`},
	},
	// A qualified enter does not move the connection default, exactly as in the
	// non-WAL case -- so the bare getter and "main." disagree here too.
	"qualified-enter-on-wal": {
		{sql: `PRAGMA journal_mode = wal`},
		{sql: `PRAGMA main.locking_mode = exclusive`},
		{sql: `INSERT INTO t VALUES(1)`},
		{sql: `PRAGMA main.locking_mode = normal`, why: "still the same bit (WAL-first)"},
	},
	// Exclusive first and a qualified enter: this reaches the pinned state the
	// way exclusive-then-wal does, but through "main.".
	"qualified-exclusive-then-wal": {
		{sql: `PRAGMA main.locking_mode = exclusive`},
		{sql: `PRAGMA journal_mode = wal`},
		{sql: `INSERT INTO t VALUES(1)`},
		{sql: `PRAGMA main.locking_mode = normal`, why: "the oracle is pinned here too; the setter reports the pager mode either way"},
	},
}

// walExclusiveProbes are read back after every step: the three locking_mode
// forms (the bare one reports the connection DEFAULT, the qualified ones each
// database's own mode) plus journal_mode, since this pragma pair is exactly
// about the two interacting.
var walExclusiveProbes = []string{
	`PRAGMA locking_mode`,
	`PRAGMA main.locking_mode`,
	`PRAGMA temp.locking_mode`,
	`PRAGMA journal_mode`,
	`PRAGMA main.journal_mode`,
}

func TestWalExclusiveLockingMatchesCSQLite(t *testing.T) {
	for name, steps := range walExclusiveSequences {
		name, steps := name, steps
		t.Run(name, func(t *testing.T) {
			godb, cgodb := lockingModePair(t)
			defer godb.Discard()
			defer cgodb.Close()

			for i, step := range steps {
				_, _, goErr := godb.ExecArgs(step.sql, nil)
				if step.decline && goErr != nil {
					// Declined as designed. A declined statement is deliberately
					// NOT replayed against the oracle -- that is what keeps the
					// two sides in the same mode, which the probes below confirm.
					checkProbes(t, godb, cgodb, i, step.sql)
					continue
				}
				if goErr != nil {
					t.Fatalf("step %d %q: this engine declined: %v", i, step.sql, goErr)
				}
				// Accepted -- including a step that was supposed to be declined.
				// That case is deliberately NOT short-circuited: replaying it on
				// the oracle and letting the probes speak turns "the policy
				// changed" into the actual wrong ANSWER the policy prevents,
				// which is the only thing worth failing on. (Measured: with the
				// decline removed, the mined-TCL corpus scores 5 passes MORE and
				// still reports wrong=0 -- it cannot see this at all.)
				if _, cerr := cgodb.Exec(step.sql); cerr != nil {
					t.Fatalf("step %d %q: C SQLite rejected a statement this engine accepted: %v", i, step.sql, cerr)
				}
				if step.decline {
					t.Logf("step %d %q was expected to be DECLINED (%s) and was accepted; what follows is what that costs", i, step.sql, step.why)
				}
				checkProbes(t, godb, cgodb, i, step.sql)
				if step.decline {
					t.Fatalf("step %d %q: expected this engine to DECLINE (%s); it accepted, and every probe still agreed -- either the oracle's rule changed (see TestWalExclusiveOracleReallyPinsTheMode) or this sequence no longer reaches the ambiguous state", i, step.sql, step.why)
				}
			}
		})
	}
}

// TestWalExclusiveOracleReallyPinsTheMode keeps the oracle's OWN behaviour --
// the fact the decline exists for -- live in an assertion rather than in a
// comment. If a future SQLite lets an exclusive WAL connection back out, or
// starts refusing before the WAL is opened, this fails and the decline should
// be revisited; nothing else in the suite would notice.
//
// It also pins the -shm claim, which is the whole reason serving the ENTRY is
// honest here: C SQLite writes no wal-index file in this state, and neither
// does this engine (it keeps no wal-index at all -- engine/wal_write.go).
func TestWalExclusiveOracleReallyPinsTheMode(t *testing.T) {
	openSeeded := func(t *testing.T) (*sql.DB, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "a.db")
		db, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		db.SetMaxOpenConns(1) // locking_mode is CONNECTION state
		t.Cleanup(func() { db.Close() })
		if _, err := db.Exec(`CREATE TABLE t(x)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		return db, path
	}

	t.Run("untouched WAL is still leavable", func(t *testing.T) {
		db, path := openSeeded(t)
		mustCGOPragma(t, db, `PRAGMA journal_mode=wal`, "wal")
		mustCGOPragma(t, db, `PRAGMA locking_mode=exclusive`, "exclusive")
		// No statement has opened the WAL, so C SQLite really does let go.
		mustCGOPragma(t, db, `PRAGMA locking_mode=normal`, "normal")
		mustCGOPragma(t, db, `PRAGMA main.locking_mode`, "normal")
		if got := sideFiles(t, path); got != "[a.db]" {
			t.Fatalf("no WAL should have been created yet, directory holds %s", got)
		}
	})

	t.Run("one statement pins it, and writes no -shm", func(t *testing.T) {
		db, path := openSeeded(t)
		mustCGOPragma(t, db, `PRAGMA journal_mode=wal`, "wal")
		mustCGOPragma(t, db, `PRAGMA locking_mode=exclusive`, "exclusive")
		if _, err := db.Exec(`INSERT INTO t VALUES(1)`); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		// THE refusal: the setter answers the mode it could not leave.
		mustCGOPragma(t, db, `PRAGMA locking_mode=normal`, "exclusive")
		mustCGOPragma(t, db, `PRAGMA main.locking_mode`, "exclusive")
		// ...while the bare getter reports the connection DEFAULT, which the
		// same statement DID move. Those two answers differ, which is why this
		// engine cannot fake the pair by tracking one string.
		mustCGOPragma(t, db, `PRAGMA locking_mode`, "normal")
		if got := sideFiles(t, path); got != "[a.db a.db-wal]" {
			t.Fatalf("an exclusive WAL keeps its wal-index in heap memory: directory should hold only the database and its -wal, holds %s", got)
		}
		// ...and leaving WAL mode closes the Wal, which unpins it again.
		mustCGOPragma(t, db, `PRAGMA journal_mode=delete`, "delete")
		mustCGOPragma(t, db, `PRAGMA locking_mode=normal`, "normal")
	})

	t.Run("a WAL opened in NORMAL mode is never pinned", func(t *testing.T) {
		// The control that makes the rule "opened while exclusive" rather than
		// "in WAL mode at all": open the WAL first, THEN go exclusive, and the
		// connection can still leave. This one writes a -shm, and does so
		// whichever mode it is in afterwards.
		db, path := openSeeded(t)
		mustCGOPragma(t, db, `PRAGMA journal_mode=wal`, "wal")
		if _, err := db.Exec(`INSERT INTO t VALUES(1)`); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		mustCGOPragma(t, db, `PRAGMA locking_mode=exclusive`, "exclusive")
		mustCGOPragma(t, db, `PRAGMA locking_mode=normal`, "normal")
		if got := sideFiles(t, path); got != "[a.db a.db-shm a.db-wal]" {
			t.Fatalf("a WAL opened in normal mode has an on-disk wal-index, directory holds %s", got)
		}
	})
}

// TestWalExclusiveHoldsTheLockAndTheRows is the honesty half: it is what
// separates "the engine entered exclusive locking mode on a WAL database" from
// "the engine remembered a string". Reporting the mode without holding the lock
// is the failure this catches, and it is the failure a no-op accept would be.
//
// Two claims:
//
//  1. THE LOCK. While the engine's session is in WAL + exclusive, another live
//     connection's write is refused ("database is locked"), and gets in again
//     once that session is gone. It is a musql connection: C cannot open this
//     engine's file (AGENTS.md Rule 3), and the lock is met the same way. (C SQLite in this state shuts the
//     second connection's READS out too, once it has written; this engine holds
//     the reader-friendly rung, which is a weaker lock and never a wrong
//     answer -- the same ceiling engine/lock.go already documents for the
//     non-WAL case.)
//  2. THE ROWS. Everything written under WAL + exclusive is C SQLite's data
//     afterwards, cell for cell, with the file still in WAL mode -- read
//     through the export.
func TestWalExclusiveHoldsTheLockAndTheRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.db")
	seed, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	if _, _, err := seed.ExecArgs(`CREATE TABLE t(x, y)`, nil); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	// The OTHER connection is a musql one -- C cannot open this engine's file
	// (AGENTS.md Rule 3) -- and C judges the rows afterwards through the export.
	orig := musqlBusyTimeout()
	defer restoreBusyTimeout(orig)
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	cWrite := func() error {
		_, err := other.Exec(`INSERT INTO t VALUES(99, 'c')`)
		return err
	}
	// Control: with nobody exclusive, the other connection gets in. Without this
	// the assertion below could pass because two writers simply never coexist.
	if err := cWrite(); err != nil {
		t.Fatalf("control: the other connection must be able to write: %v", err)
	}
	if _, err := other.Exec(`DELETE FROM t`); err != nil {
		t.Fatalf("control cleanup: %v", err)
	}

	held, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	for _, s := range []string{
		`PRAGMA journal_mode = wal`,
		`PRAGMA locking_mode = exclusive`,
		`INSERT INTO t VALUES(1, 'a')`,
		`INSERT INTO t VALUES(2, 'b')`,
	} {
		if _, _, err := held.ExecArgs(s, nil); err != nil {
			held.Discard()
			t.Fatalf("%s: %v", s, err)
		}
	}
	werr := cWrite()
	if werr == nil {
		held.Close()
		t.Fatal("another connection committed a write while a session was in WAL + exclusive locking mode -- the mode is reported but the lock is not held")
	}
	if !strings.Contains(werr.Error(), "database is locked") {
		held.Close()
		t.Fatalf("the other connection was blocked, but not the way C blocks a second writer: got %v", werr)
	}
	// Close is this engine's commit; it takes the WAL path with the
	// locking-mode lock still held (writer.go's doCommitWAL /
	// releaseCommitLocks), which is the write the read-back below checks.
	if err := held.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// This engine keeps no wal-index either, in any mode -- which is what makes
	// serving the exclusive entry honest rather than a claim. (The -wal itself
	// need not be there: the commit that ENTERS WAL mode still takes the
	// ordinary rewrite path, because it has to stamp the header's file-format
	// versions -- see DB.walEntering.)
	if got := sideFiles(t, path); strings.Contains(got, "-shm") {
		t.Fatalf("no wal-index file may be written in exclusive locking mode, directory holds %s", got)
	}

	// The rows, read by the oracle over the export, cell for cell.
	cgodb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open(export): %v", err)
	}
	defer cgodb.Close()
	rows, err := cgodb.Query(`SELECT x, y FROM t ORDER BY x`)
	if err != nil {
		t.Fatalf("cgo read-back: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var x int
		var y string
		if err := rows.Scan(&x, &y); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, fmt.Sprintf("%d/%s", x, y))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if want := "[1/a 2/b]"; fmt.Sprintf("%v", got) != want {
		t.Fatalf("C SQLite reads back %v from a WAL written under exclusive locking mode, want %s", got, want)
	}
	var jm string
	if err := cgodb.QueryRow(`PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatalf("journal_mode read-back: %v", err)
	}
	if jm != "wal" {
		t.Fatalf("the file should still be in WAL mode, C SQLite reports %q", jm)
	}
	if err := cWrite(); err != nil {
		t.Fatalf("the other connection must get back in once the exclusive session is gone: %v", err)
	}
}

// musqlBusyTimeout shortens the engine's lock wait for a test that expects a
// BUSY, returning the value to restore.
func musqlBusyTimeout() time.Duration {
	orig := engine.BusyTimeout
	engine.BusyTimeout = 100 * time.Millisecond
	return orig
}

func restoreBusyTimeout(d time.Duration) { engine.BusyTimeout = d }

// ---- helpers ----

// checkProbes compares every walExclusiveProbe's single cell, plus the row
// count of the table both sides are writing, after one step.
func checkProbes(t *testing.T, godb *engine.Session, cgodb *sql.DB, i int, sql string) {
	t.Helper()
	for _, probe := range walExclusiveProbes {
		want := cgoLockingModeCell(t, cgodb, probe)
		got := goLockingModeCell(t, godb, probe)
		if got != want {
			t.Fatalf("after step %d %q: %s = %q, C SQLite answers %q", i, sql, probe, got, want)
		}
	}
	// The ROWS, not just the mode: a write taken under WAL + exclusive has to
	// be the same write on both sides.
	if gotRows, wantRows := goCount(t, godb), cgoCount(t, cgodb); gotRows != wantRows {
		t.Fatalf("after step %d %q: SELECT count(*) FROM t = %d, C SQLite answers %d", i, sql, gotRows, wantRows)
	}
}

// goLockingModeCell / cgoLockingModeCell read one value-less getter's single
// cell off each engine. locking_mode's column is named "locking_mode" and
// journal_mode's "journal_mode", so the shape check is by position rather than
// by name; goLockingMode (locking_mode_test.go) is the locking_mode-only
// version and stays as it is.
func goLockingModeCell(t *testing.T, db *engine.Session, probe string) string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	cols, rows, err := p.QueryArgs(probe, nil)
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	if len(cols) != 1 || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: shape is not one row of one column: cols=%v rows=%v", probe, cols, rows)
	}
	return string(rows[0][0].S)
}

func cgoLockingModeCell(t *testing.T, db *sql.DB, probe string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(probe).Scan(&s); err != nil {
		t.Fatalf("C SQLite %s: %v", probe, err)
	}
	return s
}

func goCount(t *testing.T, db *engine.Session) int64 {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := p.QueryArgs(`SELECT count(*) FROM t`, nil)
	if err != nil {
		t.Fatalf("SELECT count(*): %v", err)
	}
	return rows[0][0].I
}

func cgoCount(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("C SQLite SELECT count(*): %v", err)
	}
	return n
}

// mustCGOPragma runs a pragma on the oracle and asserts its single cell.
func mustCGOPragma(t *testing.T, db *sql.DB, pragma, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(pragma).Scan(&got); err != nil {
		t.Fatalf("C SQLite %s: %v", pragma, err)
	}
	if got != want {
		t.Fatalf("C SQLite %s = %q, this gate is written against %q", pragma, got, want)
	}
}

// sideFiles lists the database's directory, which is how the wal-index claim is
// checked: an exclusive-mode WAL has none on disk.
func sideFiles(t *testing.T, path string) string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return fmt.Sprintf("%v", names)
}
