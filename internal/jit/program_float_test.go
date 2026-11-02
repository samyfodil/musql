//go:build (amd64 || arm64) && (unix || windows)

package jit

import (
	"math"
	"math/rand/v2"
	"testing"
	"unsafe"

	"github.com/samyfodil/musql/internal/libm"
)

// kbnStep is func.c's kahanBabuskaNeumaierStep.
func kbnStep(s, e *float64, r float64) {
	t := *s + r
	if math.Abs(*s) > math.Abs(r) {
		*e += (*s - t) + r
	} else {
		*e += (r - t) + *s
	}
	*s = t
}

// floatRun emits insns over the float blocks cols, runs them with acc as
// FAcc, and answers the register file, Out, the overflow flag and FAcc.
// svc answers a POpMath service for the row it stopped on.
func floatRun(t *testing.T, insns []ProgInsn, acc [2]float64, svc func(regs []int64, row int), cols ...[]float64) ([]int64, int64, int64, [2]float64) {
	t.Helper()
	code, err := EmitProgram(insns, len(cols))
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	kern, err := Map(code)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	defer kern.Close()
	regs := make([]int64, 16)
	var out, ovf int64
	args := &ProgArgs{N: int64(len(cols[0])), Regs: &regs[0], Out: &out, Overflow: &ovf, FAcc: acc}
	for i, c := range cols {
		args.Col[i] = (*int64)(unsafe.Pointer(&c[0]))
	}
	for {
		kern.Call2(args)
		if args.PC == 0 {
			break
		}
		if svc == nil {
			t.Fatalf("stopped for service %d", args.PC-1)
		}
		svc(regs, int(args.N+args.Row))
	}
	return regs, out, ovf, args.FAcc
}

func fbits(x float64) int64 { return int64(math.Float64bits(x)) }

func TestFloatProgramAccumulates(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	n := 4000
	xs := make([]float64, n)
	for i := range xs {
		switch r.IntN(10) {
		case 0:
			xs[i] = math.NaN() // NULL
		case 1:
			xs[i] = math.Copysign(0, float64(r.IntN(2)*2-1))
		case 2:
			xs[i] = 1e300 * (r.Float64() - 0.5)
		default:
			xs[i] = (r.Float64() - 0.5) * math.Ldexp(1, r.IntN(80)-40)
		}
	}
	// sum: the same steps as SQLite, in row order.
	insns := []ProgInsn{{Op: POpLoadCol, A: 0, B: 0}, {Op: POpFAccSum, A: 0, B: 1}}
	regs, _, _, acc := floatRun(t, insns, [2]float64{}, nil, xs)
	var s, e float64
	rows := int64(0)
	for _, x := range xs {
		if x == x {
			kbnStep(&s, &e, x)
			rows++
		}
	}
	if math.Float64bits(acc[0]) != math.Float64bits(s) || math.Float64bits(acc[1]) != math.Float64bits(e) || regs[1] != rows {
		t.Errorf("sum: kernel (%v, %v, %d), Go (%v, %v, %d)", acc[0], acc[1], regs[1], s, e, rows)
	}
	// A second call goes on from the first's state.
	_, _, _, acc2 := floatRun(t, insns, acc, nil, xs[:100])
	for _, x := range xs[:100] {
		if x == x {
			kbnStep(&s, &e, x)
		}
	}
	if acc2[0] != s || acc2[1] != e {
		t.Errorf("resumed sum: kernel (%v, %v), Go (%v, %v)", acc2[0], acc2[1], s, e)
	}

	// min and max keep the first of equal values: -0 and +0 tie.
	for _, op := range []POp{POpFAccMin, POpFAccMax} {
		seed := math.Inf(1)
		if op == POpFAccMax {
			seed = math.Inf(-1)
		}
		ys := []float64{math.NaN(), math.Copysign(0, -1), 0, math.NaN(), math.Copysign(0, -1)}
		_, _, _, got := floatRun(t, []ProgInsn{{Op: POpLoadCol, A: 0, B: 0}, {Op: op, A: 0, B: 1}}, [2]float64{seed}, nil, ys)
		if math.Float64bits(got[0]) != math.Float64bits(math.Copysign(0, -1)) {
			t.Errorf("op %d over signed zeros: %v (bits %x)", op, got[0], math.Float64bits(got[0]))
		}
		_, _, _, got = floatRun(t, []ProgInsn{{Op: POpLoadCol, A: 0, B: 0}, {Op: op, A: 0, B: 1}}, [2]float64{seed}, nil, xs)
		want := seed
		for _, x := range xs {
			if (op == POpFAccMin && x < want) || (op == POpFAccMax && x > want) {
				want = x
			}
		}
		if got[0] != want {
			t.Errorf("op %d: %v, want %v", op, got[0], want)
		}
	}

	// count() of a REAL counts the rows that are not NULL.
	_, out, _, _ := floatRun(t, []ProgInsn{{Op: POpLoadCol, A: 0, B: 0}, {Op: POpFAccCount, A: 0}}, [2]float64{}, nil, xs)
	if out != rows {
		t.Errorf("count: %d, want %d", out, rows)
	}
}

func TestFloatProgramArithmetic(t *testing.T) {
	neg0 := math.Copysign(0, -1)
	xs := []float64{1.5, -2, 0, neg0, math.Inf(1), math.Inf(-1), math.NaN(), 1e-310, 3}
	ys := []float64{0, neg0, 2, -3, math.Inf(1), 4, 5, 1e300, math.NaN()}
	// r2 = x/y, r3 = (x - y) * 2.5, r4 = x < y (NULL: 1), r5 = abs(x),
	// r6 = float64(int64 7), r7 = x IS NULL, then the sum of r2.
	insns := []ProgInsn{
		{Op: POpLoadCol, A: 0, B: 0},
		{Op: POpLoadCol, A: 1, B: 1},
		{Op: POpFDiv, A: 2, B: 0, C: 1},
		{Op: POpFSub, A: 3, B: 0, C: 1},
		{Op: POpSetConst, A: 8, B: int(fbits(2.5))},
		{Op: POpFMul, A: 3, B: 3, C: 8},
		{Op: POpFCmp, A: 4, B: 0, C: 1, Cond: CondL, Imm: 1},
		{Op: POpFAbs, A: 5, B: 0},
		{Op: POpSetConst, A: 9, B: 7},
		{Op: POpI2F, A: 6, B: 9},
		{Op: POpFIsNull, A: 7, B: 0},
		{Op: POpService, A: 0},
	}
	for i := range xs {
		x, y := xs[i], ys[i]
		var got []int64
		floatRun(t, insns, [2]float64{}, func(regs []int64, row int) {
			if row == 0 {
				got = append([]int64(nil), regs[:8]...)
			}
		}, []float64{x}, []float64{y})
		div := math.NaN()
		if y != 0 {
			div = x / y
		}
		lt := int64(0)
		if x < y || x != x || y != y {
			lt = 1
		}
		abs := x
		if x < 0 {
			abs = -x
		}
		isNull := int64(0)
		if x != x {
			isNull = 1
		}
		sameF := func(a int64, b float64) bool {
			fa := math.Float64frombits(uint64(a))
			return math.Float64bits(fa) == math.Float64bits(b) || (fa != fa && b != b)
		}
		if !sameF(got[2], div) || !sameF(got[3], (x-y)*2.5) || got[4] != lt || !sameF(got[5], abs) ||
			!sameF(got[6], 7) || got[7] != isNull {
			t.Errorf("x=%v y=%v: div %v sub %v lt %d abs %v i2f %v null %d", x, y,
				math.Float64frombits(uint64(got[2])), math.Float64frombits(uint64(got[3])), got[4],
				math.Float64frombits(uint64(got[5])), math.Float64frombits(uint64(got[6])), got[7])
		}
	}
	// A NULL in a comparison that cannot hold one declines.
	_, _, ovf, _ := floatRun(t, []ProgInsn{
		{Op: POpLoadCol, A: 0, B: 0},
		{Op: POpFCmp, A: 1, B: 0, C: 0, Cond: CondE, Imm: ProgNullDeclines},
	}, [2]float64{}, nil, []float64{1, math.NaN()})
	if ovf == 0 {
		t.Error("a NULL compared under ProgNullDeclines did not raise the flag")
	}
}

// A kernel's Exit stops the program for its service, which answers in Go,
// and the program resumes on the same row.
func TestFloatProgramMathService(t *testing.T) {
	f, err := libm.Build1("exp")
	if err != nil {
		t.Fatal(err)
	}
	xs := []float64{0.5, 1e-300, -2, 2000, 3}
	insns := []ProgInsn{
		{Op: POpLoadCol, A: 0, B: 0},
		{Op: POpMath, A: 1, B: 0, Kernel: f, Imm: 0},
		{Op: POpFAccSum, A: 1, B: 2},
	}
	stops := 0
	_, _, _, acc := floatRun(t, insns, [2]float64{}, func(regs []int64, row int) {
		stops++
		regs[1] = fbits(libm.Exp(xs[row]))
	}, xs)
	var s, e float64
	for _, x := range xs {
		kbnStep(&s, &e, libm.Exp(x))
	}
	if math.Float64bits(acc[0]) != math.Float64bits(s) || math.Float64bits(acc[1]) != math.Float64bits(e) {
		t.Errorf("sum(exp(x)) = (%v, %v), want (%v, %v)", acc[0], acc[1], s, e)
	}
	if stops != 2 {
		t.Errorf("%d stops, want 2 (the tiny and the huge x)", stops)
	}
}
