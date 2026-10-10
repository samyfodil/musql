//go:build !amd64 && !wasm

package jit

import "fmt"

// EmitVecDist has no encoder on this architecture yet; the engine computes the
// distances in Go there.
func EmitVecDist(l2 bool) ([]byte, error) {
	return nil, fmt.Errorf("jit: no vector-distance encoder for this architecture")
}

// EmitI8Dot likewise.
func EmitI8Dot() ([]byte, error) {
	return nil, fmt.Errorf("jit: no int8 dot-product encoder for this architecture")
}

// EmitMaxAbsBits likewise.
func EmitMaxAbsBits() ([]byte, error) {
	return nil, fmt.Errorf("jit: no max-abs encoder for this architecture")
}

// EmitQuantize likewise.
func EmitQuantize() ([]byte, error) {
	return nil, fmt.Errorf("jit: no quantize encoder for this architecture")
}

// EmitVecDistStrided likewise.
func EmitVecDistStrided(l2 bool) ([]byte, error) {
	return nil, fmt.Errorf("jit: no vector-distance encoder for this architecture")
}
