//go:build !linux && !darwin && !windows && !js

package filelock

import (
	"fmt"
	"os"
	"runtime"
)

// Package filelock provides unsupported stubs for platforms without byte-range
// locking. LockRange fails explicitly rather than falling back to process-level
// F_SETLK, which has different semantics.

// errUnsupported names the platform in an error message.
func errUnsupported(op string) error {
	return fmt.Errorf("filelock: %s: byte-range file locking is not implemented on %s/%s (this build has no open-file-description lock primitive; see internal/filelock/filelock_unsupported.go)",
		op, runtime.GOOS, runtime.GOARCH)
}

// LockRange reports the platform gap rather than taking a weaker lock.
func LockRange(f *os.File, start, length int64, exclusive bool) (ok bool, err error) {
	return false, errUnsupported("LockRange")
}

// UnlockRange is the counterpart to LockRange and fails the same way.
func UnlockRange(f *os.File, start, length int64) error {
	return errUnsupported("UnlockRange")
}
