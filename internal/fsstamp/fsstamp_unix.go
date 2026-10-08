//go:build linux || darwin

package fsstamp

import "golang.org/x/sys/unix"

// Of returns path's size and mtime in nanoseconds; ok is false when it
// cannot be stat'ed. unix.Stat into a stack Stat_t rather than os.Stat,
// which builds a heap FileInfo: one allocation (the path's NUL-terminated
// copy) instead of two, on a call every statement makes twice.
func Of(path string) (size, mtime int64, ok bool) {
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil {
		return 0, 0, false
	}
	return st.Size, st.Mtim.Nano(), true
}
