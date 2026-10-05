//go:build arm64 && (unix || windows)

package jit

// flushICache makes n bytes of freshly written code at start visible to the
// instruction fetcher. Implemented in trampoline_arm64.s; see the comment there
// for why mprotect alone is not enough on this architecture.
//
//go:noescape
func flushICache(start *byte, n uintptr)
