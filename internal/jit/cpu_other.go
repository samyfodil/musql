//go:build !amd64 && !arm64 && !js

package jit

// HasVector is false on every architecture without an encoder.
func HasVector() bool { return false }

// VMMinWork is the least native work worth an entry from the VDBE (see
// engine/vdbe_jit.go's vmJITMinWork).
const VMMinWork = 3

// VMReal reports whether the VM JIT has its REAL arms (VReal to VMath).
const VMReal = false
