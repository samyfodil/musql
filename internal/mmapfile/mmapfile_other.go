//go:build !unix

package mmapfile

import (
	"errors"
	"io"
	"os"
)

// Open reads path instead of mapping: on Windows a mapped (or merely open) file
// cannot be replaced by the rename every rewrite publishes with.
// Correct, slower, never wrong: Data() returns same bytes either way.
func Open(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &File{data: data}, nil
}

// OpenFile is Open over a file already open, which it reads and closes.
func OpenFile(f *os.File) (*File, error) {
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data := make([]byte, fi.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	return &File{data: data}, nil
}

// Close releases the loaded bytes.
func (f *File) Close() error {
	if f == nil {
		return nil
	}
	f.data = nil
	return nil
}

// CanMapAt reports whether MapAt maps: not on this platform.
const CanMapAt = false

// MapAt fails here: a platform without mapping has no range of a file to
// hand out without reading it.
func MapAt(f *os.File, off int64, n int) (*File, error) {
	return nil, errors.New("mmapfile: no mapping on this platform")
}
