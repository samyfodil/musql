package engine

import (
	"strings"
	"testing"
)

// TestEveryOpcodeHasAName: EXPLAIN and error messages print an opcode by its
// mnemonic, and one missing from opNames printed as "Op(84)". The loop ends at
// the last opcode declared; move its bound when one is added after it.
func TestEveryOpcodeHasAName(t *testing.T) {
	for op := OpCode(1); op <= OpRecQueueCheck; op++ {
		if strings.HasPrefix(op.String(), "Op(") {
			t.Errorf("opcode %d has no name in opNames", op)
		}
	}
}
