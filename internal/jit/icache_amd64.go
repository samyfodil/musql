//go:build amd64 && (unix || windows)

package jit

// flushICache is a no-op on x86-64 (caches are coherent with stores).
func flushICache(start *byte, n uintptr) {}
