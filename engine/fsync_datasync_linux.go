//go:build linux

package engine

import (
	"os"

	"golang.org/x/sys/unix"
)

// datasyncFile makes f's data durable, and whatever metadata reading it back
// needs (its size), without committing the rest (mtime): fdatasync(2).
func datasyncFile(f *os.File) error {
	for {
		err := unix.Fdatasync(int(f.Fd()))
		if err != unix.EINTR {
			return err
		}
	}
}
