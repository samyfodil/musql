//go:build !amd64 && !arm64 && !wasm

package jit

import "fmt"

// EmitProgram has no encoder on this architecture. Returns error so the engine's
// call site is identical everywhere, with fallback at runtime.
func EmitProgram(insns []ProgInsn, nCols int) ([]byte, error) {
	return nil, fmt.Errorf("jit: no program encoder for this architecture")
}
