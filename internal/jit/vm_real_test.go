//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math"
	"math/big"
	"testing"

	"github.com/samyfodil/musql/internal/fir"
)

func treal(f float64) tvalue { return tvalue{Typ: tvReal, F: f} }
func tint(i int64) tvalue    { return tvalue{Typ: tvInt, I: i} }

// same is a == b, a REAL to the bit.
func same(a, b tvalue) bool {
	return a.Typ == b.Typ && a.I == b.I && math.Float64bits(a.F) == math.Float64bits(b.F) && string(a.S) == string(b.S)
}

// TestVMRealArith: the REAL arms compute what Go does and exit, writing
// nothing, on everything the VDBE must answer.
func TestVMRealArith(t *testing.T) {
	if !Available || !VMReal {
		t.Skip("no REAL arms here")
	}
	ops := []struct {
		op VOp
		fn func(b, a float64) float64
	}{
		{VFAdd, func(b, a float64) float64 { return b + a }},
		{VFSub, func(b, a float64) float64 { return b - a }},
		{VFMul, func(b, a float64) float64 { return b * a }},
		{VFDiv, func(b, a float64) float64 { return b / a }},
	}
	vals := []tvalue{treal(2.5), treal(math.Copysign(0, -1)), treal(1e308), treal(math.Inf(1)), tint(3), tint(0), tint(-1 << 62),
		{Typ: tvNull}, {Typ: tvText, S: []byte("1")}, treal(0)}
	for _, o := range ops {
		for _, b := range vals {
			for _, av := range vals {
				regs := []tvalue{av, b, tint(77)}
				args := &ProgArgs{}
				runVM(t, []VInsn{{Op: o.op, A: 0, B: 1, C: 2}, {Op: VExit}}, nil, regs, args, 0)
				num := func(v tvalue) (float64, bool) {
					switch v.Typ {
					case tvReal:
						return v.F, true
					case tvInt:
						return float64(v.I), true
					}
					return 0, false
				}
				bf, bok := num(b)
				af, aok := num(av)
				if b.Typ == tvInt && av.Typ == tvInt && o.op != VFDiv && VMRealInts {
					// Two INTEGERs are the INTEGER arm: an INTEGER, or an exit on
					// overflow.
					x, y := big.NewInt(b.I), big.NewInt(av.I)
					switch o.op {
					case VFAdd:
						x.Add(x, y)
					case VFSub:
						x.Sub(x, y)
					default:
						x.Mul(x, y)
					}
					switch {
					case !x.IsInt64() && (args.PC != 0 || !same(regs[2], tint(77))):
						t.Errorf("op %d (%+v, %+v): pc %d r[2] %+v, want an exit on overflow", o.op, b, av, args.PC, regs[2])
					case x.IsInt64() && (args.PC != 1 || !same(regs[2], tint(x.Int64()))):
						t.Errorf("op %d (%+v, %+v): pc %d r[2] %+v, want INTEGER %d", o.op, b, av, args.PC, regs[2], x.Int64())
					}
					continue
				}
				exit := !bok || !aok || (b.Typ == tvInt && av.Typ == tvInt) || (o.op == VFDiv && af == 0)
				var want float64
				if !exit {
					want = o.fn(bf, af)
					exit = want != want
				}
				if exit {
					if args.PC != 0 || !same(regs[2], tint(77)) {
						t.Errorf("op %d (%+v, %+v): pc %d r[2] %+v, want an exit writing nothing", o.op, b, av, args.PC, regs[2])
					}
					continue
				}
				if args.PC != 1 || regs[2].Typ != tvReal || math.Float64bits(regs[2].F) != math.Float64bits(want) || regs[2].I != 0 {
					t.Errorf("op %d (%+v, %+v): pc %d r[2] %+v, want %v", o.op, b, av, args.PC, regs[2], want)
				}
			}
		}
	}
	// A run that knows its REALs: a literal, then arithmetic on it.
	regs := []tvalue{{}, {}, {}, tint(4)}
	prog := []VInsn{
		{Op: VReal, A: 0, Imm: int64(math.Float64bits(1.5))},
		{Op: VFAdd, A: 0, B: 0, C: 1},
		{Op: VFMul, A: 3, B: 1, C: 2},
		{Op: VExit},
	}
	args := &ProgArgs{}
	runVM(t, prog, []bool{true, false, false, false}, regs, args, 0)
	if args.PC != 3 || !same(regs[0], treal(1.5)) || !same(regs[1], treal(3)) || !same(regs[2], treal(12)) {
		t.Errorf("known run: pc %d regs %+v", args.PC, regs[:3])
	}
}

// TestVMRealCompare: VFCmpJump, VFCmpSet and VFCmpBr over REALs and the
// INTEGERs a double holds exactly; a larger INTEGER, a NULL or a NaN exits.
func TestVMRealCompare(t *testing.T) {
	if !Available || !VMReal {
		t.Skip("no REAL arms here")
	}
	vals := []tvalue{treal(2.5), treal(math.Copysign(0, -1)), treal(0), tint(2), tint(3), tint(1 << 53), tint(1<<53 + 1), treal(math.Inf(-1)),
		treal(math.NaN()), {Typ: tvNull}}
	conds := []Cond{CondE, CondNE, CondL, CondLE, CondG, CondGE}
	for _, c := range conds {
		for _, b := range vals {
			for _, av := range vals {
				ok := func(v tvalue) bool {
					return (v.Typ == tvReal && !math.IsNaN(v.F)) || (v.Typ == tvInt && v.I <= 1<<53 && v.I >= -(1<<53))
				}
				f := func(v tvalue) float64 {
					if v.Typ == tvInt {
						return float64(v.I)
					}
					return v.F
				}
				exit := !ok(b) || !ok(av)
				var holds bool
				if !exit {
					x, y := f(b), f(av)
					holds = map[Cond]bool{CondE: x == y, CondNE: x != y, CondL: x < y, CondLE: x <= y, CondG: x > y, CondGE: x >= y}[c]
				}
				// The jump form.
				regs := []tvalue{av, b}
				args := &ProgArgs{}
				runVM(t, []VInsn{{Op: VFCmpJump, A: 0, B: 1, T: 2, Cond: c}, {Op: VExit}, {Op: VExit}}, nil, regs, args, 0)
				want := int64(1)
				if holds {
					want = 2
				}
				if exit {
					want = 0
				}
				if args.PC != want {
					t.Errorf("jump %x (%+v, %+v): pc %d, want %d", c, b, av, args.PC, want)
				}
				// The store form, and fused with its test.
				regs = []tvalue{av, b, tint(77)}
				args = &ProgArgs{}
				runVM(t, []VInsn{{Op: VFCmpSet, A: 0, B: 1, C: 2, Cond: c}, {Op: VExit}}, nil, regs, args, 0)
				if exit {
					if args.PC != 0 || !same(regs[2], tint(77)) {
						t.Errorf("set %x (%+v, %+v): pc %d r[2] %+v, want an exit", c, b, av, args.PC, regs[2])
					}
				} else if args.PC != 1 || !same(regs[2], tint(map[bool]int64{true: 1, false: 0}[holds])) {
					t.Errorf("set %x (%+v, %+v): pc %d r[2] %+v, want %v", c, b, av, args.PC, regs[2], holds)
				}
				regs = []tvalue{av, b, tint(77)}
				args = &ProgArgs{}
				prog := []VInsn{{Op: VFCmpSet, A: 0, B: 1, C: 2, Cond: c}, {Op: VIf, A: 2, T: 3, Imm: 1}, {Op: VExit}, {Op: VExit}}
				runVM(t, prog, []bool{true, false, false, false}, regs, args, 0)
				want = 2
				if holds {
					want = 3
				}
				if exit {
					want = 0
				}
				if args.PC != want {
					t.Errorf("fused %x (%+v, %+v): pc %d, want %d", c, b, av, args.PC, want)
				}
			}
		}
	}
}

// TestVMMath: VMath runs its kernel on a REAL or an INTEGER argument and
// exits on a NULL, an INTEGER where only a REAL may go, a NaN answer and
// the kernel's own Exit.
func TestVMMath(t *testing.T) {
	if !Available || !VMReal {
		t.Skip("no REAL arms here")
	}
	b := fir.New("sqrt-or-exit", 1, false)
	x := b.Arg(0)
	b.ExitIf(fir.FGt, x, b.K(1e300))
	b.Return(b.FSqrt(x))
	kern, err := b.Func()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		arg    tvalue
		imm    int64
		want   float64
		exited bool
	}{
		{treal(2.25), 0, 1.5, false},
		{tint(16), 0, 4, false},
		{tint(16), 1, 0, true},
		{tvalue{Typ: tvNull}, 0, 0, true},
		{treal(-1), 0, 0, true},    // NaN: NULL
		{treal(1e301), 0, 0, true}, // the kernel's Exit
		{tvalue{Typ: tvText, S: []byte("4")}, 0, 0, true},
	}
	for _, c := range cases {
		regs := []tvalue{c.arg, tint(77)}
		args := &ProgArgs{}
		runVM(t, []VInsn{{Op: VMath, A: 0, B: 1, C: 1, Kernel: kern, Imm: c.imm}, {Op: VExit}}, nil, regs, args, 0)
		if c.exited {
			if args.PC != 0 || !same(regs[1], tint(77)) {
				t.Errorf("%+v imm %d: pc %d r[1] %+v, want an exit", c.arg, c.imm, args.PC, regs[1])
			}
			continue
		}
		if args.PC != 1 || !same(regs[1], treal(c.want)) {
			t.Errorf("%+v imm %d: pc %d r[1] %+v, want %v", c.arg, c.imm, args.PC, regs[1], c.want)
		}
	}
}

// TestVMRealPaths: the REAL arms where the emitter knows an operand's type,
// where a result is dead, a fused test that jumps on false, a REAL compare's
// result kept in a machine register, and a VMath with no usable kernel.
func TestVMRealPaths(t *testing.T) {
	if !Available || !VMReal {
		t.Skip("no REAL arms here")
	}
	all := func(n int) []bool { return make([]bool, n) }
	entry := func(n int) []bool { e := all(n); e[0] = true; return e }
	// r0 = 2.5 (known REAL), r1 = 3 (known INTEGER), r2 = r1 + r0, a dead
	// REAL and a dead sum, then r0 < r2 kept in a register for r3 = r4 + it.
	regs := []tvalue{{}, {}, {}, {}, tint(10), {}}
	prog := []VInsn{
		{Op: VReal, A: 0, Imm: int64(math.Float64bits(2.5))},
		{Op: VInt, A: 1, Imm: 3},
		{Op: VFAdd, A: 0, B: 1, C: 2},
		{Op: VReal, A: 5, Imm: int64(math.Float64bits(9)), Dead: true},
		{Op: VFMul, A: 0, B: 2, C: 5, Dead: true},
		{Op: VFCmpSet, A: 2, B: 0, C: 3, Cond: CondL},
		{Op: VAdd, A: 3, B: 4, C: 3},
		{Op: VFCmpJump, A: 0, B: 2, T: 9, Cond: CondG},
		{Op: VExit},
		{Op: VExit},
	}
	args := &ProgArgs{}
	runVM(t, prog, entry(len(prog)), regs, args, 0)
	if args.PC != 9 || !same(regs[2], treal(5.5)) || !same(regs[3], tint(11)) {
		t.Errorf("known types: pc %d r2 %+v r3 %+v", args.PC, regs[2], regs[3])
	}
	// A fused compare that jumps when false, its result dead.
	for _, x := range []float64{1, 4} {
		regs = []tvalue{treal(x), treal(2), tint(77)}
		prog = []VInsn{
			{Op: VFCmpBr, A: 1, B: 0, C: 2, T: 3, Cond: CondG, Dead: true},
			{Op: VFused, A: 2, C: 0, T: 3},
			{Op: VExit},
			{Op: VExit},
		}
		args = &ProgArgs{}
		runVM(t, prog, entry(len(prog)), regs, args, 0)
		if want := map[bool]int64{true: 2, false: 3}[x > 2]; args.PC != want {
			t.Errorf("VFCmpBr on false, %v > 2: pc %d, want %d", x, args.PC, want)
		}
	}
	// No kernel, or one of the wrong arity: the VDBE's.
	k := fir.New("one", 1, false)
	k.Return(k.Arg(0))
	one, err := k.Func()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []VInsn{{Op: VMath, A: 0, B: 1, C: 1}, {Op: VMath, A: 0, B: 2, C: 1, Kernel: one}} {
		regs = []tvalue{treal(1), tint(77)}
		args = &ProgArgs{}
		runVM(t, []VInsn{in, {Op: VExit}}, nil, regs, args, 0)
		if args.PC != 0 || !same(regs[1], tint(77)) {
			t.Errorf("%+v: pc %d r1 %+v, want an exit", in, args.PC, regs[1])
		}
	}
	// A kernel the CPU cannot run fails the emission; wasm computes fma from
	// doubles, and so runs every one.
	k = fir.New("fma", 1, false)
	x := k.Arg(0)
	k.Return(k.FMA(x, x, x))
	fma, err := k.Func()
	if err != nil {
		t.Fatal(err)
	}
	restore := SetCPUForTest(false, false)
	_, err = EmitVM([]VInsn{{Op: VMath, A: 0, B: 1, C: 1, Kernel: fma}, {Op: VExit}}, 2, tvLayout)
	restore()
	if firFeaturesOptional && err == nil {
		t.Error("a kernel needing FMA was emitted without it")
	}
	if !firFeaturesOptional && err != nil {
		t.Errorf("a kernel with an fma: %v", err)
	}
}
