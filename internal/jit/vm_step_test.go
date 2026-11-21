package jit

import (
	"slices"
	"testing"
)

// TestVMStepShared: vmTarget is the one target vmStepOf gives every op, and
// steps made into one shared array (vmStepsOf) are the steps made one at a
// time, none of them writing over another's registers.
func TestVMStepShared(t *testing.T) {
	var prog []VInsn
	for op := range 256 {
		in := VInsn{Op: VOp(op), A: 1, B: 2, C: 3, T: 7, Imm: 4}
		prog = append(prog, in)
		st := vmStepOf(in, 0)
		tg, ok := vmTarget(in)
		if len(st.targets) > 1 || ok != (len(st.targets) == 1) || ok && st.targets[0] != tg {
			t.Errorf("op %d: targets %v, vmTarget %d %v", op, st.targets, tg, ok)
		}
	}
	prog = append(prog, VInsn{Op: VResultRow, A: 10, B: 5, Skip: 0b00101})
	for pc, st := range vmStepsOf(prog) {
		one := vmStepOf(prog[pc], pc)
		if !slices.Equal(st.uses, one.uses) || !slices.Equal(st.defs, one.defs) || !slices.Equal(st.targets, one.targets) || st.useAll != one.useAll || st.fall != one.fall {
			t.Errorf("pc %d: shared %+v, alone %+v", pc, st, one)
		}
	}
}
