//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math"
	"slices"
	"testing"
)

// TestVMInSet: VInSet finds every probe among sorted keys, entered anywhere or
// fused, and exits writing nothing on a bad count or operand.
func TestVMInSet(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	for _, n := range []int{0, 1, 2, 3, 4, 7, 8, 9, 64, 257} {
		keys := make([]int64, 0, n)
		for k := range n {
			keys = append(keys, int64(k*3-50))
		}
		if n >= 4 {
			keys[0], keys[n-1] = math.MinInt64+1, math.MaxInt64
		}
		probes := []int64{math.MinInt64, math.MinInt64 + 1, math.MaxInt64, math.MaxInt64 - 1, 0}
		for x := int64(-60); x < int64(3*n); x++ {
			probes = append(probes, x)
		}
		for _, x := range probes {
			want := int64(0)
			if slices.Contains(keys, x) {
				want = 1
			}
			regs := make([]tvalue, 4+n) // the last a spare
			regs[0], regs[1], regs[2] = tvalue{Typ: tvInt, I: x}, tvalue{Typ: tvNull}, tvalue{Typ: tvInt, I: int64(n)}
			for k, v := range keys {
				regs[3+k] = tvalue{Typ: tvInt, I: v}
			}
			args := &ProgArgs{}
			runVM(t, []VInsn{{Op: VInSet, A: 0, B: 3, C: 1, Imm: 2}, {Op: VExit}}, nil, regs, args, 0)
			if args.PC != 1 || regs[1].Typ != tvInt || regs[1].I != want {
				t.Fatalf("n=%d x=%d: pc %d r[1] %+v, want %d", n, x, args.PC, regs[1], want)
			}
			// Fused: the operand loaded by the step before, read again after.
			spare := 3 + n
			regs[1], regs[spare] = tvalue{Typ: tvNull}, tvalue{Typ: tvNull}
			prog := []VInsn{
				{Op: VInt, A: 0, Imm: x},
				{Op: VInSet, A: 0, B: 3, C: 1, Imm: 2},
				{Op: VCmpSet, A: 0, B: 0, C: spare, Cond: CondE},
				{Op: VExit},
			}
			args = &ProgArgs{}
			runVM(t, prog, []bool{true, false, false, false}, regs, args, 0)
			if args.PC != 3 || regs[1].Typ != tvInt || regs[1].I != want || regs[spare].I != 1 || regs[0].I != x {
				t.Fatalf("fused n=%d x=%d: pc %d r[1] %+v r[0] %+v, want %d", n, x, args.PC, regs[1], regs[0], want)
			}
		}
	}
	exits := []struct {
		name string
		x    tvalue
		n    tvalue
	}{
		{"no set built", tvalue{Typ: tvInt, I: 1}, tvalue{Typ: tvNull}},
		{"a set of other values", tvalue{Typ: tvInt, I: 1}, tvalue{Typ: tvInt, I: -1}},
		{"a NULL operand", tvalue{Typ: tvNull}, tvalue{Typ: tvInt, I: 2}},
		{"a text operand", tvalue{Typ: tvText, S: []byte("1")}, tvalue{Typ: tvInt, I: 2}},
	}
	for _, c := range exits {
		regs := []tvalue{c.x, {Typ: tvInt, I: 77}, c.n, {Typ: tvInt, I: 1}, {Typ: tvInt, I: 2}}
		args := &ProgArgs{}
		runVM(t, []VInsn{{Op: VInSet, A: 0, B: 3, C: 1, Imm: 2}, {Op: VExit}}, nil, regs, args, 0)
		if args.PC != 0 || regs[1].Typ != tvInt || regs[1].I != 77 {
			t.Fatalf("%s: pc %d r[1] %+v, want an exit at 0 with r[1] untouched", c.name, args.PC, regs[1])
		}
	}
}
