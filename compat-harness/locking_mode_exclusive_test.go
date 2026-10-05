package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestLockingModeFormsThroughDriver checks all forms of locking_mode through the driver.
// change of lock LIFETIME is "= exclusive"; what it then owes is
// TestLockingModeExclusiveOracleSpec's table, run against both engines by
// TestLockingModeExclusiveThroughDriver.
func TestLockingModeFormsThroughDriver(t *testing.T) {
	declines := map[string]bool{}
	for qi, q := range []string{
		`PRAGMA locking_mode`,
		`PRAGMA main.locking_mode`,
		`PRAGMA temp.locking_mode`,
		`PRAGMA locking_mode=normal`,
		`PRAGMA main.locking_mode=normal`,
		`PRAGMA temp.locking_mode=normal`,
		// C pins temp EXCLUSIVE and refuses to change it, so this one is
		// answerable without entering any mode at all.
		`PRAGMA temp.locking_mode=exclusive`,
		// Unrecognized right-hand sides: pure queries per getLockingMode.
		`PRAGMA locking_mode=xyz`,
		`PRAGMA locking_mode=1`,
		`PRAGMA locking_mode=''`,
		`PRAGMA main.locking_mode=xyz`,
		// And the two that ask for the real thing.
		`PRAGMA locking_mode=exclusive`,
		`PRAGMA locking_mode=EXCLUSIVE`,
		`PRAGMA main.locking_mode=exclusive`,
	} {
		qi, q := qi, q
		t.Run(fmt.Sprintf("%02d", qi), func(t *testing.T) {
			var out [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				if _, err := db.Exec(`CREATE TABLE t(x)`); err != nil {
					t.Fatal(err)
				}
				out[i] = renderQuery(db, q)
				db.Close()
			}
			if declines[q] {
				if out[1] != "ERR" {
					t.Errorf("%s: served now (%s) -- if the lock lifetime is really held, move it out of the decline set", q, out[1])
				}
				if out[0] == "ERR" {
					t.Errorf("%s: the ORACLE refuses it too -- this case no longer measures a decline", q)
				}
				return
			}
			if out[0] != out[1] {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", q, out[0], out[1])
			}
		})
	}
}

// TestLockingModeExclusiveLeavesTheModeLikeC pins the counter-intuitive half
// of the pragma: "= normal" flips what the getter REPORTS at once, and the lock
// is not given up until the next access ends. Measured on the oracle with two
// live connections -- right after "= normal" conn2 still gets "database is
// locked", and only after conn1 runs one more statement does it get in.
func TestLockingModeExclusiveLeavesTheModeLikeC(t *testing.T) {
	for _, drv := range []string{"sqlite3", "sqlite"} {
		drv := drv
		t.Run(drv, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.db")
			a, err := sql.Open(drv, p)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			a.SetMaxOpenConns(1)
			for _, s := range []string{`CREATE TABLE t(x)`, `INSERT INTO t VALUES(1)`,
				`PRAGMA locking_mode=exclusive`, `INSERT INTO t VALUES(2)`} {
				if _, err := a.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			if _, err := a.Exec(`PRAGMA locking_mode=normal`); err != nil {
				t.Fatal(err)
			}
			if got := renderQuery(a, `PRAGMA locking_mode`); got != "[locking_mode][normal]" {
				t.Errorf("the getter must report normal at once, got %s", got)
			}
			// ...and one more statement on a, which is the access whose end
			// gives the lock up.
			if _, err := a.Exec(`INSERT INTO t VALUES(3)`); err != nil {
				t.Fatal(err)
			}
			b, err := sql.Open(drv, p)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			b.SetMaxOpenConns(1)
			if _, err := b.Exec(`INSERT INTO t VALUES(9)`); err != nil {
				t.Errorf("after leaving the mode a second connection must get in: %v", err)
			}
		})
	}
}

// TestLockingModeExclusiveOracleSpec measures, on the ORACLE alone, exactly
// what an implementation of "= exclusive" owes -- so the decline's cost is a
// number rather than an opinion, and so the eventual implementation has a
// target table to hit. Nothing here runs against this engine.
//
// The rows say three things worth knowing:
//
//   - the pragma ALONE excludes nobody. C's own doc puts it as "the change does
//     not actually take effect until the next time the database file is
//     accessed", and it is visible here: with the mode set and NO statement
//     run, a second connection still reads AND writes.
//   - after a READ, the exclusive connection holds SHARED: another connection
//     can still read and can no longer write (sqlite3PagerLockingMode,
//     pager.c; holdLockingModeLock in engine/lock.go is this engine's port of
//     exactly that, for a long-lived session).
//   - after a WRITE, it holds EXCLUSIVE: another connection loses the read too.
//     In WAL mode it loses the read as soon as the exclusive connection has
//     merely read, and no -shm file is created at all.
//
// Which is why this cannot be served through this driver as it stands: the
// lock would have to be held across statements on ONE descriptor, and a
// driver connection has FOUR onto its main file -- Conn.readFile
// (O_RDONLY, every autocommit SELECT takes SHARED on it), engine.Session.f
// (O_RDWR, engine/session.go, where the engine's own holdLockingModeLock
// puts the lock), the fresh O_RDONLY that engine.withSharedLock opens per
// Session.BeginWrite (engine/lock.go, engine/session.go), and, in WAL
// mode, the write *DB's own independent fd (openWriteLocked degrades the
// Session borrow to nil for WAL, engine/writer_open.go). These are OFD
// locks, owned by the open file DESCRIPTION, so holding EXCLUSIVE on any one of
// them blocks this same connection's next statement on another exactly as
// another process would. Serving the pragma means unifying those four onto one
// descriptor for the connection's lifetime, which is what C's unixFile already
// is (one `int h` per connection, os_unix.c:261).
func TestLockingModeExclusiveOracleSpec(t *testing.T) {
	type want struct{ bRead, bWrite bool }
	for _, c := range []struct {
		journal, act string
		want         want
		shm          bool
	}{
		{"delete", "none", want{true, true}, false},
		{"delete", "read", want{true, false}, false},
		{"delete", "write", want{false, false}, false},
		// wal-none is the only row where the second connection gets to WRITE,
		// which is what creates the -shm: under exclusive WAL there is none.
		{"wal", "none", want{true, true}, true},
		{"wal", "read", want{false, false}, false},
		{"wal", "write", want{false, false}, false},
	} {
		c := c
		t.Run(c.journal+"-"+c.act, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.db")
			a, err := sql.Open("sqlite3", p)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			a.SetMaxOpenConns(1)
			for _, s := range []string{`CREATE TABLE t(x)`, `INSERT INTO t VALUES(1)`,
				`PRAGMA journal_mode=` + c.journal, `PRAGMA locking_mode=exclusive`} {
				if _, err := a.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			if got := renderQuery(a, `PRAGMA locking_mode`); got != "[locking_mode][exclusive]" {
				t.Fatalf("the oracle did not enter exclusive mode: %s", got)
			}
			switch c.act {
			case "read":
				if _, err := a.Exec(`SELECT count(*) FROM t`); err != nil {
					t.Fatal(err)
				}
			case "write":
				if _, err := a.Exec(`INSERT INTO t VALUES(2)`); err != nil {
					t.Fatal(err)
				}
			}
			// The busy timeout goes in the DSN, not in a statement: PREPARING
			// any statement -- "PRAGMA busy_timeout" included -- loads the
			// schema, which needs a SHARED lock, so under exclusive mode the
			// statement that would shorten the wait is itself the one that
			// waits the default 5s.
			b, err := sql.Open("sqlite3", p+"?_busy_timeout=50")
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			b.SetMaxOpenConns(1)
			_, rerr := b.Exec(`SELECT count(*) FROM t`)
			_, werr := b.Exec(`INSERT INTO t VALUES(9)`)
			if (rerr == nil) != c.want.bRead {
				t.Errorf("second connection read: want ok=%v, got %v", c.want.bRead, rerr)
			}
			if (werr == nil) != c.want.bWrite {
				t.Errorf("second connection write: want ok=%v, got %v", c.want.bWrite, werr)
			}
			if _, err := os.Stat(p + "-shm"); (err == nil) != c.shm {
				t.Errorf("-shm present=%v, want %v", err == nil, c.shm)
			}
		})
	}
}
