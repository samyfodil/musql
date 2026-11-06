//go:build amd64 && (unix || windows)

package jit

import (
	"math"
	"math/rand"
	"slices"
	"testing"
)

// randStraightProgram is a random program the vector loop can take: one
// straight run of loads, tests, arithmetic and logic over the registers
// written before it and the constants in 8 to 11, then the accumulates.
func randStraightProgram(rng *rand.Rand, nCols int) []ProgInsn {
	conds := []Cond{CondE, CondNE, CondL, CondGE, CondLE, CondG}
	var p []ProgInsn
	var have []int // registers written so far
	src := func() int {
		if len(have) == 0 || rng.Intn(4) == 0 {
			return 8 + rng.Intn(4)
		}
		return have[rng.Intn(len(have))]
	}
	dst := func() int {
		r := rng.Intn(8)
		if !slices.Contains(have, r) {
			have = append(have, r)
		}
		return r
	}
	for range 2 + rng.Intn(10) {
		var in ProgInsn
		switch k := rng.Intn(12); k {
		case 0, 1, 2:
			in = ProgInsn{Op: POpLoadCol, B: rng.Intn(nCols)}
		case 3:
			in = ProgInsn{Op: POpSetConst, B: []int{-3, 0, 1, 2, 7, 1 << 20}[rng.Intn(6)]}
		case 4:
			in = ProgInsn{Op: POpCmp, B: src(), C: src(), Cond: conds[rng.Intn(6)]}
		case 5:
			in = ProgInsn{Op: []POp{POpAnd, POpOr}[rng.Intn(2)], B: src(), C: src()}
		case 6:
			in = ProgInsn{Op: []POp{POpNot, POpLoadReg}[rng.Intn(2)], B: src()}
		case 7:
			// A test leaving the row, in each form the lowering and fusion
			// give one.
			switch rng.Intn(4) {
			case 0:
				p = append(p, ProgInsn{Op: POpSkipIfZero, A: src()})
			case 1:
				p = append(p, ProgInsn{Op: []POp{POpJumpIfZero, POpJumpIfNotZero}[rng.Intn(2)], A: ProgNextRow, B: src()})
			case 2:
				p = append(p, ProgInsn{Op: POpCmpJump, A: ProgNextRow, B: src(), C: src(), Cond: conds[rng.Intn(6)]})
			default:
				p = append(p, ProgInsn{Op: POpCmpSkip, B: src(), C: src(), Cond: conds[rng.Intn(6)]})
			}
			continue
		default:
			in = ProgInsn{Op: []POp{POpAdd, POpSub, POpMul, POpShl, POpShr}[rng.Intn(5)], B: src(), C: src()}
		}
		in.A = dst()
		p = append(p, in)
		if rng.Intn(3) == 0 {
			r := dst()
			p = append(p, ProgInsn{Op: POpLoadCol, A: r, B: rng.Intn(nCols)},
				ProgInsn{Op: POpCmp, A: r, B: r, C: 8 + rng.Intn(4), Cond: conds[rng.Intn(6)]},
				ProgInsn{Op: POpSkipIfZero, A: r})
		}
	}
	if rng.Intn(3) == 0 {
		for j := range 2 + rng.Intn(2) {
			op := []POp{POpRAccSum, POpRAccMin, POpRAccMax}[rng.Intn(3)]
			p = append(p, ProgInsn{Op: op, A: 13 + j, B: src()})
		}
		return append(p, ProgInsn{Op: POpRAccCount, A: 16})
	}
	switch op := []POp{POpAccCount, POpAccSum, POpAccMin, POpAccMax}[rng.Intn(4)]; op {
	case POpAccCount:
		p = append(p, ProgInsn{Op: op})
	default:
		p = append(p, ProgInsn{Op: op, A: src(), B: 12})
	}
	return p
}

// TestVecMatchesScalar holds the vector loop to the Go reference: a run it
// does not flag answers exactly as the reference does, and a run whose
// reference overflows it flags.
func TestVecMatchesScalar(t *testing.T) {
	if !cpuHasAVX2() {
		t.Skip("no AVX2")
	}
	rng := rand.New(rand.NewSource(7))
	const n = 256
	ran, vouched := 0, 0
	for trial := 0; trial < 3000; trial++ {
		nCols := 1 + rng.Intn(MaxProgCols)
		cols := make([][]int64, nCols)
		wide := rng.Intn(4) == 0
		for c := range cols {
			cols[c] = make([]int64, n)
			for i := range cols[c] {
				cols[c][i] = int64(rng.Intn(19) - 9)
				if wide && rng.Intn(40) == 0 {
					cols[c][i] = []int64{math.MaxInt64, math.MinInt64, 1 << 40, -(1 << 33), 70, -1}[rng.Intn(6)]
				}
			}
		}
		insns := randStraightProgram(rng, nCols)
		code, err := EmitProgramVec(insns, nCols)
		if err != nil {
			continue
		}
		consts := []int64{int64(rng.Intn(9) - 4), int64(rng.Intn(9) - 4), 0, 1}
		fresh := func() []int64 {
			regs := make([]int64, 20)
			copy(regs[8:], consts)
			regs[14], regs[15] = math.MaxInt64, math.MinInt64
			return regs
		}
		want := fresh()
		ref := interpProgram(deadArithAsConst(insns), cols, n, want)
		kern, err := Map(code)
		if err != nil {
			t.Fatal(err)
		}
		got := fresh()
		var out, ovf int64
		args := &ProgArgs{N: n, Regs: &got[0], Out: &out, Overflow: &ovf}
		for i, c := range cols {
			args.Col[i] = &c[0]
		}
		kern.Call2(args)
		kern.Close()
		ran++
		if ovf != 0 {
			continue // the caller runs these rows through the scalar loop
		}
		vouched++
		if ref.ovf != 0 || out != ref.out || !slices.Equal(got[12:], want[12:]) {
			t.Fatalf("trial %d: vector out %d regs %v, reference out %d regs %v overflow %d\nprogram %v\nfused %v",
				trial, out, got[12:], ref.out, want[12:], ref.ovf, insns, Fuse(insns))
		}
	}
	if ran < 1000 || vouched < ran/2 {
		t.Fatalf("%d programs vectorized, %d of them vouched for: too few to test", ran, vouched)
	}
	t.Logf("%d programs vectorized, %d vouched for", ran, vouched)
}
