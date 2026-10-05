package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestAttachSharedMemMinedLongURIFilename tests ATTACH with memory mode and cache=shared.
func TestAttachSharedMemMinedLongURIFilename(t *testing.T) {
	db := memdbTestDB(t)
	defer db.Close()
	for _, s := range []string{
		`ATTACH printf('file:%09000x/x.db?mode=memory&cache=shared',1) AS aux1`,
		`CREATE TABLE aux1.t1(x,y)`,
		`INSERT INTO aux1.t1(x,y) VALUES(1,2),(3,4)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	got := memdbQueryRows(t, db, `SELECT * FROM aux1.t1`)
	want := [][]string{{"1", "2"}, {"3", "4"}}
	if len(got) != len(want) || got[0][0] != want[0][0] || got[0][1] != want[0][1] ||
		got[1][0] != want[1][0] || got[1][1] != want[1][1] {
		t.Fatalf("SELECT * FROM aux1.t1 = %v, want %v", got, want)
	}
}

// TestAttachSharedMemAcrossConnectionsWhileBothOpen mirrors
// TestAttachMemdbSharedAcrossConnectionsWhileBothOpen exactly, for the OTHER
// shared in-RAM ATTACH form: "file:name?mode=memory&cache=shared" reuses the
// SAME process-wide registry (memdb_registry.go's sharedMemPathSentinel),
// under a namespaced key. Two INDEPENDENT *DB sessions (standing in for two
// independent connections) share one store's content while BOTH remain open;
// the first session's write is flushed via an explicit BEGIN/COMMIT so it is
// durable in the shared backing file before the second session reads it (see
// the memdb test's own doc comment for why that boundary matters).
func TestAttachSharedMemAcrossConnectionsWhileBothOpen(t *testing.T) {
	const uri = `file:r39b-shared-1?mode=memory&cache=shared`

	a := memdbTestDB(t)
	if err := a.Exec(`ATTACH '` + uri + `' AS auxA`); err != nil {
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

	// b attaches the SAME URI from an entirely independent *DB while a is
	// still open (refcount now 2): it must see a's already-committed row.
	b := memdbTestDB(t)
	if err := b.Exec(`ATTACH '` + uri + `' AS auxB`); err != nil {
		t.Fatalf("b: ATTACH: %v", err)
	}
	if got := memdbQueryRows(t, b, `SELECT x FROM auxB.t`); len(got) != 1 || got[0][0] != "42" {
		t.Fatalf("b: SELECT x FROM auxB.t = %v, want [[42]] -- shared cache=shared content not visible across connections", got)
	}

	// a closes (refcount 2 -> 1); b, still open, must be unaffected.
	if err := a.Close(); err != nil {
		t.Fatalf("a.Close: %v", err)
	}
	if got := memdbQueryRows(t, b, `SELECT x FROM auxB.t`); len(got) != 1 || got[0][0] != "42" {
		t.Fatalf("after a closed: SELECT x FROM auxB.t = %v, want [[42]] -- store freed too early", got)
	}

	// b closes too (refcount 1 -> 0): the store is freed, so a THIRD,
	// independent session attaching the identical URI gets a genuinely
	// FRESH, empty store -- not the content the first two sessions wrote.
	if err := b.Close(); err != nil {
		t.Fatalf("b.Close: %v", err)
	}
	c := memdbTestDB(t)
	if err := c.Exec(`ATTACH '` + uri + `' AS auxC`); err != nil {
		t.Fatalf("c: ATTACH: %v", err)
	}
	if got := memdbQueryRows(t, c, `SELECT count(*) FROM auxC.sqlite_master WHERE name='t'`); len(got) != 1 || got[0][0] != "0" {
		t.Fatalf("c (after both prior handles closed): sqlite_master count = %v, want [[0]] -- store was not freed on last close", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("c.Close: %v", err)
	}
}

// TestAttachSharedMemEmptyPathStaysPrivate pins a review-caught wrong-answer
// bug: an EMPTY URI path ("file:?mode=memory&cache=shared") is NEVER a
// shared-cache candidate in C SQLite, regardless of cache=shared --
// btree.c:2594's own precondition requires isTempDb==0, and isTempDb is
// EXACTLY "zFilename==0 || zFilename[0]==0" (btree.c:2548). Two independent
// sessions each ATTACHing the identical empty-path URI must NOT see each
// other's content -- unlike TestAttachSharedMemAcrossConnectionsWhileBothOpen's
// NAMED case just above, which correctly does share.
func TestAttachSharedMemEmptyPathStaysPrivate(t *testing.T) {
	const uri = `file:?mode=memory&cache=shared`

	a := memdbTestDB(t)
	defer a.Close()
	if err := a.Exec(`ATTACH '` + uri + `' AS auxA`); err != nil {
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

	b := memdbTestDB(t)
	defer b.Close()
	if err := b.Exec(`ATTACH '` + uri + `' AS auxB`); err != nil {
		t.Fatalf("b: ATTACH: %v", err)
	}
	if got := memdbQueryRows(t, b, `SELECT count(*) FROM auxB.sqlite_master WHERE name='t'`); len(got) != 1 || got[0][0] != "0" {
		t.Fatalf("b: sqlite_master count = %v, want [[0]] -- empty-path cache=shared wrongly shared a's content", got)
	}
}

// TestAttachSharedMemJournalModeReadsMemory mirrors
// TestAttachMemdbJournalModeReadsMemory: a cache=shared attachment's
// PRAGMA journal_mode reads back "memory", and a mode= set on it is silently
// ignored, exactly like an ordinary ATTACH ':memory:' or vfs=memdb one.
func TestAttachSharedMemJournalModeReadsMemory(t *testing.T) {
	db := memdbTestDB(t)
	if err := db.Exec(`ATTACH 'file:r39b-jm?mode=memory&cache=shared' AS aux`); err != nil {
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

// TestAttachSharedMemSameSessionSecondAliasIsAlreadyAttached pins real
// SQLite's OWN answer, which this gate used to have backwards. Two ATTACHes of
// one cache=shared name inside ONE connection would be physically one BtShared,
// and sqlite3BtreeOpen refuses to hand the same one to a connection twice
// (btree.c:2631-2640's "if( pExisting && pExisting->pBt==pBt ) return
// SQLITE_CONSTRAINT"), which attachFunc reports as "database is already
// attached" (attach.c:200-202). Measured against the 3.53.3 oracle: the second
// ATTACH errors and aux2 does not exist afterwards.
//
// The gate previously asserted an errVDBEUnsupported decline, on the theory
// that C SQLite served two aliases with immediate cross-alias visibility.
// It serves no second alias at all, so this is an UPGRADE of the assertion, not
// a relaxation: the engine now answers what C answers.
func TestAttachSharedMemSameSessionSecondAliasIsAlreadyAttached(t *testing.T) {
	db := memdbTestDB(t)
	if err := db.Exec(`ATTACH 'file:r39b-alias-1?mode=memory&cache=shared' AS aux1`); err != nil {
		t.Fatalf("first ATTACH: %v", err)
	}
	err := db.Exec(`ATTACH 'file:r39b-alias-1?mode=memory&cache=shared' AS aux2`)
	if err == nil {
		t.Fatal("second ATTACH of the same cache=shared name in one session: got no error, want an error")
	}
	if !strings.Contains(err.Error(), "database is already attached") {
		t.Errorf("second ATTACH error = %v, want \"database is already attached\"", err)
	}
	if errors.Is(err, errVDBEUnsupported) {
		t.Errorf("second ATTACH error = %v, want a C SQLite error, not a decline", err)
	}
	// The decline must not have leaked a registry reference: aux1 must still
	// work normally afterward.
	if err := db.Exec(`CREATE TABLE aux1.t(x)`); err != nil {
		t.Fatalf("CREATE TABLE aux1.t: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAttachSharedCacheOnRealFileDeclined pins that cache=shared combined
// with anything OTHER than mode=memory stays declined: a real FILE's shared
// page cache (two connections reading/writing through ONE BtShared/pager,
// btree.c:2594-2649) is a different feature this engine's one-pager-per-
// attachment design has no counterpart for at all.
func TestAttachSharedCacheOnRealFileDeclined(t *testing.T) {
	db := memdbTestDB(t)
	defer db.Close()
	err := db.Exec(`ATTACH 'file:` + filepath.Join(t.TempDir(), "realfile.db") + `?cache=shared' AS aux`)
	if err == nil {
		t.Fatal("cache=shared on a real file: got no error, want a decline")
	}
	if !errors.Is(err, errVDBEUnsupported) {
		t.Errorf("cache=shared on a real file error = %v, want errVDBEUnsupported", err)
	}
}

// TestAttachCachePrivateIsNoOp pins that cache=private -- the default anyway
// -- is accepted as the verified no-op it already was before "cache" became a
// recognized parameter key (e_uri.test, uri.test).
func TestAttachCachePrivateIsNoOp(t *testing.T) {
	db := memdbTestDB(t)
	defer db.Close()
	if err := db.Exec(`ATTACH 'file:` + filepath.Join(t.TempDir(), "private.db") + `?cache=private' AS aux`); err != nil {
		t.Fatalf("cache=private: %v", err)
	}
	if err := db.Exec(`CREATE TABLE aux.t(x)`); err != nil {
		t.Fatalf("CREATE TABLE aux.t: %v", err)
	}
}

// TestAttachCacheBogusValueDeclinesWithOracleMessage pins the exact oracle
// wording (main.c:3294, zModeType="cache") for a cache= value that is neither
// "shared" nor "private" -- not a decline, this engine agrees with the oracle
// here, the same "no such access mode" precedent mode='s own switch uses.
func TestAttachCacheBogusValueDeclinesWithOracleMessage(t *testing.T) {
	db := memdbTestDB(t)
	defer db.Close()
	err := db.Exec(`ATTACH 'file:` + filepath.Join(t.TempDir(), "bogus.db") + `?cache=bogus' AS aux`)
	if err == nil {
		t.Fatal("cache=bogus: got no error, want an error")
	}
	if got, want := err.Error(), "no such cache mode: bogus"; !strings.Contains(got, want) {
		t.Errorf("cache=bogus error = %q, want to contain %q", got, want)
	}
}
