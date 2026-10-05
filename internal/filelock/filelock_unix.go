//go:build linux || darwin

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// LockRange attempts a non-blocking OFD byte-range lock on [start, start+length).
// OFD locks are scoped to the file description, not the process, so multiple
// handles allow same-process multi-connection conflict. ok=false (err=nil) means
// held incompatibly (EAGAIN/EACCES); non-nil err is a real error.
func LockRange(f *os.File, start, length int64, exclusive bool) (ok bool, err error) {
	typ := int16(unix.F_RDLCK)
	if exclusive {
		typ = int16(unix.F_WRLCK)
	}

	lk := unix.Flock_t{
		Type:   typ,
		Whence: int16(unix.SEEK_SET),
		Start:  start,
		Len:    length,
	}

	if err := unix.FcntlFlock(f.Fd(), ofdSetlk, &lk); err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// UnlockRange releases the OFD byte-range lock. F_UNLCK always succeeds (no-op if unlocked).
func UnlockRange(f *os.File, start, length int64) error {
	lk := unix.Flock_t{
		Type:   int16(unix.F_UNLCK),
		Whence: int16(unix.SEEK_SET),
		Start:  start,
		Len:    length,
	}

	return unix.FcntlFlock(f.Fd(), ofdSetlk, &lk)
}
