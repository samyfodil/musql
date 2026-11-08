//go:build !amd64 && !arm64 && !js

package jit

// HasVector is false on every architecture without an encoder.
func HasVector() bool { return false }

// VMMinWork is the least native work worth an entry from the VDBE (see
// engine/vdbe_jit.go's vmJITMinWork).
const VMMinWork = 3

// VMReal reports whether the VM JIT has its REAL arms (VReal to VMath).
const VMReal = false

// VMSegCursor reports whether a VColumn marked segment-capable reads a
// segment cursor's column blocks (VCursor.Seg).
const VMSegCursor = false

// VMTextOrder reports whether VTextCmpJump and VTextCmpSet take an ordered
// condition, and a numeric affinity's whole-number test (Imm 1).
const VMTextOrder = false
