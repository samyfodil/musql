// Package mmapfile maps a file read-only into memory for columnar segments.
// A column block requires 8-byte-aligned bytes already in the address space.
// Unsupported platforms read the file instead (correct, slower, never wrong).
package mmapfile

import (
	"os"
	"unsafe"
)

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

// ReadOut returns b's bytes, b a slice of this file's mapping, read from the
// file with pread rather than through the mapping -- on a platform where a
// first touch of a mapped page costs more than reading it (readNotFault) --
// and false when it did not: elsewhere, for bytes outside the mapping, or
// when the read fails (the caller then reads b itself, as it would have).
func (f *File) ReadOut(b []byte) ([]byte, bool) {
	if !readNotFault || f == nil || !f.mapped || f.f == nil || len(b) == 0 || len(f.data) == 0 {
		return nil, false
	}
	base := uintptr(unsafe.Pointer(unsafe.SliceData(f.data)))
	at := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	if at < base || at+uintptr(len(b)) > base+uintptr(len(f.data)) {
		return nil, false
	}
	out := make([]byte, len(b))
	if _, err := f.f.ReadAt(out, int64(at-base)); err != nil {
		return nil, false
	}
	return out, true
}
