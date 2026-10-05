// This file is the lock machinery: the segment format's own lock
// (segment_delta.go's withSegmentWriteLock, and holdLockingModeLock below for
// "PRAGMA locking_mode=exclusive"). Both are OFD byte-range locks through
// internal/filelock, polled with a timeout rather than blocked on.
package engine

import (
	"errors"
	"os"
	"time"

	"github.com/samyfodil/musql/internal/filelock"
)

// ErrBusy mirrors SQLite's SQLITE_BUSY: returned when a conflicting lock (the
// segment lock another writer holds, or one held for locking_mode=exclusive)
// could not be acquired before the busy timeout elapsed. Callers should treat this the same way a database/sql driver
// treats SQLITE_BUSY -- retry the whole transaction later, don't assume
// anything was written.
var ErrBusy = errors.New("engine: database is locked (SQLITE_BUSY)")

// BusyTimeout bounds how long acquireLock retries a conflicting lock before
// giving up with ErrBusy, mirroring PRAGMA busy_timeout. It is a package variable (not a const),
// exported so tests can shrink it (e.g. to keep a "writer waits for a slow
// reader" scenario from taking the full default) or exercise the ErrBusy
// timeout path directly; ordinary callers never need to change it.
var BusyTimeout = 5 * time.Second

// busyRetryInterval is the fixed poll interval between non-blocking lock
// attempts. A fixed short interval keeps latency negligible in the uncontended case.
var busyRetryInterval = 5 * time.Millisecond

// acquireLock repeatedly attempts a non-blocking OFD/LockFileEx byte-range
// lock on [start, start+length) of f until it succeeds or timeout elapses.
// This is deliberately never a blocking OS-level lock acquisition (e.g.
// F_SETLKW): a real blocking wait risks an unrecoverable same-process
// deadlock (two goroutines, one connection each, both waiting on the OS),
// and gives no way to bound how long a caller waits. Polling with a
// timeout gives us both SQLITE_BUSY semantics (surface "busy" rather than
// hang forever) and a way to widen or shrink the wait window per call site.
func acquireLock(f *os.File, start, length int64, exclusive bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := filelock.LockRange(f, start, length, exclusive)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrBusy
		}
		time.Sleep(busyRetryInterval)
	}
}

// releaseLock releases the byte-range lock on [start, start+length) of f.
// Like filelock.UnlockRange (fcntl F_UNLCK / Win32 UnlockFileEx), releasing
// a range not currently held by this file description is a harmless no-op,
// so callers may release defensively without tracking exactly which locks
// they still hold.
func releaseLock(f *os.File, start, length int64) error {
	return filelock.UnlockRange(f, start, length)
}

// holdLockingModeLock acquires and holds a SHARED lock for "PRAGMA
// locking_mode=exclusive", blocking new WRITE operations but allowing readers.
func (db *DB) holdLockingModeLock() error {
	if db.lockingHeld {
		return nil
	}
	// Use the same lock that a commit takes, held rather than released. This is an
	// OFD byte-range lock on the sidecar lock file so it survives segment rewrites.
	f, err := openSegmentLockFile(db.path)
	if err != nil {
		return err
	}
	// SHARED, not exclusive: exclusive mode blocks writers but allows readers until
	// a write lands. Testing caught that EXCLUSIVE would block readers too.
	if lerr := acquireLock(f, segStateByte, 1, false, BusyTimeout); lerr != nil {
		f.Close()
		return segmentLockBusy(lerr, "write")
	}
	db.segLockFile = f
	db.noteLockingHeld(true)
	return nil
}

// noteLockingHeld records whether this session holds the locking-mode lock.
func (db *DB) noteLockingHeld(held bool) {
	db.lockingHeld = held
}

// releaseSegmentLockingModeLock drops the held lock a segment-backed
// session took for "PRAGMA locking_mode=exclusive". Idempotent, because it is
// reached from the pragma's own "=normal" arm, from Close and from Discard.
func (db *DB) releaseSegmentLockingModeLock() {
	if db.segLockFile == nil {
		return
	}
	releaseLock(db.segLockFile, segStateByte, 1)
	db.segLockFile.Close()
	db.segLockFile = nil
	db.noteLockingHeld(false)
}
