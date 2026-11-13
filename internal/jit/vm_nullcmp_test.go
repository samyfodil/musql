//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import "testing"

// TestVMNullCmp: a comparison that answers a NULL operand (Imm) stores NULL
// or falls through or jumps as the VDBE's compareOp does, alone and fused
// with its test, and an operand read after it is guarded again.
func TestVMNullCmp(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	noGC(t)
	null, i := tvalue{Typ: tvNull}, func(v int64) tvalue { return tvalue{Typ: tvInt, I: v} }
	text := tvalue{Typ: tvText, S: []byte("abc")}

	// r2 = r1 < r0; r3 = r0 + r0.
	set := []VInsn{{Op: VCmpSet, A: 0, B: 1, C: 2, Cond: CondL, Imm: 1}, {Op: VAdd, A: 0, B: 0, C: 3}, {Op: VExit}}
	for _, c := range []struct {
		r0, r1 tvalue
		pc     int64
		r2, r3 tvalue
	}{
		{null, i(5), 1, null, i(-1)},
		{i(4), null, 2, null, i(8)},
		{i(3), i(5), 2, i(0), i(6)},
		{i(7), i(5), 2, i(1), i(14)},
	} {
		regs := []tvalue{c.r0, c.r1, text, i(-1)}
		args := &ProgArgs{}
		runVM(t, set, nil, regs, args, 0)
		if args.PC != c.pc || !same(regs[2], c.r2) || regs[2].S != nil || !same(regs[3], c.r3) {
			t.Fatalf("set %+v %+v: pc %d r2 %+v r3 %+v", c.r0, c.r1, args.PC, regs[2], regs[3])
		}
	}

	// if r1 < r0 goto 2; r3 = 7.
	for _, imm := range []int64{1, 2} {
		jump := []VInsn{{Op: VCmpJump, A: 0, B: 1, T: 2, Cond: CondL, Imm: imm}, {Op: VInt, A: 3, Imm: 7}, {Op: VExit}}
		for _, c := range []struct {
			r0, r1 tvalue
			jumps  bool
		}{{null, i(1), imm == 2}, {i(1), null, imm == 2}, {i(2), i(1), true}, {i(1), i(2), false}} {
			regs := []tvalue{c.r0, c.r1, null, i(-1)}
			args := &ProgArgs{}
			runVM(t, jump, nil, regs, args, 0)
			if args.PC != 2 || (regs[3].I == 7) == c.jumps {
				t.Fatalf("jump imm %d %+v %+v: pc %d r3 %+v", imm, c.r0, c.r1, args.PC, regs[3])
			}
		}
	}

	// Fused: r2 = r1 < r0; if !r2 goto 4 (a NULL jumps when the test's C
	// says); r3 = 1, or at 4 r3 = 2.
	for _, nullJumps := range []int{0, 1} {
		prog := []VInsn{
			{Op: VCmpSet, A: 0, B: 1, C: 2, Cond: CondL, Imm: 1},
			{Op: VIf, A: 2, T: 4, C: nullJumps},
			{Op: VInt, A: 3, Imm: 1},
			{Op: VExit},
			{Op: VInt, A: 3, Imm: 2},
			{Op: VExit},
		}
		entry := make([]bool, len(prog))
		entry[0] = true
		for _, c := range []struct {
			r0, r1 tvalue
			r3     int64
		}{{null, i(1), 1 + int64(nullJumps)}, {i(1), null, 1 + int64(nullJumps)}, {i(2), i(1), 1}, {i(1), i(2), 2}} {
			regs := []tvalue{c.r0, c.r1, text, i(-1)}
			args := &ProgArgs{}
			runVM(t, prog, entry, regs, args, 0)
			if regs[3].I != c.r3 {
				t.Fatalf("fused C %d %+v %+v: pc %d r3 %+v", nullJumps, c.r0, c.r1, args.PC, regs[3])
			}
		}
	}
}
