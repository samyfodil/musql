//go:build linux

package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// newAnonFile creates a file in dir with no name (O_TMPFILE), so a crash
// leaves nothing behind, that linkAnon can still give one later; false where
// the filesystem has no unnamed files.
func newAnonFile(dir string) (*os.File, bool) {
	fd, err := unix.Open(dir, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, false
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir, "musql-spill")), true
}

// linkAnon gives f, a newAnonFile file, the name dst, replacing whatever dst
// was: linked beside it first, since a link cannot replace, then renamed.
func linkAnon(f *os.File, dst string) error {
	tmp := dst + ".link"
	os.Remove(tmp)
	if err := unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", f.Fd()), unix.AT_FDCWD, tmp, unix.AT_SYMLINK_FOLLOW); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
