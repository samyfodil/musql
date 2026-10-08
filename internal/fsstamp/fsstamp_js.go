package fsstamp

import (
	"runtime"
	"unsafe"
)

// The host reads path and writes out during the call and keeps neither, so
// both stay on the stack: a stamp is two per statement.
//
//go:wasmimport musqljit stat_stamp
//go:noescape
func statStamp(path unsafe.Pointer, n int32, out unsafe.Pointer) int32

// Of returns path's size and mtime in nanoseconds; ok is false when it
// cannot be stat'ed.
func Of(path string) (size, mtime int64, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	var out [2]int64
	r := statStamp(unsafe.Pointer(unsafe.StringData(path)), int32(len(path)), unsafe.Pointer(&out))
	runtime.KeepAlive(path)
	if r == 0 {
		return 0, 0, false
	}
	return out[0], out[1], true
}

//go:wasmimport musqljit fstat_stamp
//go:noescape
func fstatStamp(fd int32, out unsafe.Pointer) int32

// OfFile returns an open file's size, mtime in nanoseconds, and whether it
// still has a name (a nonzero link count): the host's fstat, one call, where
// stat'ing the path again was a syscall/js round trip per commit.
func OfFile(fd uintptr) (size, mtime int64, linked, ok bool) {
	var out [3]int64
	if fstatStamp(int32(fd), unsafe.Pointer(&out)) == 0 {
		return 0, 0, false, false
	}
	return out[0], out[1], out[2] > 0, true
}

// FileStamps reports whether OfFile works on this platform.
const FileStamps = true
