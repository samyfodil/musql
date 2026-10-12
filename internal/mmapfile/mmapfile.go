// Package mmapfile maps a file read-only into memory for columnar segments.
// A column block requires 8-byte-aligned bytes already in the address space.
// Unsupported platforms read the file instead (correct, slower, never wrong).
package mmapfile

import (
	"os"
	"sync"
)

// File is a mapped (or, on a platform without mapping, a loaded) file.
type File struct {
	data   []byte
	mapped bool
	f      *os.File
	ahead  sync.Once
}

// Data returns the file's bytes (read-only until Close).
func (f *File) Data() []byte {
	if f == nil {
		return nil
	}
	return f.data
}

// Mapped reports whether the file is mapped (true) or read (false).
func (f *File) Mapped() bool { return f != nil && f.mapped }

// Len is the file's length in bytes.
func (f *File) Len() int {
	if f == nil {
		return 0
	}
	return len(f.data)
}

// ReadAhead asks, once per mapping, for the whole file to be read into memory
// ahead of its first touch, where the platform needs asking (see the darwin
// readAhead); elsewhere it does nothing. A hint: the bytes read the same
// either way.
func (f *File) ReadAhead() {
	if f == nil || !f.mapped {
		return
	}
	f.ahead.Do(func() { readAhead(f.data) })
}
