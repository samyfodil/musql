//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/samyfodil/musql/internal/fir"
)

// firRandTab is a table random kernels load from.
var firRandTab = func() []uint64 {
	r := rand.New(rand.NewPCG(9, 9))
	t := make([]uint64, 64)
	for i := range t {
		t[i] = math.Float64bits((r.Float64() - 0.5) * 1000)
	}
	return t
}()

// randomKernel builds a kernel of random steps over two arguments: every op
// and comparison, nested branches, variables of both classes, exits and
// returns. A double crosses into an integer register only when it cannot be
// a NaN, whose payload the hardware and Go may pick differently.
func randomKernel(r *rand.Rand, fused bool) *fir.Func {
	b := fir.New("rand", 2, fused)
	type pool struct {
		f, clean, i []fir.Val
	}
	vf, vi := b.Var(fir.F), b.Var(fir.I)
	b.Set(vf, b.K(1.5))
	b.Set(vi, b.Const(7))
	top := &pool{}
	x, y := b.Arg(0), b.Arg(1)
	top.f = []fir.Val{x, y, vf}
	top.clean = []fir.Val{x, y}
	top.i = []fir.Val{vi}
	pick := func(vs []fir.Val) fir.Val { return vs[r.IntN(len(vs))] }
	var block func(p *pool, depth, n int)
	block = func(p *pool, depth, n int) {
		for range n {
			switch k := r.IntN(30); {
			case k < 4:
				ops := []func(a, c fir.Val) fir.Val{b.FAdd, b.FSub, b.FMul, b.FDiv}
				if firFMod {
					ops = append(ops, b.FMod)
				}
				p.f = append(p.f, ops[r.IntN(len(ops))](pick(p.f), pick(p.f)))
			case k < 6:
				ops := []func(a fir.Val) fir.Val{b.FSqrt, b.RoundEven, b.Floor, b.Ceil, b.Trunc}
				p.f = append(p.f, ops[r.IntN(len(ops))](pick(p.f)))
			case k < 7:
				v := pick(p.clean)
				w := b.FAbs(v)
				if r.IntN(2) == 0 {
					w = b.FNeg(v)
				}
				p.f, p.clean = append(p.f, w), append(p.clean, w)
			case k < 9:
				if fused || r.IntN(2) == 0 {
					p.f = append(p.f, b.MAdd(pick(p.f), pick(p.f), pick(p.f)))
				} else if cpuFeat.fma {
					p.f = append(p.f, b.FMA(pick(p.f), pick(p.f), pick(p.f)))
				}
			case k < 10:
				v := b.IToF(pick(p.i))
				p.f, p.clean = append(p.f, v), append(p.clean, v)
			case k < 11:
				v := b.K((r.Float64() - 0.5) * 100)
				p.f, p.clean = append(p.f, v), append(p.clean, v)
			case k < 12:
				v := b.LoadF(firRandTab, b.AndK(pick(p.i), 63))
				p.f, p.clean = append(p.f, v), append(p.clean, v)
			case k < 16:
				ops := []func(a, c fir.Val) fir.Val{b.Add, b.Sub, b.Mul, b.And, b.Or, b.Xor}
				a := pick(p.i)
				if r.IntN(3) == 0 {
					a = pick(p.clean) // a double's bits
				}
				c := pick(p.i)
				if r.IntN(3) == 0 {
					c = b.Const(uint64(r.Int64N(1<<40)) - 1<<39)
				}
				p.i = append(p.i, ops[r.IntN(len(ops))](a, c))
			case k < 18:
				ops := []func(a fir.Val, n int) fir.Val{b.Shl, b.Shr, b.Sar}
				p.i = append(p.i, ops[r.IntN(3)](pick(p.i), r.IntN(64)))
			case k < 19:
				p.i = append(p.i, b.FToI(pick(p.f)))
			case k < 20:
				p.i = append(p.i, b.LoadI(firRandTab, b.AndK(pick(p.i), 63)))
			case k < 21:
				p.i = append(p.i, b.Const(r.Uint64()))
			case k < 23:
				if r.IntN(2) == 0 {
					b.Set(vf, pick(p.f))
				} else {
					b.Set(vi, pick(p.i))
				}
			case k < 24:
				b.Set(vf, b.IToF(pick(p.i))) // into a variable from the other class's op
			case k < 29 && depth < 3:
				inner := func() {
					q := &pool{f: append([]fir.Val(nil), p.f...), clean: append([]fir.Val(nil), p.clean...), i: append([]fir.Val(nil), p.i...)}
					block(q, depth+1, 1+r.IntN(8))
					switch r.IntN(6) {
					case 0:
						b.Exit()
					case 1:
						b.Return(pick(q.f))
					}
				}
				var els func()
				if r.IntN(2) == 0 {
					els = inner
				}
				c := fir.Cmp(r.IntN(16))
				if c.Float() {
					b.If(c, pick(p.f), pick(p.f), inner, els)
				} else {
					b.If(c, pick(p.i), pick(p.i), inner, els)
				}
			}
		}
	}
	block(top, 0, 10+r.IntN(40))
	b.Return(pick(top.f))
	f, err := b.Func()
	if err != nil {
		panic(err)
	}
	return f
}

// TestFirRandomKernels holds the backend to the evaluator over random
// kernels, with the registers a program offers and with so few that most
// values live in spill slots.
func TestFirRandomKernels(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	n := 600
	if testing.Short() {
		n = 150
	}
	inputs := make([]float64, 0, 64)
	for range 48 {
		inputs = append(inputs, (r.Float64()-0.5)*math.Pow(10, float64(r.IntN(12)-6)))
	}
	inputs = append(inputs, 0, math.Copysign(0, -1), 1, -1, 0.5, 2.5, -2.5, 1e300, -1e-300, 3, 4096)
	refused := 0
	for k := range n {
		fused := cpuFeat.fma && k%2 == 1
		f := randomKernel(r, fused)
		code, err := firLoop(f, k%len(firBudgets))
		if err != nil {
			// Refused, not miscompiled: more live values than the slots, or an
			// instruction this CPU lacks.
			if strings.Contains(err.Error(), "spill slots") || !cpuFeat.round || !cpuFeat.fma {
				refused++
				continue
			}
			t.Fatalf("kernel %d: %v", k, err)
		}
		xs := make([]float64, 0, 2*len(inputs))
		ys := make([]float64, 0, 2*len(inputs))
		for _, x := range inputs {
			xs = append(xs, x, inputs[r.IntN(len(inputs))])
			ys = append(ys, inputs[r.IntN(len(inputs))], x)
		}
		got, exited := runFir(code, xs, ys)
		for i := range xs {
			want, wantExit := fir.Eval(f, xs[i], ys[i])
			same := math.Float64bits(got[i]) == math.Float64bits(want) || (got[i] != got[i] && want != want)
			if exited[i] != wantExit || (!wantExit && !same) {
				t.Fatalf("kernel %d (env %d) on (%v, %v): machine code %v (exit %v), Eval %v (exit %v)\n%v",
					k, k%len(firBudgets), xs[i], ys[i], got[i], exited[i], want, wantExit, f)
			}
		}
		code.Close()
	}
	if refused > n/10 {
		t.Errorf("%d of %d kernels refused", refused, n)
	}
}
