//go:build arm64

package jit

// HasVector reports whether vector kernels can be emitted (always true on arm64).
func HasVector() bool { return true }

// VMMinWork is the least native work worth an entry from the VDBE (see
// engine/vdbe_jit.go's vmJITMinWork).
const VMMinWork = 3
