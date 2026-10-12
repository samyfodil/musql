//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"slices"
	"testing"
)

// progResult is everything a program's caller can observe.
type progResult struct {
	out, ovf int64
	counters []int64 // the accumulate steps' row counters, in program order
	sel      []int64 // POpEmitRow's selection
}

// interpProgram runs insns the way their contract says, in Go: the reference
// the emitted code, fused and not, is held to.
func interpProgram(insns []ProgInsn, cols [][]int64, n int, regs []int64) progResult {
	var acc, ovf int64
	for _, in := range insns {
		switch in.Op {
		case POpAccMin:
			acc = math.MaxInt64
		case POpAccMax:
			acc = math.MinInt64
		}
	}
	var sel []int64
	cmp := func(c Cond, x, y int64) bool {
		switch c {
		case CondE:
			return x == y
		case CondNE:
			return x != y
		case CondL:
			return x < y
		case CondGE:
			return x >= y
		case CondLE:
			return x <= y
		default:
			return x > y
		}
	}
	b2i := func(b bool) int64 {
		if b {
			return 1
		}
		return 0
	}
	for row := 0; row < n; row++ {
	body:
		for pc := 0; pc < len(insns); {
			in := insns[pc]
			pc++
			jump := func(t int) bool {
				if t == ProgNextRow || t >= len(insns) {
					return false
				}
				pc = t
				return true
			}
			operandB := func() int64 {
				if in.BCol {
					return cols[in.B][row]
				}
				return regs[in.B]
			}
			switch in.Op {
			case POpLoadCol:
				regs[in.A] = cols[in.B][row]
			case POpLoadReg:
				regs[in.A] = regs[in.B]
			case POpCmp:
				regs[in.A] = b2i(cmp(in.Cond, regs[in.B], regs[in.C]))
			case POpAnd:
				regs[in.A] = regs[in.B] & regs[in.C]
			case POpOr:
				regs[in.A] = regs[in.B] | regs[in.C]
			case POpNot:
				regs[in.A] = b2i(regs[in.B] == 0)
			case POpSetConst:
				regs[in.A] = int64(in.B)
			case POpSkipIfZero:
				if regs[in.A] == 0 {
					break body
				}
			case POpJump:
				if !jump(in.A) {
					break body
				}
			case POpJumpIfZero, POpJumpIfNotZero:
				if (regs[in.B] == 0) == (in.Op == POpJumpIfZero) && !jump(in.A) {
					break body
				}
			case POpCmpSkip:
				if !cmp(in.Cond, operandB(), regs[in.C]) {
					break body
				}
			case POpCmpJump:
				if cmp(in.Cond, operandB(), regs[in.C]) && !jump(in.A) {
					break body
				}
			case POpAdd, POpSub, POpMul:
				x, y := regs[in.B], regs[in.C]
				var r int64
				var o bool
				switch in.Op {
				case POpAdd:
					r = x + y
					o = (x >= 0) == (y >= 0) && (r >= 0) != (x >= 0)
				case POpSub:
					r = x - y
					o = (x >= 0) != (y >= 0) && (r >= 0) != (x >= 0)
				default:
					hi, lo := bits.Mul64(uint64(x), uint64(y))
					r = int64(lo)
					// Signed overflow: the high word is not the sign extension.
					shi := int64(hi)
					if x < 0 {
						shi -= y
					}
					if y < 0 {
						shi -= x
					}
					o = shi != r>>63
				}
				regs[in.A] = r
				if o {
					ovf = 1
				}
			case POpAccCount:
				acc++
			case POpAccSum:
				v := regs[in.A]
				s := acc + v
				if (acc >= 0) == (v >= 0) && (s >= 0) != (acc >= 0) {
					ovf = 1
				}
				acc = s
				regs[in.B]++
			case POpAccMin:
				acc = min(acc, regs[in.A])
				regs[in.B]++
			case POpAccMax:
				acc = max(acc, regs[in.A])
				regs[in.B]++
			case POpEmitRow:
				sel = append(sel, int64(row-n))
				acc++
			default:
				panic(fmt.Sprintf("interpProgram: op %d", in.Op))
			}
		}
	}
	return progResult{out: acc, ovf: ovf, sel: sel}
}

// randProgram builds a random program of the shapes the engine lowers:
// loads, comparisons, boolean logic, arithmetic, forward branches, one kind
// of accumulate. Registers 8..11 are constants the caller parks.
func randProgram(rng *rand.Rand, nCols int) []ProgInsn {
	n := 3 + rng.Intn(14)
	reg := func() int { return rng.Intn(12) }
	tmp := func() int { return rng.Intn(8) }
	conds := []Cond{CondE, CondNE, CondL, CondGE, CondLE, CondG}
	accKind := []POp{POpAccCount, POpAccSum, POpAccMin, POpAccMax, POpEmitRow}[rng.Intn(5)]
	counter := 12 // the accumulate's row counter, outside every other range
	// A quarter of the programs are only conditions and their tests, the
	// shape the condition fusion takes whole.
	condOnly := rng.Intn(4) == 0
	if condOnly {
		n = 1 + rng.Intn(3)
		accKind = []POp{POpAccCount, POpEmitRow}[rng.Intn(2)]
	}
	var p []ProgInsn
	for i := 0; i < n; i++ {
		target := func() int {
			if rng.Intn(4) == 0 {
				return ProgNextRow
			}
			// Forward of where this instruction lands; past the end means
			// the end of the row.
			return len(p) + 1 + rng.Intn(n-i+2)
		}
		if condOnly || rng.Intn(3) == 0 {
			p = append(p, randCondTree(rng, nCols, conds)...)
			r := p[len(p)-1].A
			switch rng.Intn(3) {
			case 0:
				p = append(p, ProgInsn{Op: POpSkipIfZero, A: r})
			case 1:
				p = append(p, ProgInsn{Op: POpJumpIfZero, A: target(), B: r})
			default:
				p = append(p, ProgInsn{Op: POpJumpIfNotZero, A: target(), B: r})
			}
			continue
		}
		var in ProgInsn
		switch k := rng.Intn(13); k {
		case 0, 1:
			in = ProgInsn{Op: POpLoadCol, A: tmp(), B: rng.Intn(nCols)}
		case 2:
			in = ProgInsn{Op: POpLoadReg, A: tmp(), B: reg()}
		case 3, 4:
			in = ProgInsn{Op: POpCmp, A: tmp(), B: reg(), C: reg(), Cond: conds[rng.Intn(6)]}
		case 5:
			in = ProgInsn{Op: []POp{POpAnd, POpOr}[rng.Intn(2)], A: tmp(), B: reg(), C: reg()}
		case 6:
			in = ProgInsn{Op: POpNot, A: tmp(), B: reg()}
		case 7:
			in = ProgInsn{Op: POpSetConst, A: tmp(), B: rng.Intn(7) - 3}
		case 8, 9:
			in = ProgInsn{Op: POpSkipIfZero, A: reg()}
		case 10:
			in = ProgInsn{Op: []POp{POpJumpIfZero, POpJumpIfNotZero}[rng.Intn(2)], A: target(), B: reg()}
		case 11:
			in = ProgInsn{Op: POpJump, A: target()}
		default:
			in = ProgInsn{Op: []POp{POpAdd, POpSub, POpMul}[rng.Intn(3)], A: tmp(), B: reg(), C: reg()}
		}
		p = append(p, in)
		// The common lowered shape, often: a load, a comparison, its test.
		if rng.Intn(3) == 0 {
			r := tmp()
			p = append(p, ProgInsn{Op: POpLoadCol, A: r, B: rng.Intn(nCols)},
				ProgInsn{Op: POpCmp, A: r, B: r, C: 8 + rng.Intn(4), Cond: conds[rng.Intn(6)]},
				ProgInsn{Op: POpSkipIfZero, A: r})
		}
	}
	switch accKind {
	case POpAccSum, POpAccMin, POpAccMax:
		p = append(p, ProgInsn{Op: accKind, A: reg(), B: counter})
	default:
		p = append(p, ProgInsn{Op: accKind})
	}
	// Jump targets were drawn against the program as it grew; any past the
	// end now means "the end of the row" all the same.
	return p
}

// randCondTree is a boolean expression in post-order, the shape a WHERE
// clause's AND/OR/NOT lowers to: comparisons of loaded columns and the
// parked constants, joined, each result in its own temporary, the root last.
// Some loads share one register across terms, as the lowering's do.
func randCondTree(rng *rand.Rand, nCols int, conds []Cond) []ProgInsn {
	free := rng.Perm(12)[:8:8]
	take := func() int { r := free[0]; free = free[1:]; return r }
	var p []ProgInsn
	var gen func(depth int) int
	gen = func(depth int) int {
		if len(free) >= 7 && depth < 3 && rng.Intn(3) != 0 {
			if rng.Intn(5) == 0 {
				x := gen(depth + 1)
				r := take()
				p = append(p, ProgInsn{Op: POpNot, A: r, B: x})
				return r
			}
			x := gen(depth + 1)
			y := gen(depth + 1)
			r := take()
			p = append(p, ProgInsn{Op: []POp{POpAnd, POpOr}[rng.Intn(2)], A: r, B: x, C: y})
			return r
		}
		r := take()
		b := 8 + rng.Intn(4)
		if len(free) > 0 && rng.Intn(3) != 0 {
			l := free[len(free)-1] // a register no node holds yet
			p = append(p, ProgInsn{Op: POpLoadCol, A: l, B: rng.Intn(nCols)})
			b = l
		}
		c := 8 + rng.Intn(4)
		if rng.Intn(2) == 0 {
			b, c = c, b
		}
		p = append(p, ProgInsn{Op: POpCmp, A: r, B: b, C: c, Cond: conds[rng.Intn(len(conds))]})
		return r
	}
	gen(0)
	return p
}

// TestFusionMatchesUnfused runs random programs three ways -- the Go
// reference, the emitted code unfused, the emitted code fused -- and
// requires the three to agree on everything the caller can see.
func TestFusionMatchesUnfused(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	defer SetFusion(true)
	rng := rand.New(rand.NewSource(42))
	const n = 300
	fusedSome, condsSome := 0, 0
	RegAllocStatsForTest()
	PredicatedStatsForTest()
	for trial := 0; trial < 1500; trial++ {
		nCols := 1 + rng.Intn(MaxProgCols)
		cols := make([][]int64, nCols)
		for c := range cols {
			cols[c] = make([]int64, n)
			for i := range cols[c] {
				cols[c][i] = int64(rng.Intn(9) - 4)
				if rng.Intn(50) == 0 {
					cols[c][i] = math.MaxInt64 - int64(rng.Intn(3))
				}
			}
		}
		insns := randProgram(rng, nCols)
		consts := []int64{int64(rng.Intn(9) - 4), int64(rng.Intn(9) - 4), 0, 1}
		fresh := func() []int64 {
			regs := make([]int64, 16)
			copy(regs[8:], consts)
			return regs
		}
		run := func(fuse bool) progResult {
			SetFusion(fuse)
			code, err := EmitProgram(insns, nCols)
			if err != nil {
				t.Fatalf("trial %d: emit: %v\n%v", trial, err, insns)
			}
			kern, err := Map(code)
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			defer kern.Close()
			regs := fresh()
			var out, ovf int64
			sel := make([]int64, n)
			args := &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf, Sel: &sel[0]}
			for i, c := range cols {
				args.Col[i] = &c[0]
			}
			kern.Call2(args)
			r := progResult{out: out, ovf: b2int(ovf != 0), counters: []int64{regs[12]}}
			if insns[len(insns)-1].Op == POpEmitRow {
				r.sel = sel[:out]
			}
			return r
		}
		regs := fresh()
		want := interpProgram(insns, cols, n, regs)
		want.ovf = b2int(want.ovf != 0)
		want.counters = []int64{regs[12]}
		if insns[len(insns)-1].Op != POpEmitRow {
			want.sel = nil
		}
		if len(Fuse(insns)) < len(insns) {
			fusedSome++
		}
		if _, c := fuseConds(insns); c {
			condsSome++
		}
		for _, fuse := range []bool{false, true} {
			got := run(fuse)
			if got.out != want.out || got.ovf != want.ovf || !slices.Equal(got.counters, want.counters) || !slices.Equal(got.sel, want.sel) {
				t.Fatalf("trial %d fused=%v: got %+v, want %+v\nprogram %v\nfused %v", trial, fuse, got, want, insns, Fuse(insns))
			}
		}
	}
	if allocated, spilled, _ := RegAllocStatsForTest(); allocated == 0 {
		t.Fatalf("%d registers allocated, %d left in the file: the allocator went untested", allocated, spilled)
	}
	if condsSome < 60 {
		t.Fatalf("only %d of 1500 programs had a condition fused into branches", condsSome)
	}
	if pred := PredicatedStatsForTest(); pred < 50 {
		t.Fatalf("only %d programs emitted with a predicated row: it went untested", pred)
	} else {
		t.Logf("%d programs emitted with a predicated row", pred)
	}
	if fusedSome < 500 {
		t.Fatalf("only %d of 1500 programs fused at all -- the test exercises too little", fusedSome)
	}
}

func b2int(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// TestFusionShrinksTheLoweredShape checks the shape every lowered filter has
// -- load, compare, test -- becomes one instruction.
func TestFusionShrinksTheLoweredShape(t *testing.T) {
	got := Fuse([]ProgInsn{
		{Op: POpLoadCol, A: 0, B: 1},
		{Op: POpCmp, A: 0, B: 0, C: 5, Cond: CondG},
		{Op: POpSkipIfZero, A: 0},
		{Op: POpAccCount},
	})
	want := []ProgInsn{{Op: POpCmpSkip, B: 1, BCol: true, C: 5, Cond: CondG}, {Op: POpAccCount}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("fused to %v, want %v", got, want)
	}
}

// BenchmarkProgramFusion times the shape the engine lowers most -- three
// predicates over three columns and a sum -- emitted fused and unfused, over
// 100,000 rows.
func BenchmarkProgramFusion(b *testing.B) {
	if !Available {
		b.Skip("no JIT on this platform")
	}
	defer SetFusion(true)
	const n = 100_000
	rng := rand.New(rand.NewSource(3))
	cols := make([][]int64, 4)
	for c := range cols {
		cols[c] = make([]int64, n)
		for i := range cols[c] {
			cols[c][i] = int64(rng.Intn(1000))
		}
	}
	insns := []ProgInsn{
		{Op: POpLoadCol, A: 0, B: 0},
		{Op: POpCmp, A: 0, B: 0, C: 8, Cond: CondG},
		{Op: POpSkipIfZero, A: 0},
		{Op: POpLoadCol, A: 1, B: 1},
		{Op: POpCmp, A: 1, B: 1, C: 9, Cond: CondL},
		{Op: POpSkipIfZero, A: 1},
		{Op: POpLoadCol, A: 2, B: 2},
		{Op: POpCmp, A: 2, B: 2, C: 10, Cond: CondNE},
		{Op: POpSkipIfZero, A: 2},
		{Op: POpLoadCol, A: 3, B: 3},
		{Op: POpAccSum, A: 3, B: 12},
	}
	for _, fuse := range []bool{false, true} {
		b.Run(fmt.Sprintf("fused=%v", fuse), func(b *testing.B) {
			SetFusion(fuse)
			code, err := EmitProgram(insns, 4)
			if err != nil {
				b.Fatal(err)
			}
			k, err := Map(code)
			if err != nil {
				b.Fatal(err)
			}
			defer k.Close()
			regs := make([]int64, 16)
			regs[8], regs[9], regs[10] = 200, 800, 5
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

// TestFusionCondChains checks AND, OR and NOT chains, many terms deep, fuse
// into compare-and-branches with no boolean left.
func TestFusionCondChains(t *testing.T) {
	for _, op := range []POp{POpAnd, POpOr} {
		var p []ProgInsn
		const terms = 7
		for i := 0; i < terms; i++ {
			p = append(p, ProgInsn{Op: POpLoadCol, A: 0, B: i % MaxProgCols},
				ProgInsn{Op: POpCmp, A: 1 + i, B: 0, C: 8 + i%4, Cond: CondG})
		}
		acc := 1
		for i := 1; i < terms; i++ {
			p = append(p, ProgInsn{Op: op, A: 1 + i, B: acc, C: 1 + i})
			acc = 1 + i
		}
		p = append(p, ProgInsn{Op: POpNot, A: 15, B: acc}, ProgInsn{Op: POpSkipIfZero, A: 15}, ProgInsn{Op: POpAccCount})
		got := Fuse(p)
		for _, in := range got {
			switch in.Op {
			case POpCmp, POpAnd, POpOr, POpNot, POpSkipIfZero, POpLoadCol:
				t.Fatalf("%v chain left %v:\n%v", op, in.Op, got)
			}
		}
		if len(got) != terms+1 {
			t.Fatalf("%v chain fused to %d steps, want %d:\n%v", op, len(got), terms+1, got)
		}
	}
}
