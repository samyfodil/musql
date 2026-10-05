package driver

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/internal/filelock"
)

// TestSegmentAutocommitFailureLeavesNothingBehind verifies that failed autocommit
// statements do not leave partial row changes in memory.
func TestSegmentAutocommitFailureLeavesNothingBehind(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,10),(2,20)`,
	} {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}
	if _, eerr := db.Exec(`INSERT INTO t VALUES(3,30),(4,20)`); eerr == nil {
		t.Fatal("the UNIQUE violation was accepted")
	}
	var n int
	if qerr := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); qerr != nil {
		t.Fatal(qerr)
	}
	if n != 2 {
		t.Errorf("after the failed INSERT this connection sees %d rows, want 2: the "+
			"statement's partial effect survived in the session's row store", n)
	}
	// ...and a LATER successful commit must not carry it either.
	if _, eerr := db.Exec(`INSERT INTO t VALUES(5,50)`); eerr != nil {
		t.Fatal(eerr)
	}
	if qerr := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); qerr != nil {
		t.Fatal(qerr)
	}
	if n != 3 {
		t.Errorf("rows = %d, want 3: the failed statement's row was appended by a later commit", n)
	}
	// A fresh connection reads the file, so this is what was actually committed.
	db2, err2 := sql.Open(DriverName, filepath.Join(t.TempDir(), "unused.musq"))
	if err2 != nil {
		t.Fatal(err2)
	}
	db2.Close()
}

// TestSegmentHoldsOneSessionPerConnection is item (1) of the goal made executable:
// no throwaway per-statement session remains. A held session keeps its loaded row
// stores, so the SECOND statement must not re-read the table from the file.
func TestSegmentHoldsOneSessionPerConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.musq")
	db, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, eerr := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)`); eerr != nil {
		t.Fatal(eerr)
	}

	for i := 1; i <= 3; i++ {
		if _, eerr := db.Exec(`INSERT INTO t VALUES(?,?)`, i, i*10); eerr != nil {
			t.Fatal(eerr)
		}
	}
	var n int
	if qerr := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); qerr != nil {
		t.Fatal(qerr)
	}
	if n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
	// Every statement above committed, so a SECOND *sql.DB on the same file -- a
	// different connection, a different session -- finds all three.
	other, oerr := sql.Open(DriverName, path)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer other.Close()
	var m int
	if qerr := other.QueryRow(`SELECT count(*) FROM t`).Scan(&m); qerr != nil {
		t.Fatalf("a second connection cannot read what autocommit committed: %v", qerr)
	}
	if m != 3 {
		t.Errorf("a second connection sees %d rows, want 3 -- autocommit did not commit", m)
	}
}

// TestSegmentBusyCommitLeavesNothingBehind is the sibling of the test above for
// the case it does NOT cover: a statement that RAN CLEANLY and whose COMMIT then
// failed.
//
// The engine's statement-level atomicity is what makes the test above true, and
// it does nothing here -- the statement did not fail, so nothing undid it. Its
// rows sit in the held session's stores with the caller already told the
// statement failed, and the NEXT statement's commit appends them. N4's
// readers-and-writers repro found it as a LOST row: a DELETE answered busy, so
// its ledger never recorded it, and the kill record rode out on the next commit
// anyway -- the concurrent run was missing a row its own ledger replay kept.
//
// The condition is made directly rather than by timing: another descriptor holds
// the segment database's lock file, which is what a commit's append has to take
// (engine's withSegmentWriteLock). See engine's own
// TestN4BusyClassificationIsComplete for the same standing-in.
func TestSegmentBusyCommitLeavesNothingBehind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busycommit.musq")
	db, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// ONE connection, so the statement that fails and the statement after it share
	// the session -- which is the whole mechanism.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES(1)`); err != nil {
		t.Fatal(err)
	}

	holder, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if ok, lerr := filelock.LockRange(holder, 0, 0, true); lerr != nil || !ok {
		t.Fatalf("holder: locking the lock file: ok=%v err=%v", ok, lerr)
	}

	oldTimeout := engine.BusyTimeout
	engine.BusyTimeout = 50 * time.Millisecond
	defer func() { engine.BusyTimeout = oldTimeout }()

	// The DELETE runs, cannot commit, and is reported as failed.
	if _, err := db.Exec(`DELETE FROM t WHERE id=1`); err == nil {
		t.Fatal("expected the DELETE's commit to fail while the lock is held")
	} else if !errors.Is(err, engine.ErrBusy) {
		t.Fatalf("expected engine.ErrBusy, got %v", err)
	}

	filelock.UnlockRange(holder, 0, 0)
	engine.BusyTimeout = oldTimeout

	// The next statement on the same connection commits. It must carry ITS OWN
	// change and nothing else.
	if _, err := db.Exec(`INSERT INTO t VALUES(2)`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow(`SELECT group_concat(id) FROM (SELECT id FROM t ORDER BY id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "1,2" {
		t.Fatalf("this connection sees %q, want \"1,2\" -- the busy DELETE was published anyway", got)
	}
	// And on the file, read by a connection that never saw the failed statement.
	db2, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.QueryRow(`SELECT group_concat(id) FROM (SELECT id FROM t ORDER BY id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "1,2" {
		t.Fatalf("the FILE holds %q, want \"1,2\" -- a statement reported busy was committed", got)
	}
}
