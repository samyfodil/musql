//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"unsafe"

	"github.com/samyfodil/musql/internal/fir"
	"github.com/samyfodil/musql/internal/libm"
)

// runFir runs k over xs (and ys): each answer, and whether Go was left it.
func runFir(k *Code, xs, ys []float64) ([]float64, []bool) {
	n := len(xs)
	if ys == nil {
		ys = make([]float64, n)
	}
	out := make([]float64, n)
	exits := make([]int64, n)
	args := &ProgArgs{N: int64(n)}
	args.Col[0] = (*int64)(unsafe.Pointer(&xs[0]))
	args.Col[1] = (*int64)(unsafe.Pointer(&ys[0]))
	args.Col[2] = (*int64)(unsafe.Pointer(&out[0]))
	args.Col[3] = &exits[0]
	k.Call2(args)
	ex := make([]bool, n)
	for i, e := range exits {
		ex[i] = e != 0
	}
	return out, ex
}

// firNeeds reports whether f needs an instruction this CPU lacks.
func firNeeds(f *fir.Func) bool {
	round := firHasOp(f.Body, fir.OpRoundEven) || firHasOp(f.Body, fir.OpFloor) || firHasOp(f.Body, fir.OpCeil) ||
		firHasOp(f.Body, fir.OpTrunc)
	return !cpuFeat.fma && firHasOp(f.Body, fir.OpFMA) || !cpuFeat.round && round
}

func firInputs(name string, n int) []float64 {
	r := rand.New(rand.NewPCG(3, uint64(len(name))))
	xs := make([]float64, 0, n)
	for len(xs) < n {
		switch r.IntN(4) {
		case 0:
			xs = append(xs, math.Float64frombits(r.Uint64()))
		case 1:
			xs = append(xs, (r.Float64()-0.5)*2000)
		case 2:
			xs = append(xs, math.Ldexp(1+r.Float64(), r.IntN(2046)-1022))
		default:
			xs = append(xs, 0.9+0.2*r.Float64())
		}
	}
	return append(xs, 0, -0.0, 1, -1, math.Inf(1), math.Inf(-1), math.NaN(), 0x1p-1074, math.MaxFloat64)
}

// TestFirKernels runs every kernel as machine code and holds it to the
// Go function: the same bits wherever the kernel answers, in each glibc
// build this CPU can run.
func TestFirKernels(t *testing.T) {
	builds := []bool{false}
	if cpuFeat.fma {
		builds = append(builds, true)
	}
	names := make([]string, 0, len(libm.Kernels1))
	for name := range libm.Kernels1 {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, fused := range builds {
		restore := libm.SetFusedForTest(fused)
		for _, name := range names {
			f, err := libm.Build1(name)
			if err != nil {
				t.Fatal(err)
			}
			if firNeeds(f) {
				t.Logf("fused=%v %s: not on this CPU", fused, name)
				continue
			}
			k, err := firLoop(f, 0)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			xs := firInputs(name, 300000)
			got, exited := runFir(k, xs, nil)
			left := 0
			for i, x := range xs {
				if exited[i] {
					left++
					continue
				}
				want := libm.Kernels1[name].Go(x)
				if math.Float64bits(got[i]) != math.Float64bits(want) {
					t.Errorf("fused=%v %s(%x) = %x, Go %x", fused, name, math.Float64bits(x), math.Float64bits(got[i]), math.Float64bits(want))
					break
				}
				if ref, ex := fir.Eval(f, x); ex || math.Float64bits(ref) != math.Float64bits(got[i]) {
					t.Errorf("fused=%v %s(%x): machine code answered where Eval did not", fused, name, math.Float64bits(x))
					break
				}
			}
			t.Logf("fused=%v %s: %d of %d left to Go", fused, name, left, len(xs))
			k.Close()
		}
		for name, kern := range libm.Kernels2 {
			f, err := libm.Build2(name)
			if err != nil {
				t.Fatal(err)
			}
			if firNeeds(f) {
				t.Logf("fused=%v %s: not on this CPU", fused, name)
				continue
			}
			k, err := firLoop(f, 0)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			xs := firInputs(name, 200000)
			ys := firInputs(name+"y", len(xs))[:len(xs)]
			for i := range ys {
				if i%3 == 0 {
					ys[i] = math.Abs(ys[i]) // pow's fast path wants a positive x
					xs[i], ys[i] = ys[i], xs[i]/1000
				}
			}
			got, exited := runFir(k, xs, ys)
			for i := range xs {
				if exited[i] {
					continue
				}
				if want := kern.Go(xs[i], ys[i]); math.Float64bits(got[i]) != math.Float64bits(want) {
					t.Errorf("fused=%v %s(%x, %x) = %x, Go %x", fused, name, math.Float64bits(xs[i]), math.Float64bits(ys[i]),
						math.Float64bits(got[i]), math.Float64bits(want))
					break
				}
			}
			k.Close()
		}
		restore()
	}
}

// A kernel that needs an instruction the CPU lacks is refused, not emitted.
func TestKernelOKRefusesMissingFeatures(t *testing.T) {
	build := func(fused bool, step func(b *fir.B, x fir.Val) fir.Val) *fir.Func {
		b := fir.New("feat", 1, fused)
		x := b.Arg(0)
		b.Return(step(b, x))
		f, err := b.Func()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	fma := build(false, func(b *fir.B, x fir.Val) fir.Val { return b.FMA(x, x, x) })
	round := build(false, func(b *fir.B, x fir.Val) fir.Val { return b.Floor(x) })
	fused := build(true, func(b *fir.B, x fir.Val) fir.Val { return b.MAdd(x, x, x) })
	plain := build(false, func(b *fir.B, x fir.Val) fir.Val { return b.MAdd(x, x, x) })
	if cpuFeat.fma && cpuFeat.round && (!KernelOK(fma) || !KernelOK(round) || !KernelOK(fused)) {
		t.Error("a kernel this CPU can run was refused")
	}
	restore := SetCPUForTest(false, false)
	defer restore()
	for name, f := range map[string]*fir.Func{"fma": fma, "round": round, "fused": fused} {
		if firFeaturesOptional && KernelOK(f) {
			t.Errorf("%s: accepted without the instruction", name)
		}
	}
	if !KernelOK(plain) {
		t.Error("an unfused multiply-add needs nothing")
	}
}

// TestFirFMA: a kernel's fma is math.FMA's, rounded once, wherever it
// answers: over random operands, over sums that cancel to the product's
// rounding error, and over ties. A backend that computes fma from doubles
// may leave an input to Go, but not one in the ordinary range.
func TestFirFMA(t *testing.T) {
	if !cpuFeat.fma {
		t.Skip("no fma here")
	}
	cs := map[string]func(b *fir.B, x, y fir.Val) fir.Val{
		"k":       func(b *fir.B, x, y fir.Val) fir.Val { return b.K(0.3) },
		"zero":    func(b *fir.B, x, y fir.Val) fir.Val { return b.K(0) },
		"-xy":     func(b *fir.B, x, y fir.Val) fir.Val { return b.FNeg(b.FMul(x, y)) },
		"x":       func(b *fir.B, x, y fir.Val) fir.Val { return x },
		"-x*1e-9": func(b *fir.B, x, y fir.Val) fir.Val { return b.FMul(x, b.K(-1e-9)) },
		"huge":    func(b *fir.B, x, y fir.Val) fir.Val { return b.K(0x1p1020) },
		"wide":    func(b *fir.B, x, y fir.Val) fir.Val { return b.FMul(y, b.K(0x1p-300)) },
	}
	r := rand.New(rand.NewPCG(5, 6))
	var xs, ys []float64
	for range 100000 {
		x, y := (r.Float64()-0.5)*math.Pow(2, float64(r.IntN(60)-30)), (r.Float64()-0.5)*math.Pow(2, float64(r.IntN(60)-30))
		if r.IntN(4) == 0 {
			// Ties: products whose low half is a half ulp of c's.
			x, y = float64(1+r.Int64N(1<<26)), float64(1+r.Int64N(1<<27))*0x1p-26
		}
		xs, ys = append(xs, x), append(ys, y)
	}
	n := len(xs)
	for range 100000 {
		// Anywhere: what is not left to Go must still be exact.
		xs = append(xs, math.Ldexp(r.Float64()-0.5, r.IntN(2100)-1050))
		ys = append(ys, math.Ldexp(r.Float64()-0.5, r.IntN(2100)-1050))
	}
	special := []float64{0, math.Copysign(0, -1), 1, -1, math.Inf(1), math.NaN(), 0x1p-1074, math.MaxFloat64, 0x1p-500, 0x1p500}
	for _, x := range special {
		for _, y := range special {
			xs, ys = append(xs, x), append(ys, y)
		}
	}
	for name, c := range cs {
		b := fir.New("fma", 2, false)
		x, y := b.Arg(0), b.Arg(1)
		b.Return(b.FMA(x, y, c(b, x, y)))
		f, err := b.Func()
		if err != nil {
			t.Fatal(err)
		}
		k, err := firLoop(f, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, exited := runFir(k, xs, ys)
		left := 0
		for i := range xs {
			want, _ := fir.Eval(f, xs[i], ys[i])
			if exited[i] {
				left++
				if !firExitsMore || i < n && name != "huge" {
					t.Fatalf("%s: fma(%v, %v) left to Go", name, xs[i], ys[i])
				}
				continue
			}
			if math.Float64bits(got[i]) != math.Float64bits(want) && !(got[i] != got[i] && want != want) {
				t.Fatalf("%s: fma(%x, %x) = %x, math.FMA %x", name, math.Float64bits(xs[i]), math.Float64bits(ys[i]),
					math.Float64bits(got[i]), math.Float64bits(want))
			}
		}
		t.Logf("%s: %d of %d left to Go", name, left, len(xs))
		k.Close()
	}
}

// POpFCmp under every condition, NULL answered as Imm.
func TestFloatProgramCompares(t *testing.T) {
	xs := []float64{1, 2, 2, math.NaN(), -0.0}
	ys := []float64{2, 1, 2, 1, 0}
	for _, c := range []Cond{CondE, CondNE, CondL, CondLE, CondG, CondGE} {
		insns := []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpLoadCol, A: 1, B: 1},
			{Op: POpFCmp, A: 2, B: 0, C: 1, Cond: c, Imm: 1},
			{Op: POpService, A: 0},
		}
		for i := range xs {
			var got int64
			floatRun(t, insns, [2]float64{}, func(regs []int64, row int) { got = regs[2] }, xs[i:i+1], ys[i:i+1])
			x, y := xs[i], ys[i]
			want := map[Cond]bool{CondE: x == y, CondNE: x != y, CondL: x < y, CondLE: x <= y, CondG: x > y, CondGE: x >= y}[c]
			if x != x {
				want = true // the NULL answer
			}
			if (got == 1) != want {
				t.Errorf("cond %x on (%v, %v): %d", c, x, y, got)
			}
		}
	}
}

// fmod is exact, so the kernel's must be math.Mod's, a NaN for a NaN, where
// it does not leave the input to Go.
func TestFirFMod(t *testing.T) {
	b := fir.New("fmod", 2, false)
	b.Return(b.FMod(b.Arg(0), b.Arg(1)))
	f, err := b.Func()
	if err != nil {
		t.Fatal(err)
	}
	for budget := range firBudgets {
		k, err := firLoop(f, budget)
		if err != nil {
			t.Fatal(err)
		}
		r := rand.New(rand.NewPCG(21, uint64(budget)))
		special := []float64{0, math.Copysign(0, -1), 1, -1, 7, -7, 0.5, math.Inf(1), math.Inf(-1), math.NaN(),
			0x1p-1074, -0x1p-1074, 0x1p-1022, math.MaxFloat64, -math.MaxFloat64, 1e300, 3e-300}
		var xs, ys []float64
		for _, x := range special {
			for _, y := range special {
				xs, ys = append(xs, x), append(ys, y)
			}
		}
		for range 100000 {
			x := math.Float64frombits(r.Uint64())
			y := math.Float64frombits(r.Uint64())
			switch r.IntN(4) {
			case 0:
				x, y = (r.Float64()-0.5)*1e6, (r.Float64()-0.5)*100
			case 1:
				// n*y rounded: x/y is n, or rounds to it from below.
				y = (r.Float64() - 0.5) * 100
				x = float64(r.Int64N(1<<53)>>r.IntN(53)) * y
				x = math.Nextafter(x, math.Inf(r.IntN(2)*2-1))
			}
			xs, ys = append(xs, x), append(ys, y)
		}
		got, exited := runFir(k, xs, ys)
		roundedUp, extra := 0, 0
		for i := range xs {
			want := math.Mod(xs[i], ys[i])
			if exited[i] && firExitsMore && !firFModExits(xs[i], ys[i]) {
				extra++
				continue
			}
			if exited[i] != firFModExits(xs[i], ys[i]) ||
				!exited[i] && math.Float64bits(got[i]) != math.Float64bits(want) && !(got[i] != got[i] && want != want) {
				t.Fatalf("fmod(%v, %v) = %v (%x, exit %v), math.Mod %v (%x)", xs[i], ys[i], got[i], math.Float64bits(got[i]),
					exited[i], want, math.Float64bits(want))
			}
			if q := math.Abs(xs[i] / ys[i]); q < 1<<53 && q == math.Trunc(q) && math.Abs(want) > math.Abs(ys[i])/2 {
				roundedUp++
			}
		}
		if roundedUp == 0 {
			t.Error("no quotient rounded up to an integer")
		}
		if extra > len(xs)/20 {
			t.Errorf("%d of %d left to Go by the fma", extra, len(xs))
		}
		k.Close()
	}
}
