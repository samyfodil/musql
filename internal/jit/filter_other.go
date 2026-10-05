//go:build !amd64 && !arm64

package jit

import "fmt"

// EmitFilterCountSIMD has no encoder on this architecture; returns an error.
//
// amd64 and arm64 each have one; everything else lands here. The two are
// separate files rather than one with flags, because they share no encoding:
// NEON compares two int64 lanes where AVX2 does four, and the entry register is
// X0 rather than RDI.
func EmitFilterCountSIMD(condA Cond, two bool, condC Cond) ([]byte, error) {
	return nil, fmt.Errorf("jit: no vector encoder for this architecture")
}

// EmitFilterCount likewise.
func EmitFilterCount(condA Cond, two bool, condC Cond) ([]byte, error) {
	return nil, fmt.Errorf("jit: no encoder for this architecture")
}
