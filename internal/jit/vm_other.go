//go:build !amd64 && !arm64 && !wasm

package jit

import "fmt"

// emitVM runs the VDBE on unsupported platforms.
func emitVM(prog []VInsn, nReg int, lay ValueLayout, bound, entry []bool, cache bool) ([]byte, error) {
	return nil, fmt.Errorf("jit: no VM emitter for this architecture")
}
