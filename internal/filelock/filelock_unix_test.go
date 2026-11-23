//go:build linux || darwin

package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLockRangeEINVAL(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fails := 0
	fcntlFlock = func(fd uintptr, cmd int, lk *unix.Flock_t) error {
		if fails > 0 {
			fails--
			return unix.EINVAL
		}
		return unix.FcntlFlock(fd, cmd, lk)
	}
	defer func() { fcntlFlock = unix.FcntlFlock }()

	fails = einvalRetries
	if ok, err := LockRange(f, 0, 10, true); !ok || err != nil {
		t.Fatalf("after %d spurious EINVALs: ok=%v err=%v", einvalRetries, ok, err)
	}
	if err := UnlockRange(f, 0, 10); err != nil {
		t.Fatal(err)
	}
	fails = einvalRetries + 1
	if ok, err := LockRange(f, 0, 10, true); ok || !errors.Is(err, unix.EINVAL) {
		t.Fatalf("an EINVAL that persists: ok=%v err=%v, want EINVAL", ok, err)
	}
}
