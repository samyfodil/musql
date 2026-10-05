//go:build (amd64 || arm64) && (unix || windows)

package jit

import (
	"math"
	"math/rand"
	"testing"
	"unsafe"
)

// ProgArgs is an ABI like Args: generated code addresses its fields by byte
// offset, so a field inserted in the middle silently repoints every load.
func TestProgArgsLayout(t *testing.T) {
	var p ProgArgs
	for _, c := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Col", unsafe.Offsetof(p.Col), POffCol},
		{"N", unsafe.Offsetof(p.N), POffN},
		{"Regs", unsafe.Offsetof(p.Regs), POffRegs},
		{"Out", unsafe.Offsetof(p.Out), POffOut},
		{"Overflow", unsafe.Offsetof(p.Overflow), POffOverflow},
		{"Sel", unsafe.Offsetof(p.Sel), POffSel},
		{"PC", unsafe.Offsetof(p.PC), POffPC},
	} {
		if c.got != c.want {
			t.Errorf("ProgArgs.%s is at %d, generated code reads %d", c.name, c.got, c.want)
		}
	}
}

// A compiled program must compute what Go computes, for shapes the fixed
// kernels cannot express at all: three predicates, an OR, and a sum.
//
// This is the whole point of the program JIT. EmitFilterCount answers one
// shape; these are four, and none of them needed a new emitter.
func TestEmittedProgramMatchesGo(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	const n = 5000
	rng := rand.New(rand.NewSource(7))
	a := make([]int64, n)
	b := make([]int64, n)
	c := make([]int64, n)
	for i := range a {
		a[i] = int64(rng.Intn(200) - 100)
		b[i] = int64(rng.Intn(10))
		c[i] = int64(rng.Intn(1000) - 500)
	}

	// Register file: slots 0..3 scratch, 4..6 hold the bound constants.
	const (
		rA, rB, rC   = 0, 1, 2
		rT1, rT2     = 3, 7
		rK0, rK1, r2 = 4, 5, 6
	)
	run := func(insns []ProgInsn, nCols int, k0, k1, k2 int64, cols ...[]int64) (int64, int64) {
		code, err := EmitProgram(insns, nCols)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		kern, err := Map(code)
		if err != nil {
			t.Fatalf("map: %v", err)
		}
		defer kern.Close()
		regs := make([]int64, 16)
		regs[rK0], regs[rK1], regs[r2] = k0, k1, k2
		var out, ovf int64
		args := &ProgArgs{N: int64(n), Regs: &regs[0], Out: &out, Overflow: &ovf}
		for i, col := range cols {
			args.Col[i] = &col[0]
		}
		kern.Call2(args)
		return out, ovf
	}

	// 1. THREE predicates: a > k0 AND b <> k1 AND c < k2.
	insns := []ProgInsn{
		{Op: POpLoadCol, A: rA, B: 0},
		{Op: POpCmp, A: rT1, B: rA, C: rK0, Cond: CondG},
		{Op: POpSkipIfZero, A: rT1},
		{Op: POpLoadCol, A: rB, B: 1},
		{Op: POpCmp, A: rT1, B: rB, C: rK1, Cond: CondNE},
		{Op: POpSkipIfZero, A: rT1},
		{Op: POpLoadCol, A: rC, B: 2},
		{Op: POpCmp, A: rT1, B: rC, C: r2, Cond: CondL},
		{Op: POpSkipIfZero, A: rT1},
		{Op: POpAccCount},
	}
	got, _ := run(insns, 3, 0, 7, 100, a, b, c)
	want := int64(0)
	for i := range a {
		if a[i] > 0 && b[i] != 7 && c[i] < 100 {
			want++
		}
	}
	if got != want {
		t.Errorf("three predicates: program %d, Go %d", got, want)
	}

	// 2. An OR, which no fixed kernel can express.
	insns = []ProgInsn{
		{Op: POpLoadCol, A: rA, B: 0},
		{Op: POpCmp, A: rT1, B: rA, C: rK0, Cond: CondG},
		{Op: POpLoadCol, A: rB, B: 1},
		{Op: POpCmp, A: rT2, B: rB, C: rK1, Cond: CondE},
		{Op: POpOr, A: rT1, B: rT1, C: rT2},
		{Op: POpSkipIfZero, A: rT1},
		{Op: POpAccCount},
	}
	got, _ = run(insns, 2, 50, 3, 0, a, b)
	want = 0
	for i := range a {
		if a[i] > 50 || b[i] == 3 {
			want++
		}
	}
	if got != want {
		t.Errorf("OR: program %d, Go %d", got, want)
	}

	// 3. A SUM over a filter.
	insns = []ProgInsn{
		{Op: POpLoadCol, A: rA, B: 0},
		{Op: POpCmp, A: rT1, B: rA, C: rK0, Cond: CondG},
		{Op: POpSkipIfZero, A: rT1},
		{Op: POpLoadCol, A: rC, B: 1},
		{Op: POpAccSum, A: rC},
	}
	got, ovf := run(insns, 2, 0, 0, 0, a, c)
	want = 0
	for i := range a {
		if a[i] > 0 {
			want += c[i]
		}
	}
	if got != want || ovf != 0 {
		t.Errorf("sum: program %d (overflow %d), Go %d", got, ovf, want)
	}

	// 4. An overflowing sum must REPORT it, not wrap -- SQLite switches to
	//    floating point there and this path has to decline instead.
	big := make([]int64, n)
	for i := range big {
		big[i] = math.MaxInt64 / 4
	}
	insns = []ProgInsn{
		{Op: POpLoadCol, A: rC, B: 0},
		{Op: POpAccSum, A: rC},
	}
	_, ovf = run(insns, 1, 0, 0, 0, big)
	if ovf == 0 {
		t.Error("an overflowing sum reported no overflow")
	}
}
