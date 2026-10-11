//go:build !amd64

package jit

import "fmt"

// EmitVec1T8 has no encoder on this architecture yet; the engine sums in Go
// there.
func EmitVec1T8(op Vec1Op) ([]byte, error) {
	return nil, fmt.Errorf("jit: no vec1 encoder for this architecture")
}

// EmitVec1Scan16 likewise.
func EmitVec1Scan16() ([]byte, error) {
	return nil, fmt.Errorf("jit: no vec1 encoder for this architecture")
}

// EmitVec1RowsL2D likewise.
func EmitVec1RowsL2D() ([]byte, error) {
	return nil, fmt.Errorf("jit: no vec1 encoder for this architecture")
}

// EmitVec1BestD likewise.
func EmitVec1BestD(round bool) ([]byte, error) {
	return nil, fmt.Errorf("jit: no vec1 encoder for this architecture")
}

// EmitVec1Cand likewise.
func EmitVec1Cand() ([]byte, error) {
	return nil, fmt.Errorf("jit: no vec1 encoder for this architecture")
}

// EmitVec1WHT likewise.
func EmitVec1WHT() ([]byte, error) {
	return nil, fmt.Errorf("jit: no vec1 encoder for this architecture")
}
