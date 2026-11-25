package jit

import (
	"slices"
	"testing"
)

// TestVLoopReach: which entries can reach a jump back before they exit,
// along fall-through, a conditional branch's target, a Goto, a Gosub and a
// Return alike.
func TestVLoopReach(t *testing.T) {
	for _, c := range []struct {
		name string
		prog []VInsn
		want []bool
	}{
		{"straight", []VInsn{{Op: VInt, A: 0, Imm: 1}, {Op: VAdd, A: 0, B: 0, C: 1}, {Op: VExit}}, []bool{false, false, false}},
		{"loop by fall-through", []VInsn{{Op: VInt, A: 0, Imm: 1}, {Op: VNext, A: 0, T: 0}, {Op: VExit}}, []bool{true, true, false}},
		{"loop by a branch", []VInsn{
			{Op: VCmpJump, A: 0, B: 1, Cond: CondL, T: 3},
			{Op: VInt, A: 2, Imm: 1},
			{Op: VExit},
			{Op: VNext, A: 0, T: 0},
			{Op: VExit},
		}, []bool{true, false, false, true, false}},
		{"exit before the loop", []VInsn{{Op: VInt, A: 0, Imm: 1}, {Op: VExit}, {Op: VGoto, T: 0}}, []bool{false, false, true}},
		{"gosub", []VInsn{{Op: VGosub, A: 0, T: 2}, {Op: VExit}, {Op: VReturn, A: 0}}, []bool{true, false, true}},
	} {
		if got := VLoopReach(c.prog); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
