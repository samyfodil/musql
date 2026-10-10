//go:build !amd64

package jit

import "fmt"

// EmitVecDist has no encoder on this architecture yet; the engine computes the
// distances in Go there.
func EmitVecDist(l2 bool) ([]byte, error) {
	return nil, fmt.Errorf("jit: no vector-distance encoder for this architecture")
}
