package fsstamp

import (
	"runtime"
	"unsafe"
)

//go:wasmimport musqljit stat_stamp
func statStamp(path unsafe.Pointer, n int32, out unsafe.Pointer) int32

// Of returns path's size and mtime in nanoseconds; ok is false when it
// cannot be stat'ed.
func Of(path string) (size, mtime int64, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	var out [2]int64
	p := []byte(path)
	r := statStamp(unsafe.Pointer(&p[0]), int32(len(p)), unsafe.Pointer(&out))
	runtime.KeepAlive(p)
	if r == 0 {
		return 0, 0, false
	}
	return out[0], out[1], true
}
