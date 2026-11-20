//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math"
	"testing"
)

// TestProgramSumStartsFromOut checks that a sum starts from the caller's Out,
// so a run carried on from an earlier one overflows at the row a single run
// over both would.
func TestProgramSumStartsFromOut(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	insns := []ProgInsn{
		{Op: POpLoadCol, A: 0, B: 0},
		{Op: POpAccSum, A: 0, B: 1},
	}
	col := []int64{5, 5, -20, 3, 1, 1, 1, 1}
	sum := int64(0)
	for _, v := range col {
		sum += v
	}
	run := func(emit func([]ProgInsn, int) ([]byte, error), seed int64) (int64, int64) {
		code, err := emit(insns, 1)
		if err != nil {
			t.Fatal(err)
		}
		kern, err := Map(code)
		if err != nil {
			t.Fatal(err)
		}
		defer kern.Close()
		regs := make([]int64, 4)
		out, ovf := seed, int64(0)
		args := &ProgArgs{N: int64(len(col)), Regs: &regs[0], Out: &out, Overflow: &ovf}
		args.Col[0] = &col[0]
		kern.Call2(args)
		return out, ovf
	}
	cases := []struct {
		seed int64
		ovf  bool // a running sum leaves int64
	}{
		{0, false},
		{100, false},
		{math.MaxInt64 - 10, false}, // peaks at +10, never past
		{math.MaxInt64 - 9, true},   // +10 at the second row
		{math.MinInt64 + 10, false},
		{math.MinInt64 + 9, true}, // -10 after the third row
	}
	for _, c := range cases {
		out, ovf := run(EmitProgram, c.seed)
		if (ovf != 0) != c.ovf {
			t.Errorf("scalar from %d: overflow %d, want %v", c.seed, ovf, c.ovf)
		}
		if !c.ovf && out != c.seed+sum {
			t.Errorf("scalar from %d: %d, want %d", c.seed, out, c.seed+sum)
		}
	}
	if _, err := EmitProgramVec(insns, 1); err != nil {
		return
	}
	// The vector loop vouches for a sum only when the absolute values, the
	// start's with them, fit; the scalar loop answers the rest.
	for _, c := range cases {
		out, ovf := run(EmitProgramVec, c.seed)
		if c.ovf && ovf == 0 {
			t.Errorf("vector from %d: no overflow flagged", c.seed)
		}
		if ovf == 0 && out != c.seed+sum {
			t.Errorf("vector from %d: %d, want %d", c.seed, out, c.seed+sum)
		}
	}
	if out, ovf := run(EmitProgramVec, 100); ovf != 0 || out != 100+sum {
		t.Errorf("vector from 100: %d overflow %d, want %d", out, ovf, 100+sum)
	}
}
