//go:build amd64

package jit

import "golang.org/x/sys/cpu"

func cpuHasAVX2() bool { return cpu.X86.HasAVX2 }

// HasVector reports whether the vector kernels can be emitted on this machine.
// The Atom servers this project gates on have SSE4.2 and no AVX at all, so a
// caller must have a path for false -- which is the scalar kernel, and below
// that the VDBE. (arm64 has no such case: see cpu_arm64.go.)
func HasVector() bool { return cpuHasAVX2() }
