//go:build !wasm

package jit

import "testing"

func TestZZArmShapes(t *testing.T) {
	if !Available {
		t.Skip("no JIT")
	}
	const n = 200
	col := make([]int64, n)
	for i := range col {
		col[i] = int64(i * 7919 % 1000003)
	}
	cases := []struct {
		name  string
		insns []ProgInsn
	}{
		{"reg bound", []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpCmp, A: 2, B: 0, C: 1, Cond: CondG},
			{Op: POpSkipIfZero, A: 2},
			{Op: POpEmitRow},
		}},
		{"small const", []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpSetConst, A: 1, B: 10},
			{Op: POpCmp, A: 2, B: 0, C: 1, Cond: CondG},
			{Op: POpSkipIfZero, A: 2},
			{Op: POpEmitRow},
		}},
		{"large const", []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpSetConst, A: 1, B: 990000},
			{Op: POpCmp, A: 2, B: 0, C: 1, Cond: CondG},
			{Op: POpSkipIfZero, A: 2},
			{Op: POpEmitRow},
		}},
		{"jump form", []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpSetConst, A: 1, B: 500},
			{Op: POpCmp, A: 2, B: 0, C: 1, Cond: CondG},
			{Op: POpJumpIfZero, A: ProgNextRow, B: 2},
			{Op: POpEmitRow},
		}},
		{"two cols and", []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpLoadCol, A: 3, B: 1},
			{Op: POpSetConst, A: 1, B: 500},
			{Op: POpCmp, A: 2, B: 0, C: 1, Cond: CondG},
			{Op: POpCmp, A: 4, B: 3, C: 1, Cond: CondL},
			{Op: POpAnd, A: 5, B: 2, C: 4},
			{Op: POpSkipIfZero, A: 5},
			{Op: POpEmitRow},
		}},
	}
	for _, tc := range cases {
		nCols := 1
		for _, in := range tc.insns {
			if in.Op == POpLoadCol && in.B+1 > nCols {
				nCols = in.B + 1
			}
		}
		code, err := EmitProgram(tc.insns, nCols)
		if err != nil {
			t.Errorf("%s: emit: %v", tc.name, err)
			continue
		}
		k, err := Map(code)
		if err != nil {
			t.Errorf("%s: map: %v", tc.name, err)
			continue
		}
		regs := make([]int64, 8)
		regs[1] = 500
		sel := make([]int64, n)
		var out, ovf int64
		args := &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf, Sel: &sel[0]}
		args.Col[0] = &col[0]
		args.Col[1] = &col[0]
		t.Logf("%-14s %d code bytes, running...", tc.name, len(code))
		k.Call2(args)
		k.Close()
		t.Logf("%-14s selected %d", tc.name, out)
	}
}
