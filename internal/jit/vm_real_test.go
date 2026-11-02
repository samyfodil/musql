//go:build amd64 && (unix || windows)

package jit

import (
	"math"
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
