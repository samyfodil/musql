//go:build amd64

package jit

import "golang.org/x/sys/cpu"

func cpuHasAVX2() bool { return cpu.X86.HasAVX2 }

// HasVector reports whether the vector kernels can be emitted on this machine.
// The Atom servers this project gates on have SSE4.2 and no AVX at all, so a
// caller must have a path for false -- which is the scalar kernel, and below
// that the VDBE. (arm64 has no such case: see cpu_arm64.go.)
func HasVector() bool { return cpuHasAVX2() }

// VMMinWork is the least native work worth an entry from the VDBE (see
// engine/vdbe_jit.go's vmJITMinWork).
const VMMinWork = 3

// cpuFeatures is what the float kernels may use beyond SSE2, read once from
// the same CPUID bits Go reads (golang.org/x/sys/cpu): FMA3 for a multiply-add
// rounded once, SSE4.1's ROUNDSD to round to an integer. A kernel needing one
// the CPU lacks is not emitted, and the statement runs without it.
type cpuFeatures struct {
	fma, round bool
}

var cpuFeat = cpuFeatures{fma: cpu.X86.HasFMA, round: cpu.X86.HasSSE41}

// SetCPUForTest narrows the features the float kernels see, so a machine that
// has FMA and SSE4.1 can run what one without them would.
func SetCPUForTest(fma, round bool) (restore func()) {
	old := cpuFeat
	cpuFeat = cpuFeatures{fma: fma && old.fma, round: round && old.round}
	return func() { cpuFeat = old }
}

// VMReal reports whether the VM JIT has its REAL arms (VReal to VMath).
const VMReal = true
