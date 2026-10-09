//go:build !js

package fsio

import "os"

// ReadAt is f.ReadAt.
func ReadAt(f *os.File, b []byte, off int64) (int, error) { return f.ReadAt(b, off) }

// WriteAt is f.WriteAt.
func WriteAt(f *os.File, b []byte, off int64) (int, error) { return f.WriteAt(b, off) }

// Sync is f.Sync.
func Sync(f *os.File) error { return f.Sync() }
