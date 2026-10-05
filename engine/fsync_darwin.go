package engine

import (
	"os"
	"syscall"
)

// fsyncFile makes f durable: F_FULLFSYNC when full, plain fsync otherwise.
func fsyncFile(f *os.File, full bool) error {
	if full {
		return f.Sync()
	}
	return syscall.Fsync(int(f.Fd()))
}
