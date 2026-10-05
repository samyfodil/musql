// Package engine tests WAL index state ordering in queries.
package engine

import (
	"path/filepath"
	"testing"
)

// r25WalRead runs a statement through snapshot pager and query args.
func r25WalRead(t *testing.T, db *Session, sqlText string) error {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	_, _, qerr := p.QueryArgs(sqlText, nil)
	return qerr
}

// r25WalSnapshotOnly takes a snapshot without querying it.
func r25WalSnapshotOnly(t *testing.T, db *Session) {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	p.Close()
}

func r25WalDB(t *testing.T) *Session {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "r25wal.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { db.Discard() })
	for _, s := range []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`, `PRAGMA journal_mode=wal`} {
		if _, _, err := db.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return db
}

func TestPragmaR25WalIndexReadResolution(t *testing.T) {
	t.Run("a-query-that-reads-main-opens-the-wal", func(t *testing.T) {
		// Before exclusive mode: the wal-index is a real -shm file, so the leave
		// is free for that Wal's whole life. (Oracle: "normal".)
		db := r25WalDB(t)
		if err := r25WalRead(t, db, `SELECT * FROM t`); err != nil {
			t.Fatalf("SELECT * FROM t: %v", err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil || got != "exclusive" {
			t.Fatalf("=exclusive = %q, %v", got, err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err != nil || got != "normal" {
			t.Fatalf("=normal after an shm wal-index = %q, %v; want normal", got, err)
		}
		if db.walIndexState != walIndexShm {
			t.Fatalf("wal-index state = %v, want walIndexShm", db.walIndexState)
		}
	})

	t.Run("a-declined-query-still-classifies", func(t *testing.T) {
		// The classification is a fact about the program C SQLite would code,
		// not about whether this engine can run the statement -- which is what
		// makes wal2.test#7 work, since musql declines its "SELECT * FROM
		// sqlite_master" over the rootpage column.
		db := r25WalDB(t)
		if err := r25WalRead(t, db, `SELECT * FROM sqlite_master`); err == nil {
			t.Skip("this engine now serves SELECT * FROM sqlite_master; the case it stood for is covered by the harness")
		}
		if db.walIndexState != walIndexShm {
			t.Fatalf("wal-index state after a DECLINED read = %v, want walIndexShm", db.walIndexState)
		}
	})

	t.Run("a-query-under-exclusive-puts-it-in-heap", func(t *testing.T) {
		db := r25WalDB(t)
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil || got != "exclusive" {
			t.Fatalf("=exclusive = %q, %v", got, err)
		}
		if err := r25WalRead(t, db, `SELECT * FROM t`); err != nil {
			t.Fatalf("SELECT * FROM t: %v", err)
		}
		if db.walIndexState != walIndexHeap {
			t.Fatalf("wal-index state = %v, want walIndexHeap", db.walIndexState)
		}
		// The pager refuses, so main stays exclusive while the connection
		// default goes normal anyway -- pragma.c writes it unconditionally.
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err != nil || got != "exclusive" {
			t.Fatalf("=normal with a heap wal-index = %q, %v; want exclusive", got, err)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode`); err != nil || got != "normal" {
			t.Fatalf("bare getter = %q, %v; want normal", got, err)
		}
	})

	t.Run("a-query-outside-the-measured-list-is-unknown", func(t *testing.T) {
		// "SELECT * FROM (SELECT 1)" is measured NO against the oracle, but this
		// classifier only knows one end of the rule, so it declines rather than
		// guess -- a deliberate over-refusal.
		db := r25WalDB(t)
		if _, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil {
			t.Fatalf("=exclusive: %v", err)
		}
		if err := r25WalRead(t, db, `SELECT * FROM (SELECT 1)`); err != nil {
			t.Fatalf("SELECT * FROM (SELECT 1): %v", err)
		}
		if db.walIndexState != walIndexUnknown {
			t.Fatalf("wal-index state = %v, want walIndexUnknown", db.walIndexState)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err == nil {
			t.Fatalf("expected a decline after an unclassifiable query, got %q", got)
		}
	})

	// ---- the ordering hazards ----

	t.Run("a-snapshot-whose-query-never-arrives-hardens", func(t *testing.T) {
		// The wrong answer this guards: snapshot (provisional) -> a write that
		// really opens the Wal in heap memory -> a later "SELECT 1"-shaped query
		// resolving the STALE provisional back to "not opened", which would
		// answer "normal" where the oracle answers "exclusive".
		db := r25WalDB(t)
		if _, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil {
			t.Fatalf("=exclusive: %v", err)
		}
		r25WalSnapshotOnly(t, db)
		if _, _, err := db.ExecArgs(`INSERT INTO t VALUES(2)`, nil); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		if db.walIndexState != walIndexUnknown {
			t.Fatalf("wal-index state = %v, want walIndexUnknown (the unresolved snapshot must harden)", db.walIndexState)
		}
		if err := r25WalRead(t, db, `SELECT * FROM t`); err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		if db.walIndexState != walIndexUnknown {
			t.Fatalf("a later query resolved a STALE provisional: state = %v", db.walIndexState)
		}
		if got, err := lockingModeOf(t, db, `PRAGMA locking_mode=normal`); err == nil {
			t.Fatalf("expected a decline, got %q", got)
		}
	})

	t.Run("two-snapshots-with-only-the-second-queried", func(t *testing.T) {
		// The same hazard without a write between them: the first snapshot's
		// query never arrived, so what it did is unknown, and the second
		// snapshot's query cannot speak for it.
		db := r25WalDB(t)
		if _, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil {
			t.Fatalf("=exclusive: %v", err)
		}
		r25WalSnapshotOnly(t, db)
		if err := r25WalRead(t, db, `SELECT * FROM t`); err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		if db.walIndexState != walIndexUnknown {
			t.Fatalf("wal-index state = %v, want walIndexUnknown", db.walIndexState)
		}
	})

	t.Run("a-failed-statement-is-final", func(t *testing.T) {
		// ExecArgs' own opaque case must not be resolvable either: where a
		// statement failed decides whether it opened a transaction, and nothing
		// later supplies that.
		db := r25WalDB(t)
		if _, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil {
			t.Fatalf("=exclusive: %v", err)
		}
		if _, _, err := db.ExecArgs(`INSERT INTO nosuchtable VALUES(1)`, nil); err == nil {
			t.Fatal("expected the INSERT to fail")
		}
		if db.walIndexState != walIndexUnknown {
			t.Fatalf("wal-index state = %v, want walIndexUnknown", db.walIndexState)
		}
		if err := r25WalRead(t, db, `SELECT * FROM t`); err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		if db.walIndexState != walIndexUnknown {
			t.Fatalf("a query resolved a FAILED statement's unknown: state = %v", db.walIndexState)
		}
	})

	t.Run("a-temp-object-makes-every-query-unknown", func(t *testing.T) {
		// "SELECT * FROM tt" over a temp tt is NO while "SELECT * FROM t" is
		// YES, and the catalog lookup cannot tell the spellings apart -- the
		// same narrowing the DML case needs.
		db := r25WalDB(t)
		if _, _, err := db.ExecArgs(`CREATE TEMP TABLE tt(a)`, nil); err != nil {
			t.Fatalf("CREATE TEMP TABLE: %v", err)
		}
		if _, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil {
			t.Fatalf("=exclusive: %v", err)
		}
		if err := r25WalRead(t, db, `SELECT * FROM t`); err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		if db.walIndexState != walIndexUnknown {
			t.Fatalf("wal-index state = %v, want walIndexUnknown", db.walIndexState)
		}
	})

	t.Run("a-cte-shadowing-a-table-is-unknown", func(t *testing.T) {
		// "WITH t(a) AS (VALUES(1)) SELECT * FROM t" reads nothing at all, so
		// resolving "t" against the real table it shadows would be a wrong YES.
		db := r25WalDB(t)
		if _, err := lockingModeOf(t, db, `PRAGMA locking_mode=exclusive`); err != nil {
			t.Fatalf("=exclusive: %v", err)
		}
		if err := r25WalRead(t, db, `WITH t(a) AS (VALUES(1)) SELECT * FROM t`); err != nil {
			t.Fatalf("WITH: %v", err)
		}
		if db.walIndexState != walIndexUnknown {
			t.Fatalf("wal-index state = %v, want walIndexUnknown", db.walIndexState)
		}
	})

	t.Run("outside-wal-mode-nothing-is-tracked", func(t *testing.T) {
		db, err := Create(filepath.Join(t.TempDir(), "r25nowal.musq"))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		defer db.Discard()
		if _, _, err := db.ExecArgs(`CREATE TABLE t(a)`, nil); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		if err := r25WalRead(t, db, `SELECT * FROM t`); err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		if db.walIndexState != walIndexNotOpened {
			t.Fatalf("wal-index state = %v outside WAL mode, want walIndexNotOpened", db.walIndexState)
		}
	})
}
