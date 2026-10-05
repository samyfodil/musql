//go:build windows

package filelock

import (
	"errors"
	"math"
	"os"
	"runtime"
	"sync"
	"weak"

	"golang.org/x/sys/windows"
)

// POSIX byte-range locks on top of LockFileEx.
//
// Callers rely on POSIX semantics, and LockFileEx differs in three ways:
// re-locking a range this file already holds FAILS (POSIX: no-op); a shared
// lock cannot be upgraded in place (POSIX: converts); and an unlock must name
// exactly a range that was locked (POSIX: any sub-range, splitting what is
// held). So each file's held locks are tracked here, and every request is
// turned into exact LockFileEx/UnlockFileEx calls.
//
// Locks are keyed by the *os.File, weakly: a closed and collected file's locks
// are gone in the OS too, and its entry with them.

type held struct {
	start, end int64 // [start, end); end is math.MaxInt64 for "to the end"
	excl       bool
}

var (
	mu    sync.Mutex
	locks = map[weak.Pointer[os.File]][]held{}
)

func keyOf(f *os.File) weak.Pointer[os.File] {
	k := weak.Make(f)
	if _, ok := locks[k]; !ok {
		locks[k] = nil
		runtime.AddCleanup(f, func(k weak.Pointer[os.File]) {
			mu.Lock()
			delete(locks, k)
			mu.Unlock()
		}, k)
	}
	return k
}

func span(start, length int64) held {
	if length == 0 || start > math.MaxInt64-length {
		return held{start: start, end: math.MaxInt64}
	}
	return held{start: start, end: start + length}
}

func (h held) overlaps(o held) bool { return h.start < o.end && o.start < h.end }

// winRange is h as LockFileEx counts it: an offset and a length, the open-ended
// span as the largest length.
func winRange(h held) (ov windows.Overlapped, lo, hi uint32) {
	ov.Offset, ov.OffsetHigh = uint32(h.start), uint32(h.start>>32)
	if h.end == math.MaxInt64 {
		return ov, ^uint32(0), ^uint32(0)
	}
	n := h.end - h.start
	return ov, uint32(n), uint32(n >> 32)
}

// rawLock takes exactly h; ok=false means another file holds part of it.
func rawLock(f *os.File, h held) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if h.excl {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	ov, lo, hi := winRange(h)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, lo, hi, &ov); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func rawUnlock(f *os.File, h held) error {
	ov, lo, hi := winRange(h)
	err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lo, hi, &ov)
	if errors.Is(err, windows.ERROR_NOT_LOCKED) {
		return nil
	}
	return err
}

// outside returns the parts of h not covered by cut.
func outside(h, cut held) []held {
	var out []held
	if h.start < cut.start {
		out = append(out, held{h.start, cut.start, h.excl})
	}
	if cut.end < h.end {
		out = append(out, held{cut.end, h.end, h.excl})
	}
	return out
}

// LockRange takes a non-blocking byte-range lock with POSIX semantics (length 0
// is "to the end of the file"). ok=false (err=nil) means the range is locked by
// another file; a non-nil err is a real I/O error.
func LockRange(f *os.File, start, length int64, exclusive bool) (ok bool, err error) {
	mu.Lock()
	defer mu.Unlock()
	k := keyOf(f)
	req := span(start, length)
	req.excl = exclusive
	var over, rest []held
	for _, h := range locks[k] {
		if h.overlaps(req) {
			if h.start <= req.start && h.end >= req.end && (h.excl || !exclusive) {
				return true, nil // already held at least this strongly
			}
			over = append(over, h)
		} else {
			rest = append(rest, h)
		}
	}
	for _, h := range over {
		if err := rawUnlock(f, h); err != nil {
			return false, err
		}
	}
	ok, err = rawLock(f, req)
	if !ok || err != nil {
		for _, h := range over { // put back what this file held
			rawLock(f, h)
		}
		return ok, err
	}
	rest = append(rest, req)
	for _, h := range over {
		for _, p := range outside(h, req) {
			if pok, _ := rawLock(f, p); pok {
				rest = append(rest, p)
			}
		}
	}
	locks[k] = rest
	return true, nil
}

// UnlockRange releases [start, start+length) with POSIX semantics: any part of
// what this file holds, splitting a larger lock; nothing held is a no-op.
func UnlockRange(f *os.File, start, length int64) error {
	mu.Lock()
	defer mu.Unlock()
	k := keyOf(f)
	cut := span(start, length)
	var rest []held
	for _, h := range locks[k] {
		if !h.overlaps(cut) {
			rest = append(rest, h)
			continue
		}
		if err := rawUnlock(f, h); err != nil {
			locks[k] = append(rest, h)
			return err
		}
		for _, p := range outside(h, cut) {
			if pok, _ := rawLock(f, p); pok {
				rest = append(rest, p)
			}
		}
	}
	locks[k] = rest
	return nil
}
