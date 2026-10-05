package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memdbTestDB creates a fresh main database in a temp dir.
func memdbTestDB(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// memdbQueryRows runs a SELECT and returns rows as flattened text for assertions.
func memdbQueryRows(t *testing.T, db *Session, q string) [][]string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("%s: SnapshotPager: %v", q, err)
	}
	defer p.Close()
	p.SetAttachedReadersForTest(db)
	_, rows, qerr := p.QueryArgs(q, nil)
	if qerr != nil {
		t.Fatalf("%s: %v", q, qerr)
	}
	out := make([][]string, len(rows))
	for i, r := range rows {
		row := make([]string, len(r))
		for j, v := range r {
			row[j] = valueToText(v)
		}
		out[i] = row
	}
	return out
}

// TestAttachMemdbSharedAcrossConnectionsWhileBothOpen tests that two
// independent sessions can share a memdb store and see each other's commits.
func TestAttachMemdbSharedAcrossConnectionsWhileBothOpen(t *testing.T) {
	const memdbName = "file:/r39a-shared-1?vfs=memdb"

	a := memdbTestDB(t)
	if err := a.Exec(`ATTACH '` + memdbName + `' AS auxA`); err != nil {
		t.Fatalf("a: ATTACH: %v", err)
	}
	for _, s := range []string{
		`BEGIN`,
		`CREATE TABLE auxA.t(x)`,
		`INSERT INTO auxA.t VALUES(42)`,
		`COMMIT`,
	} {
		if err := a.Exec(s); err != nil {
			t.Fatalf("a: %s: %v", s, err)
		}
	}

	// b attaches the SAME name from an entirely independent *DB while a is
	// still open (refcount now 2): it must see a's already-committed row.
	b := memdbTestDB(t)
	if err := b.Exec(`ATTACH '` + memdbName + `' AS auxB`); err != nil {
		t.Fatalf("b: ATTACH: %v", err)
	}
	if got := memdbQueryRows(t, b, `SELECT x FROM auxB.t`); len(got) != 1 || got[0][0] != "42" {
		t.Fatalf("b: SELECT x FROM auxB.t = %v, want [[42]] -- shared memdb content not visible across connections", got)
	}

	// a closes (refcount 2 -> 1); b, still open, must be unaffected --
	// matches memdbClose's own refcount keep-alive (memdb.c:210-247),
	// confirmed directly against the oracle for this exact shape.
	if err := a.Close(); err != nil {
		t.Fatalf("a.Close: %v", err)
	}
	if got := memdbQueryRows(t, b, `SELECT x FROM auxB.t`); len(got) != 1 || got[0][0] != "42" {
		t.Fatalf("after a closed: SELECT x FROM auxB.t = %v, want [[42]] -- store freed too early", got)
	}

	// b closes too (refcount 1 -> 0): the store is freed, so a THIRD,
	// independent session attaching the identical name gets a genuinely
	// FRESH, empty store -- not the content the first two sessions wrote.
	if err := b.Close(); err != nil {
		t.Fatalf("b.Close: %v", err)
	}
	c := memdbTestDB(t)
	if err := c.Exec(`ATTACH '` + memdbName + `' AS auxC`); err != nil {
		t.Fatalf("c: ATTACH: %v", err)
	}
	if got := memdbQueryRows(t, c, `SELECT count(*) FROM auxC.sqlite_master WHERE name='t'`); len(got) != 1 || got[0][0] != "0" {
		t.Fatalf("c (after both prior handles closed): sqlite_master count = %v, want [[0]] -- store was not freed on last close", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("c.Close: %v", err)
	}
}

// TestAttachMemdbUnsharedWithoutLeadingSlash tests that memdb names without
// a leading slash are private to each session.
func TestAttachMemdbUnsharedWithoutLeadingSlash(t *testing.T) {
	const name = "file:r39a-unshared-1?vfs=memdb"

	a := memdbTestDB(t)
	for _, s := range []string{
		`ATTACH '` + name + `' AS auxA`,
		`CREATE TABLE auxA.t(x)`,
		`INSERT INTO auxA.t VALUES(1)`,
	} {
		if err := a.Exec(s); err != nil {
			t.Fatalf("a: %s: %v", s, err)
		}
	}

	b := memdbTestDB(t)
	if err := b.Exec(`ATTACH '` + name + `' AS auxB`); err != nil {
		t.Fatalf("b: ATTACH: %v", err)
	}
	if got := memdbQueryRows(t, b, `SELECT count(*) FROM auxB.sqlite_master WHERE name='t'`); len(got) != 1 || got[0][0] != "0" {
		t.Fatalf("b: unshared memdb leaked content across connections: sqlite_master count = %v, want [[0]]", got)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAttachMemdbJournalModeReadsMemory tests that a memdb attachment
// reports journal_mode as "memory".
func TestAttachMemdbJournalModeReadsMemory(t *testing.T) {
	db := memdbTestDB(t)
	if err := db.Exec(`ATTACH 'file:/r39a-jm?vfs=memdb' AS aux`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`PRAGMA aux.journal_mode = wal`, `PRAGMA aux.journal_mode = truncate`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v -- C SQLite silently ignores it on a memory-backed database", s, err)
		}
	}
	w := db.attachedNamed("aux").wdb
	if w == nil {
		t.Fatal("no write session was opened for the routed pragma")
	}
	p, err := w.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cols, rows, qerr := p.QueryArgs(`PRAGMA journal_mode`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(cols) != 1 || len(rows) != 1 || valueToText(rows[0][0]) != journalModeMemory {
		t.Errorf("aux journal_mode = cols=%v rows=%v, want one row [%s]", cols, rows, journalModeMemory)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAttachMemdbSameSessionSecondAlias tests that two aliases of one memdb
// in the same session follow locking rules: autocommit writes are visible,
// but reads during uncommitted writes are blocked.
func TestAttachMemdbSameSessionSecondAlias(t *testing.T) {
	db := memdbTestDB(t)
	defer db.Close()
	exec := func(s string) error { return db.Exec(s) }
	count := func(tbl string) (int64, error) {
		p, err := db.SnapshotPager()
		if err != nil {
			return 0, err
		}
		_, rows, qerr := p.Query("SELECT count(*) FROM " + tbl)
		if qerr != nil {
			return 0, qerr
		}
		return rows[0][0].I, nil
	}
	for _, s := range []string{
		`ATTACH 'file:/r39a-alias-1?vfs=memdb' AS aux1`,
		`ATTACH 'file:/r39a-alias-1?vfs=memdb' AS aux2`,
		`CREATE TABLE aux1.t(x)`,
		`INSERT INTO aux1.t VALUES(1)`,
	} {
		if err := exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if n, err := count("aux2.t"); err != nil || n != 1 {
		t.Fatalf("autocommit write through aux1, read through aux2: n=%d err=%v; want 1", n, err)
	}

	// A sibling that has NOT read in this transaction is refused.
	for _, s := range []string{`BEGIN`, `INSERT INTO aux1.t VALUES(2)`} {
		if err := exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// The read path reports the busy by its TEXT (a query error is re-wrapped
	// on its way out), which is also what the harness compares.
	if _, err := count("aux2.t"); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("read through aux2 while aux1 holds the write lock: err=%v; want \"database is locked\"", err)
	}
	if err := exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}

	// A sibling that read FIRST holds SHARED: stale read, then a refused COMMIT.
	if err := exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	if n, err := count("aux2.t"); err != nil || n != 1 {
		t.Fatalf("aux2 read at the start of the transaction: n=%d err=%v; want 1", n, err)
	}
	if err := exec(`INSERT INTO aux1.t VALUES(2)`); err != nil {
		t.Fatal(err)
	}
	if n, err := count("aux2.t"); err != nil || n != 1 {
		t.Errorf("aux2 after aux1's uncommitted write: n=%d err=%v; want the stale 1", n, err)
	}
	if err := exec(`COMMIT`); err == nil || !errors.Is(err, ErrBusy) {
		t.Errorf("COMMIT while aux2 holds SHARED: err=%v; want ErrBusy", err)
	}
	if err := exec(`ROLLBACK`); err != nil {
		t.Errorf("ROLLBACK after the refused COMMIT: %v; want success (the transaction stays open)", err)
	}
	if n, err := count("aux1.t"); err != nil || n != 1 {
		t.Errorf("aux1 after ROLLBACK: n=%d err=%v; want 1", n, err)
	}
}

// TestAttachMemdbAnalyzeUnderExclusiveLocking tests that ANALYZE under
// exclusive locking meets the expected lock contention.
func TestAttachMemdbAnalyzeUnderExclusiveLocking(t *testing.T) {
	db := memdbTestDB(t)
	defer db.Close()
	for _, s := range []string{
		`PRAGMA locking_mode=EXCLUSIVE`,
		`BEGIN`,
		`ATTACH 'file:/r39a-fts5misc-14?vfs=memdb' AS aux1`,
		`ATTACH 'file:/r39a-fts5misc-14?vfs=memdb' AS aux2`,
		`CREATE TABLE t1(x)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, s := range []string{`ANALYZE`, `COMMIT`, `COMMIT`} {
		if err := db.Exec(s); err == nil || !errors.Is(err, ErrBusy) {
			t.Errorf("%s: err=%v; want ErrBusy (\"database is locked\")", s, err)
		}
	}
	if err := db.Exec(`ROLLBACK`); err != nil {
		t.Errorf("ROLLBACK: %v; want success", err)
	}
}

// TestAttachMemdbModeCombinationDeclined tests that vfs=memdb with mode= declines.
func TestAttachMemdbModeCombinationDeclined(t *testing.T) {
	db := memdbTestDB(t)
	defer db.Close()
	err := db.Exec(`ATTACH 'file:/r39a-mode-1?vfs=memdb&mode=ro' AS aux`)
	if err == nil {
		t.Fatal("vfs=memdb&mode=ro: got no error, want a decline")
	}
	if !errors.Is(err, errVDBEUnsupported) {
		t.Errorf("vfs=memdb&mode=ro error = %v, want errVDBEUnsupported", err)
	}
}

// TestMemdbRegistryRefcount tests refcount management of memdb backing files.
func TestMemdbRegistryRefcount(t *testing.T) {
	const name = "/r39a-registry-direct"
	p1, err := memdbAcquire(name)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := memdbAcquire(name)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("memdbAcquire(%q) twice returned different paths: %q vs %q", name, p1, p2)
	}
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("backing file missing after two acquires: %v", err)
	}
	memdbRelease(name)
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("backing file removed after only ONE of two releases: %v", err)
	}
	memdbRelease(name)
	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Fatalf("backing file still present after the LAST release (err=%v), want it removed", err)
	}
	memdbRelease(name)
}

// TestAttachedDBCleanupOnFailedAttachReleasesMemdbReference tests that
// failed ATTACHes release their memdb references.
func TestAttachedDBCleanupOnFailedAttachReleasesMemdbReference(t *testing.T) {
	const name = "/r39a-cleanup-direct"
	path, err := memdbAcquire(name)
	if err != nil {
		t.Fatal(err)
	}
	ad := &attachedDB{isMemdb: true, memdbName: name, path: path}
	ad.cleanupOnFailedAttach()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("backing file still present after cleanupOnFailedAttach released the only reference (err=%v), want it removed", err)
	}
}
