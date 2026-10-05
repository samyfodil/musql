// Package mmapfile maps a file read-only into memory for columnar segments.
// A column block requires 8-byte-aligned bytes already in the address space.
// Unsupported platforms read the file instead (correct, slower, never wrong).
package mmapfile

import "os"

// File is a mapped (or, on a platform without mapping, a loaded) file.
type File struct {
	data   []byte
	mapped bool
	f      *os.File
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
