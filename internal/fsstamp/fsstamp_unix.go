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

// OfFile is Of through an open descriptor: fstat, no path lookup and no
// allocation. linked is false once the file has no name left -- renamed over
// or deleted -- which is how a caller holding the descriptor notices that its
// path now names a different file (SQLite's unixFileHasMoved test).
func OfFile(fd uintptr) (size, mtime int64, linked, ok bool) {
	var st unix.Stat_t
	if unix.Fstat(int(fd), &st) != nil {
		return 0, 0, false, false
	}
	return st.Size, st.Mtim.Nano(), st.Nlink > 0, true
}

// FileStamps reports whether OfFile works on this platform.
const FileStamps = true
