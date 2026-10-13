//go:build unix

package engine

import (
	"os"
	"syscall"
)

// segFileKeyOf is the open file's segFileKey, from one fstat.
func segFileKeyOf(f *os.File) (segFileKey, bool) {
	fi, err := f.Stat()
	if err != nil {
		return segFileKey{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return segFileKey{}, false
	}
	return segFileKey{dev: uint64(st.Dev), ino: uint64(st.Ino), size: fi.Size(), mtime: fi.ModTime().UnixNano()}, true
}
