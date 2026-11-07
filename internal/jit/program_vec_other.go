//go:build !amd64

package jit

import "fmt"

// EmitProgramVec is the amd64 vector loop, which nothing else has.
func EmitProgramVec(insns []ProgInsn, nCols int) ([]byte, error) {
	return nil, fmt.Errorf("jit: no vector program loop on this platform")
}
