//go:build !amd64 && !arm64 && !wasm

package jit

import "fmt"

// EmitVM runs the VDBE on unsupported platforms.
func EmitVM(prog []VInsn, nReg int, lay ValueLayout) ([]byte, error) {
	return nil, fmt.Errorf("jit: no VM emitter for this architecture")
}
