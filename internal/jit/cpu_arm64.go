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
