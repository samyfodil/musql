//go:build !js && !linux && !darwin

package fsstamp

import "os"

// Of returns path's size and mtime in nanoseconds; ok is false when it
// cannot be stat'ed.
func Of(path string) (size, mtime int64, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	return fi.Size(), fi.ModTime().UnixNano(), true
}

// OfFile is not available here; ok is always false and the caller stats the
// path instead.
func OfFile(fd uintptr) (size, mtime int64, linked, ok bool) { return 0, 0, false, false }

// FileStamps reports whether OfFile works on this platform.
const FileStamps = false
