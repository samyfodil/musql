//go:build arm64

package jit

// HasVector reports whether vector kernels can be emitted (always true on arm64).
func HasVector() bool { return true }

// VMMinWork is the least native work worth an entry from the VDBE (see
// engine/vdbe_jit.go's vmJITMinWork).
const VMMinWork = 3

// cpuFeatures is what the float kernels may use: on AArch64 the fused
// multiply-add and the FRINT roundings are part of the base architecture.
type cpuFeatures struct {
	fma, round bool
}

var cpuFeat = cpuFeatures{fma: true, round: true}

// SetCPUForTest narrows the features the float kernels see.
func SetCPUForTest(fma, round bool) (restore func()) {
	old := cpuFeat
	cpuFeat = cpuFeatures{fma: fma && old.fma, round: round && old.round}
	return func() { cpuFeat = old }
}

// VMReal reports whether the VM JIT has its REAL arms (VReal to VMath).
const VMReal = true

// VMSegCursor reports whether a VColumn marked segment-capable reads a
// segment cursor's column blocks (VCursor.Seg).
const VMSegCursor = true

// VMTextOrder reports whether VTextCmpJump and VTextCmpSet take an ordered
// condition, and a numeric affinity's whole-number test (Imm 1).
const VMTextOrder = true

// VMRealInts reports whether VFAdd, VFSub and VFMul over two INTEGERs run the
// INTEGER arm rather than exit.
const VMRealInts = true
