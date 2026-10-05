// Gate for PRAGMA locking_mode, ensuring exclusive locks persist across statements.
package engine

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// lockingExec runs one statement for its error alone.
func lockingExec(db *Session, sqlText string) error {
	_, _, err := db.ExecArgs(sqlText, nil)
	return err
}

// lockingModeOf runs a PRAGMA locking_mode statement and returns the result.
func lockingModeOf(t *testing.T, db *Session, sqlText string) (string, error) {
	t.Helper()
	stmt, err := ParsePragma(sqlText)
	if err != nil {
		t.Fatalf("ParsePragma(%q): %v", sqlText, err)
	}
	cols, rows, err := db.execLockingMode(stmt)
	if err != nil {
		return "", err
	}
	if len(cols) != 1 || cols[0] != "locking_mode" || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: shape is not one row of one column named locking_mode: cols=%v rows=%v", sqlText, cols, rows)
	}
	return string(rows[0][0].S), nil
}

// TestPragmaLockingModeRows walks the exact statement sequences run against the
// oracle, asserting the same answers it gave. The sequences matter as much as
// the individual statements: the two pieces of state (the connection default
// and main's own mode) only diverge partway through one.
func TestPragmaLockingModeRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		// each step is "sql -> want", or "sql -> !" for a decline
		steps [][2]string
	}{
		{
			// Oracle, fresh file connection.
			name: "fresh-connection",
			steps: [][2]string{
				{`PRAGMA locking_mode`, "normal"},
				{`PRAGMA main.locking_mode`, "normal"},
				{`PRAGMA temp.locking_mode`, "exclusive"},
				{`PRAGMA locking_mode=normal`, "normal"},
				{`PRAGMA locking_mode=NORMAL`, "normal"},
				{`PRAGMA locking_mode='normal'`, "normal"},
				{`PRAGMA locking_mode(normal)`, "normal"},
				{`PRAGMA locking_mode=xyz`, "normal"},
			},
		},
		{
			// Oracle: exclusive.test's own 1.0-1.6 sequence, verbatim.
			name: "enter-and-leave",
			steps: [][2]string{
				{`PRAGMA locking_mode = exclusive`, "exclusive"},
				{`PRAGMA locking_mode`, "exclusive"},
				{`PRAGMA main.locking_mode`, "exclusive"},
				{`PRAGMA temp.locking_mode`, "exclusive"},
				{`PRAGMA locking_mode = EXCLUSIVE`, "exclusive"},
				{`PRAGMA locking_mode = normal`, "normal"},
				{`PRAGMA locking_mode`, "normal"},
				{`PRAGMA main.locking_mode`, "normal"},
				{`PRAGMA temp.locking_mode`, "exclusive"},
				{`PRAGMA locking_mode = invalid`, "normal"},
				{`PRAGMA locking_mode`, "normal"},
			},
		},
		{
			// The rule that is NOT guessable: the BARE getter reports the
			// CONNECTION DEFAULT, which "PRAGMA main.locking_mode = exclusive"
			// deliberately does not write. Oracle: after that setter the bare
			// getter still answers "normal" while "main." answers "exclusive".
			name: "bare-getter-is-the-default",
			steps: [][2]string{
				{`PRAGMA main.locking_mode = exclusive`, "exclusive"},
				{`PRAGMA locking_mode`, "normal"},
				{`PRAGMA main.locking_mode`, "exclusive"},
				{`PRAGMA main.locking_mode = xyz`, "exclusive"}, // a query of main
				{`PRAGMA locking_mode = xyz`, "normal"},         // a query of the default
				{`PRAGMA main.locking_mode = normal`, "normal"},
			},
		},
		{
			// temp is pinned: the setter cannot move it either.
			name: "temp-is-pinned",
			steps: [][2]string{
				{`PRAGMA temp.locking_mode`, "exclusive"},
				{`PRAGMA temp.locking_mode = normal`, "exclusive"},
				{`PRAGMA temp.locking_mode`, "exclusive"},
				{`PRAGMA locking_mode`, "normal"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := lockingTestDB(t)
			defer db.Discard()
			for i, step := range tc.steps {
				got, err := lockingModeOf(t, db, step[0])
				if step[1] == "!" {
					if err == nil {
						t.Fatalf("step %d %q: expected a decline, got %q", i, step[0], got)
					}
					continue
				}
				if err != nil {
					t.Fatalf("step %d %q: %v", i, step[0], err)
				}
				if got != step[1] {
					t.Fatalf("step %d %q = %q, C SQLite answers %q", i, step[0], got, step[1])
				}
			}
		})
	}
}

// TestPragmaLockingModeDeclines pins the three shapes this engine cannot hold a
// lock for and therefore refuses to report -- the alternative being the exact
// failure this pragma was declined outright to avoid.
func TestPragmaLockingModeDeclines(t *testing.T) {
	// LEAVING exclusive on a WAL database used to be declined outright, on the
	// rule "C SQLite can leave the mode only if the WAL has not been OPENED
	// yet". That rule is FALSE, re-measured against mattn/go-sqlite3 3.53.3:
	// pager.c refuses only while sqlite3WalHeapMemory(pWal), and wal.c sets
	// that flag ONCE, at sqlite3WalOpen, from the pager's mode AT THAT MOMENT.
	// So the mode is unleavable exactly when the WAL was opened while the pager
	// was ALREADY exclusive. Now tracked (walIndexKind), and each of the three
	// states is pinned here; the oracle's own side of the same programs is
	// compat-harness/pragma_r24_wal_index_test.go.
	t.Run("wal-leave-when-the-wal-was-never-opened", func(t *testing.T) {
		db := lockingTestDB(t)
		defer db.Discard()
		if err := lockingExec(db, `PRAGMA journal_mode=wal`); err != nil {
			t.Fatalf("journal_mode=wal: %v", err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil || got != "exclusive" {
			t.Fatalf("=exclusive on a WAL database = %q, %v; want exclusive", got, err)
		}
		// Nothing has opened the WAL, so there is no wal-index at all and the
		// leave is free -- the oracle answers "normal" here too.
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err != nil || got != "normal" {
			t.Fatalf("=normal with an unopened WAL = %q, %v; want normal", got, err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode`); err != nil || got != "normal" {
			t.Fatalf("getter = %q, %v; want normal", got, err)
		}
	})

	t.Run("wal-leave-after-the-wal-opened-under-exclusive", func(t *testing.T) {
		db := lockingTestDB(t)
		defer db.Discard()
		if err := lockingExec(db, `PRAGMA journal_mode=wal`); err != nil {
			t.Fatalf("journal_mode=wal: %v", err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil || got != "exclusive" {
			t.Fatalf("=exclusive = %q, %v; want exclusive", got, err)
		}
		// A statement that codes an OP_Transaction on main, run UNDER exclusive:
		// this is the one that puts the wal-index in heap memory.
		if err := lockingExec(db, `CREATE TABLE wl(a)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		// The pager refuses, so the SETTER answers main's unchanged mode...
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err != nil || got != "exclusive" {
			t.Fatalf("=normal with a heap wal-index = %q, %v; want exclusive", got, err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA main.locking_mode`); err != nil || got != "exclusive" {
			t.Fatalf("main getter = %q, %v; want exclusive", got, err)
		}
		// ...while the CONNECTION DEFAULT went to normal anyway, because
		// pragma.c writes db->dfltLockMode before it ever asks the pager.
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode`); err != nil || got != "normal" {
			t.Fatalf("bare getter = %q, %v; want normal", got, err)
		}
		// Leaving WAL mode closes the WAL, which clears the heap-memory flag
		// with it -- so the setter really lands from there.
		if err := lockingExec(db, `PRAGMA journal_mode=delete`); err != nil {
			t.Fatalf("journal_mode=delete: %v", err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err != nil || got != "normal" {
			t.Fatalf("=normal after leaving WAL = %q, %v; want normal", got, err)
		}
	})

	t.Run("wal-leave-after-an-unclassifiable-statement", func(t *testing.T) {
		// A top-level READ carries no statement text into the write session, so
		// whether it opened the WAL is unknown -- and "SELECT 1" and
		// "SELECT * FROM t" really do differ. Declined rather than guessed.
		db := lockingTestDB(t)
		defer db.Discard()
		if err := lockingExec(db, `PRAGMA journal_mode=wal`); err != nil {
			t.Fatalf("journal_mode=wal: %v", err)
		}
		p, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager: %v", perr)
		}
		p.Close()
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil || got != "exclusive" {
			t.Fatalf("=exclusive = %q, %v; want exclusive", got, err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err == nil {
			t.Fatalf("expected =normal to be declined after an unclassifiable statement, got %q", got)
		}
	})

	t.Run("wal-from-exclusive-leave", func(t *testing.T) {
		// The other order: entering WAL while already exclusive is served too,
		// and the new Wal is unopened, so the leave is free -- verified against
		// the oracle as exclusive.test#4's own sequence.
		db := lockingTestDB(t)
		defer db.Discard()
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil || got != "exclusive" {
			t.Fatalf("locking_mode=exclusive: %q, %v", got, err)
		}
		if err := lockingExec(db, `PRAGMA journal_mode=wal`); err != nil {
			t.Fatalf("journal_mode=wal while exclusive: %v", err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err != nil || got != "normal" {
			t.Fatalf("=normal = %q, %v; want normal", got, err)
		}
	})

	t.Run("memory-backed", func(t *testing.T) {
		db := lockingTestDB(t)
		defer db.Discard()
		db.SetInMemory(true)
		for _, s := range []string{`PRAGMA locking_mode`, `PRAGMA locking_mode=normal`, `PRAGMA locking_mode=exclusive`} {
			if got, err := lockingModeOf(t, db, s); err == nil {
				t.Fatalf("%s on a memory-backed database = %q, want a decline", s, got)
			}
		}
	})

	t.Run("attached", func(t *testing.T) {
		dir := t.TempDir()
		db := lockingTestDB(t)
		defer db.Discard()
		if err := lockingExec(db, `ATTACH DATABASE '`+filepath.Join(dir, "aux.musq")+`' AS aux`); err != nil {
			t.Fatalf("ATTACH: %v", err)
		}
		// Unqualified, with an attachment: C SQLite locks aux's file too.
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err == nil {
			t.Fatalf("expected a decline with an attachment present, got %q", got)
		}
		// ...but everything that changes nothing is still answered.
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err != nil || got != "normal" {
			t.Fatalf("=normal with an attachment = %q, %v; want normal", got, err)
		}
		// The qualified form is ROUTED to aux's own session, which declines it
		// for its own reason (see execLockingMode's isAttachedSession check) --
		// that path is gated by attach_write_test.go's accept/decline split.
	})

	t.Run("attach-while-exclusive", func(t *testing.T) {
		// The mirror image: C SQLite gives a database attached under an
		// exclusive connection that SAME mode and locks its file -- ATTACH is
		// no longer declined here, it PROPAGATES the mode (attach.c:215's
		// dfltLockMode seed; see execAttach's own citation). The full
		// statement-by-statement sequence this closes (exclusive.test's own
		// 1.7-1.13) lives in attach_exclusive_locking_test.go; this case just
		// pins that the decline is gone and the round-trip (attach while
		// exclusive, then leave, then attach again while normal) both work.
		dir := t.TempDir()
		db := lockingTestDB(t)
		defer db.Discard()
		if _, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil {
			t.Fatalf("locking_mode=exclusive: %v", err)
		}
		if err := lockingExec(db, `ATTACH DATABASE '`+filepath.Join(dir, "aux.musq")+`' AS aux`); err != nil {
			t.Fatalf("ATTACH while in exclusive locking mode: %v", err)
		}
		if got := attachedLockingModeOf(t, db, "aux"); got != "exclusive" {
			t.Fatalf("aux.locking_mode right after ATTACH = %q; want exclusive", got)
		}
		if err := lockingExec(db, `DETACH aux`); err != nil {
			t.Fatalf("DETACH aux: %v", err)
		}
		// Leaving the mode, then attaching again, still works -- and the new
		// attachment now comes back normal (the default it inherits).
		if _, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err != nil {
			t.Fatalf("locking_mode=normal: %v", err)
		}
		if err := lockingExec(db, `ATTACH DATABASE '`+filepath.Join(dir, "aux.musq")+`' AS aux`); err != nil {
			t.Fatalf("ATTACH after leaving exclusive mode: %v", err)
		}
		if got := attachedLockingModeOf(t, db, "aux"); got != "normal" {
			t.Fatalf("aux.locking_mode after leaving exclusive = %q; want normal", got)
		}
	})
}

// TestPragmaLockingModeHoldsTheLock is the gate that matters: a SECOND writer
// on the same file must be locked out for as long as an exclusive-mode session
// is alive, and must get in again once that session ends. (A reader is NOT
// locked out -- see holdLockingModeLock for why that is the faithful rung, and
// compat-harness/locking_mode_test.go, which checks it with a C SQLite
// connection.)
//
// Mutation check for this gate: with execLockingMode's holdLockingModeLock call
// removed (an "accept and report, lock nothing" implementation -- the exact
// failure mode this pragma was declined to avoid), the exclusive write below
// SUCCEEDS and the test fails.
func TestPragmaLockingModeHoldsTheLock(t *testing.T) {
	orig := BusyTimeout
	BusyTimeout = 150 * time.Millisecond // fail fast instead of waiting out the 5s default
	defer func() { BusyTimeout = orig }()

	path := filepath.Join(t.TempDir(), "x.musq")
	seed, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := lockingExec(seed, `CREATE TABLE t(x)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	// otherWriterCommits reports whether a second, independent write session can
	// commit an INSERT into the same file right now.
	otherWriterCommits := func() error {
		w, err := OpenWrite(path)
		if err != nil {
			return err
		}
		if err := lockingExec(w, `INSERT INTO t VALUES(1)`); err != nil {
			w.Discard()
			return err
		}
		return w.Close()
	}

	// Control: with nobody in exclusive mode, the second writer gets in. This is
	// what keeps the assertion below from passing for the wrong reason.
	if err := otherWriterCommits(); err != nil {
		t.Fatalf("control: a second writer must be able to commit when nobody holds an exclusive lock: %v", err)
	}

	a, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if got, err := lockingModeOf(t, a, `PRAGMA locking_mode=exclusive`); err != nil || got != "exclusive" {
		a.Discard()
		t.Fatalf("locking_mode=exclusive = %q, %v", got, err)
	}
	if !a.lockingHeld {
		a.Discard()
		t.Fatal("the session reports exclusive but holds no lock -- that is the accept-and-lie failure mode")
	}
	err = otherWriterCommits()
	if !errors.Is(err, ErrBusy) {
		a.Discard()
		t.Fatalf("a second writer committed while an exclusive-mode session held the file (err=%v); the lock is not really held", err)
	}

	// Leaving the mode releases it: the lock this session holds is dropped when
	// its transaction ends, which for a *DB is its Discard/Close.
	if got, err := lockingModeOf(t, a, `PRAGMA locking_mode=normal`); err != nil || got != "normal" {
		a.Discard()
		t.Fatalf("locking_mode=normal = %q, %v", got, err)
	}
	if err := a.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if err := otherWriterCommits(); err != nil {
		t.Fatalf("a second writer must get in once the exclusive session is gone: %v", err)
	}
}

// lockingTestDB is a fresh, still-open, file-backed write session holding one
// table -- the state every case above starts from.
func lockingTestDB(t *testing.T) *Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := lockingExec(db, `CREATE TABLE t(x)`); err != nil {
		db.Discard()
		t.Fatalf("CREATE TABLE: %v", err)
	}
	return db
}
