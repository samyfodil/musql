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

// OfFile is not available here; ok is always false and the caller stats the
// path instead.
func OfFile(fd uintptr) (size, mtime int64, linked, ok bool) { return 0, 0, false, false }

// FileStamps reports whether OfFile works on this platform.
const FileStamps = false
