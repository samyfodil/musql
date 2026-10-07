//go:build !js

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
