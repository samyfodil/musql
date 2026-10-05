//go:build !unix

package mmapfile

import "os"

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

// Close releases the loaded bytes.
func (f *File) Close() error {
	if f == nil {
		return nil
	}
	f.data = nil
	return nil
}
