//go:build unix

package mmapfile

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Open maps path read-only.
//
// MAP_SHARED rather than MAP_PRIVATE: the mapping is read-only either way, and
// shared means two readers of the same file share the page cache rather than
// each getting copy-on-write pages of their own.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	n := fi.Size()
	if n == 0 {
		// mmap refuses a zero-length mapping; an empty file has no bytes to
		// hand back and needs no mapping to say so.
		f.Close()
		return &File{}, nil
	}
	if n != int64(int(n)) {
		f.Close()
		return nil, fmt.Errorf("mmapfile: %s is %d bytes, too large to map on this platform", path, n)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(n), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmapfile: mapping %s: %w", path, err)
	}
	return &File{data: data, mapped: true, f: f}, nil
}

// Close unmaps and closes. Safe to call more than once.
func (f *File) Close() error {
	if f == nil {
		return nil
	}
	var err error
	if f.mapped && f.data != nil {
		err = unix.Munmap(f.data)
	}
	f.data, f.mapped = nil, false
	if f.f != nil {
		if cerr := f.f.Close(); err == nil {
			err = cerr
		}
		f.f = nil
	}
	return err
}
