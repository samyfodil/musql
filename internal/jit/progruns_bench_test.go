package jit

import (
	"math/rand"
	"testing"
)

// BenchmarkProgramRuns times the run shapes fusion and the register
// allocator are for, over 100,000 rows of two columns, emitted with fusion on
// and off: an arithmetic chain into a filter, an AND/OR condition read by
// one test, and an expression summed.
func BenchmarkProgramRuns(b *testing.B) {
	if !Available {
		b.Skip("no JIT on this platform")
	}
	defer SetFusion(true)
	const n = 100_000
	rng := rand.New(rand.NewSource(5))
	cols := make([][]int64, 2)
	for c := range cols {
		cols[c] = make([]int64, n)
		for i := range cols[c] {
			cols[c][i] = int64(rng.Intn(1000))
		}
	}
	shapes := []struct {
		name  string
		insns []ProgInsn
	}{
		{"arith-filter", []ProgInsn{ // WHERE (a*3+b)-7 > 900
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpMul, A: 0, B: 0, C: 9},
			{Op: POpLoadCol, A: 1, B: 1},
			{Op: POpAdd, A: 0, B: 0, C: 1},
			{Op: POpSub, A: 0, B: 0, C: 10},
			{Op: POpCmp, A: 2, B: 0, C: 8, Cond: CondG},
			{Op: POpSkipIfZero, A: 2},
			{Op: POpAccCount},
		}},
		{"and-or", []ProgInsn{ // WHERE (a > 200 AND b < 800) OR a = b
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpCmp, A: 2, B: 0, C: 11, Cond: CondG},
			{Op: POpLoadCol, A: 1, B: 1},
			{Op: POpCmp, A: 3, B: 1, C: 12, Cond: CondL},
			{Op: POpAnd, A: 2, B: 2, C: 3},
			{Op: POpCmp, A: 4, B: 0, C: 1, Cond: CondE},
			{Op: POpOr, A: 2, B: 2, C: 4},
			{Op: POpSkipIfZero, A: 2},
			{Op: POpAccCount},
		}},
		{"sum-expr", []ProgInsn{ // SELECT sum(a*b+a) WHERE b > 100
			{Op: POpLoadCol, A: 1, B: 1},
			{Op: POpCmp, A: 2, B: 1, C: 13, Cond: CondG},
			{Op: POpSkipIfZero, A: 2},
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpMul, A: 3, B: 0, C: 1},
			{Op: POpAdd, A: 3, B: 3, C: 0},
			{Op: POpAccSum, A: 3, B: 14},
		}},
	}
	for _, sh := range shapes {
		for _, fuse := range []bool{false, true} {
			name := sh.name + "/unfused"
			if fuse {
				name = sh.name + "/fused"
			}
			b.Run(name, func(b *testing.B) {
				SetFusion(fuse)
				code, err := EmitProgram(sh.insns, 2)
				if err != nil {
					b.Fatal(err)
				}
				k, err := Map(code)
				if err != nil {
					b.Fatal(err)
				}
				defer k.Close()
				regs := make([]int64, 16)
				regs[8], regs[9], regs[10], regs[11], regs[12], regs[13] = 900, 3, 7, 200, 800, 100
				var out, ovf int64
				args := &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf}
				for i, c := range cols {
					args.Col[i] = &c[0]
				}
				b.SetBytes(n * 8)
				for b.Loop() {
					k.Call2(args)
				}
			})
		}
	}
}
