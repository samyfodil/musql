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

	if err := setlk(f, &lk); err != nil {
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

	return setlk(f, &lk)
}

// setlk is F_OFD_SETLK, asked again up to einvalRetries times while it fails
// with EINVAL. macOS has returned EINVAL about once in 10^6 calls under
// contention, on a valid descriptor and a valid lock, and no call made to
// reproduce it has repeated it. A filesystem without OFD locks answers EINVAL
// every time, so it still fails. The error names the call, so a failure that
// reaches a caller says it came from the lock.
func setlk(f *os.File, lk *unix.Flock_t) error {
	for i := 0; ; i++ {
		err := fcntlFlock(f.Fd(), ofdSetlk, lk)
		if err == nil {
			return nil
		}
		if i >= einvalRetries || !errors.Is(err, unix.EINVAL) {
			return os.NewSyscallError("fcntl F_OFD_SETLK", err)
		}
	}
}

var fcntlFlock = unix.FcntlFlock
