// Tests PRAGMA locking_mode behavior.
//
//	releasing at the end of each one -- and a promise is exactly what a
//	single-connection test cannot check. So the second test puts a real C
//	SQLite connection on the same file and asserts it is locked out the way
//	C SQLite locks out a second connection, and gets back in once the
//	exclusive session is gone. Reporting "exclusive" while still locking
//	per-operation is the failure this catches; it is why the pragma was
//	declined outright before.
package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// lockingModeStep is one statement plus whether this engine is expected to
// DECLINE it. Every step that is not declined must leave the engine reporting
// exactly what C SQLite reports for all three probe forms.
type lockingModeStep struct {
	sql     string
	decline bool
	why     string
}

// The sequences below are the ones actually run against mattn/go-sqlite3
// 3.53.3; the probe forms after each step are the bare getter, "main." and
// "temp.". exclusive.test's own 1.0-1.6 block is the first of them, verbatim.
var lockingModeSequences = map[string][]lockingModeStep{
	"enter-and-leave": {
		{sql: `PRAGMA locking_mode`},
		{sql: `PRAGMA locking_mode = exclusive`},
		{sql: `PRAGMA locking_mode = EXCLUSIVE`},
		{sql: `PRAGMA locking_mode = normal`},
		{sql: `PRAGMA locking_mode = invalid`},
		{sql: `PRAGMA locking_mode = exclusive`},
		{sql: `PRAGMA locking_mode = xyz`}, // unrecognized: a pure query
		{sql: `PRAGMA locking_mode = NORMAL`},
	},
	"spellings": {
		{sql: `PRAGMA locking_mode = 'exclusive'`},
		{sql: `PRAGMA locking_mode(normal)`},
		{sql: `PRAGMA locking_mode = 'EXCLUSIVE'`},
		{sql: `PRAGMA locking_mode(NORMAL)`},
	},
	"qualified-main-does-not-move-the-default": {
		{sql: `PRAGMA main.locking_mode = exclusive`},
		{sql: `PRAGMA main.locking_mode = xyz`},
		{sql: `PRAGMA main.locking_mode = normal`},
	},
	"temp-is-pinned": {
		{sql: `PRAGMA temp.locking_mode = normal`},
		{sql: `PRAGMA temp.locking_mode = exclusive`},
		{sql: `PRAGMA locking_mode = exclusive`},
		{sql: `PRAGMA temp.locking_mode = normal`},
	},
	// The WAL interaction -- entering exclusive on a WAL database, entering WAL
	// while already exclusive, and the one setter still declined in that state
	// -- lives in wal_exclusive_locking_test.go, which needs probes and controls
	// this table has no room for. Both orders used to be declined here.
}

// lockingModeProbes are read back after every step. All three are value-less
// getters, so running them never changes either side's state.
var lockingModeProbes = []string{
	`PRAGMA locking_mode`,
	`PRAGMA main.locking_mode`,
	`PRAGMA temp.locking_mode`,
}

func TestPragmaLockingModeMatchesCSQLite(t *testing.T) {
	for name, steps := range lockingModeSequences {
		t.Run(name, func(t *testing.T) {
			godb, cgodb := lockingModePair(t)
			defer godb.Discard()
			defer cgodb.Close()

			for i, step := range steps {
				_, _, goErr := godb.ExecArgs(step.sql, nil)
				if step.decline {
					if goErr == nil {
						t.Fatalf("step %d %q: expected this engine to DECLINE (%s), it accepted", i, step.sql, step.why)
					}
					// A declined statement is deliberately NOT replayed against
					// the oracle -- that is what keeps the two sides in the same
					// mode, which the probes below then confirm.
				} else {
					if goErr != nil {
						t.Fatalf("step %d %q: this engine declined: %v", i, step.sql, goErr)
					}
					if _, cerr := cgodb.Exec(step.sql); cerr != nil {
						t.Fatalf("step %d %q: C SQLite rejected a statement this engine accepted: %v", i, step.sql, cerr)
					}
				}
				for _, probe := range lockingModeProbes {
					want := cgoLockingMode(t, cgodb, probe)
					got := goLockingMode(t, godb, probe)
					if got != want {
						t.Fatalf("after step %d %q: %s = %q, C SQLite answers %q", i, step.sql, probe, got, want)
					}
				}
			}
		})
	}
}

// TestPragmaLockingModeLocksOutAnotherConnection is the concurrency half. It
// drives the stages pinned against two live C SQLite connections
// (TestLockingModeExclusiveOracleSpec):
//
//	before "= exclusive"            -> the second connection reads AND writes
//	after  "= exclusive" + a read   -> it still READS, but its write is BUSY
//	after the exclusive session ends -> it reads and writes again
//
// The second connection is a musql one: C cannot open this engine's file (AGENTS.md
// rule 3), and what the stages pin is the lock, which any other connection meets
// the same way. The held session is engine-direct, where the pragma is itself the
// first access (engine/lock.go's holdLockingModeLock takes SHARED there).
//
// Mutation check: with holdLockingModeLock's acquisition removed -- an "accept
// and report, lock nothing" implementation -- the BUSY assertion below fails.
func TestPragmaLockingModeLocksOutAnotherConnection(t *testing.T) {
	orig := engine.BusyTimeout
	engine.BusyTimeout = 100 * time.Millisecond // a blocked statement fails fast
	defer func() { engine.BusyTimeout = orig }()

	path := filepath.Join(t.TempDir(), "shared.db")
	seed, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	if _, _, err := seed.ExecArgs(`CREATE TABLE t(x)`, nil); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	oRead := func() error {
		var n int
		return other.QueryRow(`SELECT count(*) FROM t`).Scan(&n)
	}
	oWrite := func() error {
		_, err := other.Exec(`INSERT INTO t VALUES(1)`)
		return err
	}

	// Control: nobody is in exclusive mode, so the other connection gets in.
	if err := oRead(); err != nil {
		t.Fatalf("control: the other connection must be able to read: %v", err)
	}
	if err := oWrite(); err != nil {
		t.Fatalf("control: the other connection must be able to write: %v", err)
	}

	held, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if _, _, err := held.ExecArgs(`PRAGMA locking_mode = exclusive`, nil); err != nil {
		held.Discard()
		t.Fatalf("locking_mode=exclusive: %v", err)
	}
	if err := oRead(); err != nil {
		held.Discard()
		t.Fatalf("the other connection must still READ while the exclusive session holds only SHARED (C's conn2 SELECT succeeds there too): %v", err)
	}
	werr := oWrite()
	if werr == nil {
		held.Discard()
		t.Fatal("another connection committed a write while a session was in exclusive locking mode -- the mode is reported but the lock is not held")
	}
	if !strings.Contains(werr.Error(), "database is locked") {
		held.Discard()
		t.Fatalf("the other connection was blocked, but not the way C blocks a second writer: got %v, want %q", werr, "database is locked")
	}

	if err := held.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if err := oWrite(); err != nil {
		t.Fatalf("the other connection must get back in once the exclusive session is gone: %v", err)
	}
}

// ---- helpers ----

// lockingModePair is one fresh engine write session and one fresh C SQLite
// connection over SEPARATE files holding the identical schema -- separate
// because this test is about what each side REPORTS, and a shared file would
// have the exclusive session lock the oracle out (which the other test is for).
func lockingModePair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	goPath := filepath.Join(dir, "go.db")
	cgoPath := filepath.Join(dir, "cgo.db")
	godb, err := engine.Create(goPath)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	if _, _, err := godb.ExecArgs(`CREATE TABLE t(x)`, nil); err != nil {
		godb.Discard()
		t.Fatalf("CREATE TABLE: %v", err)
	}
	cgodb, err := sql.Open("sqlite3", cgoPath)
	if err != nil {
		godb.Discard()
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	// One logical connection: locking_mode is CONNECTION state, so a pooled
	// second connection would answer for a connection that never saw the setter.
	cgodb.SetMaxOpenConns(1)
	if _, err := cgodb.Exec(`CREATE TABLE t(x)`); err != nil {
		godb.Discard()
		cgodb.Close()
		t.Fatalf("cgo CREATE TABLE: %v", err)
	}
	if _, err := os.Stat(cgoPath); err != nil {
		godb.Discard()
		cgodb.Close()
		t.Fatalf("cgo database not created: %v", err)
	}
	return godb, cgodb
}

// goLockingMode reads one value-less locking_mode getter back through the
// engine's READ path over the write session's current state -- the same route
// a held transaction's getter takes (SnapshotPager, engine/writer.go).
func goLockingMode(t *testing.T, db *engine.Session, probe string) string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	cols, rows, err := p.QueryArgs(probe, nil)
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	if len(cols) != 1 || cols[0] != "locking_mode" || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: shape is not one row of one column named locking_mode: cols=%v rows=%v", probe, cols, rows)
	}
	return string(rows[0][0].S)
}

func cgoLockingMode(t *testing.T, db *sql.DB, probe string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(probe).Scan(&s); err != nil {
		t.Fatalf("C SQLite %s: %v", probe, err)
	}
	return s
}
