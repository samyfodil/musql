// This file gates ATTACH's interaction with PRAGMA locking_mode: a database
// attached while the connection's DEFAULT locking mode is exclusive really
// inherits that mode (execAttach's own citation of attach.c:215/pager.c:7374-
// 7384/5228/5405), and the bare "=normal" setter really reaches every
// attachment too, not just main (execLockingMode's citation of pragma.c:700-
// 714). Mirrors ~/.cache/musql/sqlite-353/test/exclusive.test lines 78-155
// (do_test exclusive-1.7 through exclusive-1.99, mined as this repo's
// exclusive.test#0), replayed against the exact answers C SQLite gave
// (mattn/go-sqlite3 3.53.3 -- see this bucket's investigation for the full
// oracle transcript). One connection, three ATTACHes, no second connection.
package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// attachedLockingModeOf reads back the locking_mode of the ATTACHed database
// named q, the way a routed "PRAGMA q.locking_mode" getter would answer it.
//
// There is no PUBLIC path that answers this today: the EXEC side
// (execRoutedToAttached, which "PRAGMA aux.locking_mode" is routed through)
// discards its row exactly like the unqualified form does -- see
// execLockingMode's own doc comment, "The row this produces is discarded on
// the exec path" -- and the QUERY side (ReadOnlyPager.queryPragmaStmt) has no
// general cross-database routing for this pragma name (only table_list and
// foreign_key_check are routed there). So this reads ad.pager directly:
// exactly the state SnapshotPager copies onto it after every routed write
// (refreshAttachedWriteReaders + writer.go's SnapshotPager, "p.lockingMain =
// db.lockingMain"), which is what a real getter would answer from if one
// existed, just without the (currently nonexistent) routing on top.
func attachedLockingModeOf(t *testing.T, db *Session, q string) string {
	t.Helper()
	ad := db.attachedNamed(q)
	if ad == nil {
		t.Fatalf("%s is not attached", q)
	}
	if ad.pager == nil {
		t.Fatalf("%s has no read pager", q)
	}
	if ad.pager.lockingMain {
		return lockingModeExclusive
	}
	return lockingModeNormal
}

// queryCount runs a "SELECT count(*) FROM ..." query through pg and returns
// the single integer cell it answered.
func queryCount(pg *ReadOnlyPager, sqlText string) (int64, error) {
	_, rows, err := pg.Query(sqlText)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("%s: shape is not one row of one column: rows=%v", sqlText, rows)
	}
	return rows[0][0].I, nil
}

// lockingExecCount is queryCount through a fresh top-level snapshot of db
// (SnapshotPager -- the ordinary read-path entry point).
func lockingExecCount(db *Session, sqlText string) (int64, error) {
	pg, err := db.SnapshotPager()
	if err != nil {
		return 0, err
	}
	return queryCount(pg, sqlText)
}

func TestAttachExclusiveLockingModeInheritance(t *testing.T) {
	dir := t.TempDir()
	dbSess, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	db := dbSess
	defer dbSess.Discard()

	// exclusive-1.7: enter exclusive, then ATTACH -- aux inherits it
	// immediately, with no further statement needed to "activate" it.
	if got, err := lockingModeOf(t, db, `PRAGMA locking_mode = exclusive`); err != nil || got != "exclusive" {
		t.Fatalf("locking_mode=exclusive: %q, %v", got, err)
	}
	if err := lockingExec(db, `ATTACH '`+filepath.Join(dir, "aux.musq")+`' AS aux`); err != nil {
		t.Fatalf("ATTACH aux while exclusive: %v", err)
	}
	if got, err := lockingModeOf(t, db, `PRAGMA main.locking_mode`); err != nil || got != "exclusive" {
		t.Fatalf("main.locking_mode after ATTACH = %q, %v; want exclusive", got, err)
	}
	if got := attachedLockingModeOf(t, db, "aux"); got != "exclusive" {
		t.Fatalf("aux.locking_mode right after ATTACH = %q; want exclusive (exclusive-1.7)", got)
	}

	// exclusive-1.8: the QUALIFIED "main.locking_mode=normal" moves ONLY
	// main -- aux, and the connection default, stay exclusive.
	if got, err := lockingModeOf(t, db, `PRAGMA main.locking_mode = normal`); err != nil || got != "normal" {
		t.Fatalf("main.locking_mode=normal: %q, %v", got, err)
	}
	if got, err := lockingModeOf(t, db, `PRAGMA main.locking_mode`); err != nil || got != "normal" {
		t.Fatalf("main.locking_mode = %q, %v; want normal", got, err)
	}
	if got := attachedLockingModeOf(t, db, "aux"); got != "exclusive" {
		t.Fatalf("aux.locking_mode after main-only leave = %q; want exclusive (exclusive-1.8)", got)
	}

	// exclusive-1.9: the bare getter still answers the untouched DEFAULT,
	// unaffected by the qualified setter above.
	if got, err := lockingModeOf(t, db, `PRAGMA locking_mode`); err != nil || got != "exclusive" {
		t.Fatalf("bare getter = %q, %v; want exclusive (exclusive-1.9)", got, err)
	}

	// exclusive-1.10: a THIRD database attached NOW still comes back
	// exclusive -- it inherits db.lockingDefault, which main's own now-normal
	// mode never touched. This is exactly the case a naive db.lockingMain
	// gate gets wrong (the bug this investigation's live-oracle proof found).
	if err := lockingExec(db, `ATTACH '`+filepath.Join(dir, "aux2.musq")+`' AS aux2`); err != nil {
		t.Fatalf("ATTACH aux2: %v", err)
	}
	if got, err := lockingModeOf(t, db, `PRAGMA main.locking_mode`); err != nil || got != "normal" {
		t.Fatalf("main.locking_mode = %q, %v; want normal", got, err)
	}
	if got := attachedLockingModeOf(t, db, "aux"); got != "exclusive" {
		t.Fatalf("aux.locking_mode = %q; want exclusive (exclusive-1.10)", got)
	}
	if got := attachedLockingModeOf(t, db, "aux2"); got != "exclusive" {
		t.Fatalf("aux2.locking_mode right after ATTACH = %q; want exclusive (exclusive-1.10)", got)
	}

	// exclusive-1.11: the QUALIFIED "aux.locking_mode=normal" -- routed to
	// aux's own session via the pre-existing attachedPragmaScope/
	// execRoutedToAttached machinery -- moves ONLY aux. aux2 is unaffected.
	if err := lockingExec(db, `PRAGMA aux.locking_mode = normal`); err != nil {
		t.Fatalf("aux.locking_mode=normal: %v", err)
	}
	if got, err := lockingModeOf(t, db, `PRAGMA main.locking_mode`); err != nil || got != "normal" {
		t.Fatalf("main.locking_mode = %q, %v", got, err)
	}
	if got := attachedLockingModeOf(t, db, "aux"); got != "normal" {
		t.Fatalf("aux.locking_mode = %q; want normal (exclusive-1.11)", got)
	}
	if got := attachedLockingModeOf(t, db, "aux2"); got != "exclusive" {
		t.Fatalf("aux2.locking_mode = %q; want exclusive (exclusive-1.11)", got)
	}

	// exclusive-1.12: the BARE "locking_mode=normal" now reaches EVERY
	// attached database too, not just main -- aux2 (still exclusive) flips
	// to normal, aux (already normal) stays normal, temp (pinned) is
	// unaffected.
	if got, err := lockingModeOf(t, db, `PRAGMA locking_mode = normal`); err != nil || got != "normal" {
		t.Fatalf("bare locking_mode=normal: %q, %v", got, err)
	}
	if got, err := lockingModeOf(t, db, `PRAGMA main.locking_mode`); err != nil || got != "normal" {
		t.Fatalf("main.locking_mode = %q, %v", got, err)
	}
	if got, err := lockingModeOf(t, db, `PRAGMA temp.locking_mode`); err != nil || got != "exclusive" {
		t.Fatalf("temp.locking_mode = %q, %v; want exclusive (pinned)", got, err)
	}
	if got := attachedLockingModeOf(t, db, "aux"); got != "normal" {
		t.Fatalf("aux.locking_mode = %q; want normal", got)
	}
	if got := attachedLockingModeOf(t, db, "aux2"); got != "normal" {
		t.Fatalf("aux2.locking_mode = %q; want normal (exclusive-1.12)", got)
	}

	// exclusive-1.13: a FOURTH database attached now (default is normal
	// again) comes back normal too.
	if err := lockingExec(db, `ATTACH '`+filepath.Join(dir, "aux3.musq")+`' AS aux3`); err != nil {
		t.Fatalf("ATTACH aux3: %v", err)
	}
	if got, err := lockingModeOf(t, db, `PRAGMA main.locking_mode`); err != nil || got != "normal" {
		t.Fatalf("main.locking_mode = %q, %v", got, err)
	}
	if got, err := lockingModeOf(t, db, `PRAGMA temp.locking_mode`); err != nil || got != "exclusive" {
		t.Fatalf("temp.locking_mode = %q, %v", got, err)
	}
	for _, name := range []string{"aux", "aux2", "aux3"} {
		if got := attachedLockingModeOf(t, db, name); got != "normal" {
			t.Fatalf("%s.locking_mode = %q; want normal (exclusive-1.13)", name, got)
		}
	}

	// exclusive-1.99: DETACH everything cleanly.
	for _, name := range []string{"aux", "aux2", "aux3"} {
		if err := lockingExec(db, `DETACH `+name); err != nil {
			t.Fatalf("DETACH %s: %v", name, err)
		}
	}
	if len(db.attached) != 0 {
		t.Fatalf("db.attached not empty after detaching everything: %v", db.attached)
	}
}

//

func TestAttachExclusiveLockingModeHoldsAuxLock(t *testing.T) {
	orig := BusyTimeout
	BusyTimeout = 150 * time.Millisecond
	defer func() { BusyTimeout = orig }()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.musq")
	auxPath := filepath.Join(dir, "aux.musq")

	// Seed aux with a table via an ordinary, non-exclusive session first.
	seedSess, err := Create(mainPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seed := seedSess
	if err := lockingExec(seed, `ATTACH '`+auxPath+`' AS aux`); err != nil {
		t.Fatalf("seed ATTACH: %v", err)
	}
	if err := lockingExec(seed, `CREATE TABLE aux.t(x)`); err != nil {
		t.Fatalf("seed CREATE TABLE: %v", err)
	}
	if err := seedSess.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	// otherWriterCommits reports whether an independent write session can
	// commit an INSERT into aux's file right now.
	otherWriterCommits := func() error {
		w, err := OpenWrite(auxPath)
		if err != nil {
			return err
		}
		if err := lockingExec(w, `INSERT INTO t VALUES(1)`); err != nil {
			w.Discard()
			return err
		}
		return w.Close()
	}

	// Control: with nobody exclusive, the second writer gets in.
	if err := otherWriterCommits(); err != nil {
		t.Fatalf("control: a second writer must be able to commit into aux when nobody holds it exclusive: %v", err)
	}

	dbSess, err := OpenWrite(mainPath)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	db := dbSess
	defer dbSess.Discard()
	if got, err := lockingModeOf(t, db, `PRAGMA locking_mode = exclusive`); err != nil || got != "exclusive" {
		t.Fatalf("locking_mode=exclusive: %q, %v", got, err)
	}
	if err := lockingExec(db, `ATTACH '`+auxPath+`' AS aux`); err != nil {
		t.Fatalf("ATTACH aux while exclusive: %v", err)
	}

	err = otherWriterCommits()
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("a second writer committed into aux while db held it exclusive (err=%v); the lock is not really held", err)
	}
	// ...while a READER is still let in -- mirrors holdLockingModeLock's own
	// documented rung (SHARED, not EXCLUSIVE, until a WRITE actually lands).
	if r, err := Open(auxPath); err != nil {
		t.Fatalf("a reader must still be let in while aux is merely exclusive, not yet written through: %v", err)
	} else {
		r.Close()
	}

	// DETACH releases it: execDetach commits+closes ad.wdb unconditionally,
	// and (*DB).Close's own deferred db.f.Close() drops the OFD lock with it.
	if err := lockingExec(db, `DETACH aux`); err != nil {
		t.Fatalf("DETACH aux: %v", err)
	}
	if err := otherWriterCommits(); err != nil {
		t.Fatalf("a second writer must get in once aux is detached: %v", err)
	}
}

func TestAttachExclusiveLockingModeBusyConflictUndoesAttach(t *testing.T) {
	orig := BusyTimeout
	BusyTimeout = 150 * time.Millisecond
	defer func() { BusyTimeout = orig }()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.musq")
	auxPath := filepath.Join(dir, "aux.musq")

	// Materialize aux.db first via an ordinary, non-exclusive attach.
	seedSess, err := Create(mainPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seed := seedSess
	if err := lockingExec(seed, `ATTACH '`+auxPath+`' AS aux`); err != nil {
		t.Fatalf("seed ATTACH: %v", err)
	}
	if err := lockingExec(seed, `CREATE TABLE aux.t(x)`); err != nil {
		t.Fatalf("seed CREATE TABLE: %v", err)
	}
	if err := seedSess.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	// The conflicting lock has to be manufactured WHERE THIS FORMAT TAKES ONE.
	// It used to be an exclusive lock at the SQLite byte range
	// (filelock.SharedFirst/SharedSize) on the database file itself; a segment
	// database locks the whole of a SIDECAR file instead -- openSegmentLockFile,
	// segment_delta.go -- which is what lets a held lock survive the rename a
	// compaction does. Locking the old place conflicted with nothing.
	conflict, err := openSegmentLockFile(auxPath)
	if err != nil {
		t.Fatalf("open conflict fd: %v", err)
	}
	defer conflict.Close()
	if err := acquireLock(conflict, 0, 0, true, 0); err != nil {
		t.Fatalf("seed conflicting lock: %v", err)
	}

	dbSess, err := OpenWrite(mainPath)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	db := dbSess
	defer dbSess.Discard()
	if _, err := lockingModeOf(t, db, `PRAGMA locking_mode = exclusive`); err != nil {
		t.Fatalf("locking_mode=exclusive: %v", err)
	}
	if err := lockingExec(db, `ATTACH '`+auxPath+`' AS aux`); err == nil {
		t.Fatal("expected ATTACH to be declined: aux's file is already locked by another writer")
	}
	if ad := db.attachedNamed("aux"); ad != nil {
		t.Fatalf("a failed ATTACH must leave no half-open attachment behind, got %+v", ad)
	}

	// Release the conflict and confirm the identical statement now succeeds
	// -- proving the failed attempt left no stray state behind to interfere.
	if err := releaseLock(conflict, 0, 0); err != nil {
		t.Fatalf("release conflict: %v", err)
	}
	if err := lockingExec(db, `ATTACH '`+auxPath+`' AS aux`); err != nil {
		t.Fatalf("ATTACH after the conflict cleared: %v", err)
	}
	if got := attachedLockingModeOf(t, db, "aux"); got != "exclusive" {
		t.Fatalf("aux.locking_mode after the retried ATTACH = %q; want exclusive", got)
	}
}

// PRAGMA locking_mode=exclusive;
// ATTACH 'shared.db' AS a1;
// ATTACH 'shared.db' AS a2;   -- same file, second alias -- explicitly allowed
// DETACH a2;                  -- no write attempted anywhere in this session
func TestAttachExclusiveLockingModeAliasingDetachDoesNotSelfConflict(t *testing.T) {
	orig := BusyTimeout
	BusyTimeout = 150 * time.Millisecond
	defer func() { BusyTimeout = orig }()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.musq")
	sharedPath := filepath.Join(dir, "shared.musq")

	dbSess, err := Create(mainPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	db := dbSess
	defer dbSess.Discard()

	if got, err := lockingModeOf(t, db, `PRAGMA locking_mode = exclusive`); err != nil || got != "exclusive" {
		t.Fatalf("locking_mode=exclusive: %q, %v", got, err)
	}
	if err := lockingExec(db, `ATTACH '`+sharedPath+`' AS a1`); err != nil {
		t.Fatalf("ATTACH a1: %v", err)
	}
	if err := lockingExec(db, `ATTACH '`+sharedPath+`' AS a2`); err != nil {
		t.Fatalf("ATTACH a2 (same file as a1, second alias): %v", err)
	}
	if got := attachedLockingModeOf(t, db, "a1"); got != "exclusive" {
		t.Fatalf("a1.locking_mode = %q; want exclusive", got)
	}
	if got := attachedLockingModeOf(t, db, "a2"); got != "exclusive" {
		t.Fatalf("a2.locking_mode = %q; want exclusive", got)
	}

	start := time.Now()
	err = lockingExec(db, `DETACH a2`)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DETACH a2: %v (took %s; a self-inflicted BUSY against a1's own held lock, with no external contention)", err, elapsed)
	}
	if elapsed >= BusyTimeout {
		t.Fatalf("DETACH a2 took %s, at or past BusyTimeout (%s): it retried against a lock conflict instead of returning immediately", elapsed, BusyTimeout)
	}
	if ad := db.attachedNamed("a2"); ad != nil {
		t.Fatalf("a2 still attached after DETACH: %+v", ad)
	}

	// a1's own lock must have survived a2's detach untouched: an outside
	// writer on the shared file is still refused.
	otherWriterCommits := func() error {
		w, werr := OpenWrite(sharedPath)
		if werr != nil {
			return werr
		}
		if werr := lockingExec(w, `CREATE TABLE t(x)`); werr != nil {
			w.Discard()
			return werr
		}
		return w.Close()
	}
	if err := otherWriterCommits(); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second writer committed into the shared file after a2 alone was detached (err=%v); a1's own exclusive lock did not survive", err)
	}

	// DETACHing a1 too releases the file entirely.
	if err := lockingExec(db, `DETACH a1`); err != nil {
		t.Fatalf("DETACH a1: %v", err)
	}
	if err := otherWriterCommits(); err != nil {
		t.Fatalf("a second writer must get in once both aliases are detached: %v", err)
	}
}

func TestAttachExclusiveLockingModeAliasingRealWriteStillConflicts(t *testing.T) {
	orig := BusyTimeout
	BusyTimeout = 150 * time.Millisecond
	defer func() { BusyTimeout = orig }()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.musq")
	sharedPath := filepath.Join(dir, "shared.musq")

	// Seed the shared file with a table via an ordinary session first, same
	// as TestAttachExclusiveLockingModeHoldsAuxLock.
	seedSess, err := Create(mainPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seed := seedSess
	if err := lockingExec(seed, `ATTACH '`+sharedPath+`' AS a1`); err != nil {
		t.Fatalf("seed ATTACH: %v", err)
	}
	if err := lockingExec(seed, `CREATE TABLE a1.t(x)`); err != nil {
		t.Fatalf("seed CREATE TABLE: %v", err)
	}
	if err := seedSess.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	dbSess, err := OpenWrite(mainPath)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	db := dbSess
	defer dbSess.Discard()
	if got, err := lockingModeOf(t, db, `PRAGMA locking_mode = exclusive`); err != nil || got != "exclusive" {
		t.Fatalf("locking_mode=exclusive: %q, %v", got, err)
	}
	if err := lockingExec(db, `ATTACH '`+sharedPath+`' AS a1`); err != nil {
		t.Fatalf("ATTACH a1: %v", err)
	}
	if err := lockingExec(db, `ATTACH '`+sharedPath+`' AS a2`); err != nil {
		t.Fatalf("ATTACH a2 (same file as a1, second alias): %v", err)
	}

	// The INSERT itself must fail now -- C SQLite refuses it outright, it
	// never reaches a state where the row could land.
	err = lockingExec(db, `INSERT INTO a2.t VALUES(1)`)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("INSERT INTO a2.t = %v; want a BUSY conflict against a1's own held lock, refused immediately like C SQLite", err)
	}

	// The row must never have landed.
	if got, err := lockingExecCount(db, `SELECT count(*) FROM a2.t`); err != nil || got != 0 {
		t.Fatalf("count(*) FROM a2.t after the refused INSERT = %d, %v; want 0 (the write never applied)", got, err)
	}

	// Nothing was ever written, so fa59b40's anyPageChanged skip still
	// applies: a following DETACH of both aliases succeeds cleanly, with no
	// residual lock conflict from the refused write attempt.
	if err := lockingExec(db, `DETACH a2`); err != nil {
		t.Fatalf("DETACH a2 after the refused write: %v", err)
	}
	if err := lockingExec(db, `DETACH a1`); err != nil {
		t.Fatalf("DETACH a1: %v", err)
	}
}

func TestAttachExclusiveLockingModeAliasingWriteRefusedEvenWithoutExplicitDetach(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.musq")
	sharedPath := filepath.Join(dir, "shared.musq")

	seedSess, err := Create(mainPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seed := seedSess
	if err := lockingExec(seed, `ATTACH '`+sharedPath+`' AS a1`); err != nil {
		t.Fatalf("seed ATTACH: %v", err)
	}
	if err := lockingExec(seed, `CREATE TABLE a1.t(x)`); err != nil {
		t.Fatalf("seed CREATE TABLE: %v", err)
	}
	if err := seedSess.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	dbSess, err := OpenWrite(mainPath)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	db := dbSess
	if got, err := lockingModeOf(t, db, `PRAGMA locking_mode = exclusive`); err != nil || got != "exclusive" {
		t.Fatalf("locking_mode=exclusive: %q, %v", got, err)
	}
	if err := lockingExec(db, `ATTACH '`+sharedPath+`' AS a1`); err != nil {
		t.Fatalf("ATTACH a1: %v", err)
	}
	if err := lockingExec(db, `ATTACH '`+sharedPath+`' AS a2`); err != nil {
		t.Fatalf("ATTACH a2 (same file as a1, second alias): %v", err)
	}

	err = lockingExec(db, `INSERT INTO a2.t VALUES(1)`)
	if !errors.Is(err, ErrBusy) {
		dbSess.Discard()
		t.Fatalf("INSERT INTO a2.t = %v; want a BUSY conflict, refused before Close is ever reached", err)
	}

	// No explicit DETACH anywhere -- close the whole session directly, the
	// exact shape that used to mask the wrong write entirely.
	if err := dbSess.Close(); err != nil {
		t.Fatalf("Close after the refused write: %v", err)
	}

	fresh, err := Open(sharedPath)
	if err != nil {
		t.Fatalf("reopening %s: %v", sharedPath, err)
	}
	defer fresh.Close()
	if got, err := queryCount(fresh, `SELECT count(*) FROM t`); err != nil || got != 0 {
		t.Fatalf("count(*) FROM t in %s after Close = %d, %v; want 0 (the refused write must never land, through any closing path)", sharedPath, got, err)
	}
}

func TestAttachExclusiveLockingModeAliasingPureReadNotConflicted(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.musq")
	sharedPath := filepath.Join(dir, "shared.musq")

	seedSess, err := Create(mainPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seed := seedSess
	if err := lockingExec(seed, `ATTACH '`+sharedPath+`' AS a1`); err != nil {
		t.Fatalf("seed ATTACH: %v", err)
	}
	if err := lockingExec(seed, `CREATE TABLE a1.t(x)`); err != nil {
		t.Fatalf("seed CREATE TABLE: %v", err)
	}
	if err := seedSess.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	dbSess, err := OpenWrite(mainPath)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	db := dbSess
	if got, err := lockingModeOf(t, db, `PRAGMA locking_mode = exclusive`); err != nil || got != "exclusive" {
		t.Fatalf("locking_mode=exclusive: %q, %v", got, err)
	}
	if err := lockingExec(db, `ATTACH '`+sharedPath+`' AS a1`); err != nil {
		t.Fatalf("ATTACH a1: %v", err)
	}
	if err := lockingExec(db, `ATTACH '`+sharedPath+`' AS a2`); err != nil {
		t.Fatalf("ATTACH a2 (same file as a1, second alias, a1 never writes): %v", err)
	}

	// The exact counter-example: a1 holds only its ATTACH-time SHARED lock,
	// never having written anything. A pure read through a2 must succeed.
	if err := lockingExec(db, `PRAGMA a2.integrity_check`); err != nil {
		t.Fatalf("PRAGMA a2.integrity_check (pure read, no mutation) = %v; want success, like C SQLite (SHARED-vs-SHARED never conflicts)", err)
	}

	// Same shape, another never-a-write pragma form.
	if err := lockingExec(db, `PRAGMA a2.quick_check`); err != nil {
		t.Fatalf("PRAGMA a2.quick_check (pure read) = %v; want success", err)
	}

	// A genuine write through the SAME alias must still be refused: the fix
	// narrows the guard, it does not remove it.
	err = lockingExec(db, `PRAGMA a2.user_version=5`)
	if !errors.Is(err, ErrBusy) {
		dbSess.Discard()
		t.Fatalf("PRAGMA a2.user_version=5 (genuine write) = %v; want a BUSY conflict against a1's held lock, matching C SQLite", err)
	}

	// No explicit DETACH -- close directly, then reopen sharedPath fresh
	// (mirroring TestAttachExclusiveLockingModeAliasingWriteRefusedEvenWithoutExplicitDetach)
	// and confirm the refused setter never actually applied.
	if err := dbSess.Close(); err != nil {
		t.Fatalf("Close after the refused setter: %v", err)
	}
	fresh, err := Open(sharedPath)
	if err != nil {
		t.Fatalf("reopening %s: %v", sharedPath, err)
	}
	defer fresh.Close()
	if got, err := queryCount(fresh, `PRAGMA user_version`); err != nil || got != 0 {
		t.Fatalf("PRAGMA user_version in %s after Close = %d, %v; want 0 (the refused setter must never land)", sharedPath, got, err)
	}
}
