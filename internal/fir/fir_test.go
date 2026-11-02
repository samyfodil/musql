package fir

import (
	"math"
	"strings"
	"testing"
)

func TestBuilderRefusesMalformedKernels(t *testing.T) {
	cases := map[string]func(b *B){
		"a value used outside its block": func(b *B) {
			x := b.Arg(0)
			var inner Val
			b.If(FGt, x, b.K(0), func() { inner = b.FAdd(x, x) }, nil)
			b.Return(inner)
		},
		"Set of a non-variable": func(b *B) {
			x := b.Arg(0)
			b.Set(x, x)
			b.Return(x)
		},
		"an argument it does not have": func(b *B) { b.Return(b.Arg(3)) },
		"no value":                     func(b *B) { b.Return(0) },
		"an empty table":               func(b *B) { b.Return(b.LoadF(nil, b.Const(0))) },
		"a path off the end": func(b *B) {
			x := b.Arg(0)
			b.If(FGt, x, b.K(0), func() { b.Return(x) }, nil)
		},
	}
	for name, build := range cases {
		b := New(name, 1, false)
		build(b)
		if _, err := b.Func(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

var evalTab = []uint64{math.Float64bits(2.5), 7}

func TestEvalOps(t *testing.T) {
	type tc struct {
		build  func(b *B, x, y Val) Val
		x, y   float64
		want   float64
		bits   uint64 // checked instead of want when non-zero
		exited bool
		fused  bool
	}
	neg0 := math.Copysign(0, -1)
	cases := []tc{
		{build: func(b *B, x, y Val) Val { return b.FAdd(x, y) }, x: 1, y: 2, want: 3},
		{build: func(b *B, x, y Val) Val { return b.FSub(x, y) }, x: 1, y: 2, want: -1},
		{build: func(b *B, x, y Val) Val { return b.FMul(x, y) }, x: 3, y: 2, want: 6},
		{build: func(b *B, x, y Val) Val { return b.FDiv(x, y) }, x: 3, y: 2, want: 1.5},
		{build: func(b *B, x, y Val) Val { return b.FSqrt(x) }, x: 9, want: 3},
		{build: func(b *B, x, y Val) Val { return b.FMod(x, y) }, x: -7.5, y: 2, want: -1.5},
		{build: func(b *B, x, y Val) Val { return b.FAbs(x) }, x: neg0, want: 0},
		{build: func(b *B, x, y Val) Val { return b.FNeg(x) }, x: 0, bits: 1 << 63},
		{build: func(b *B, x, y Val) Val { return b.FMA(x, y, b.K(-1)) }, x: 1 + 0x1p-30, y: 1 - 0x1p-30, want: -0x1p-60},
		{build: func(b *B, x, y Val) Val { return b.MAdd(x, y, b.K(-1)) }, x: 1 + 0x1p-30, y: 1 - 0x1p-30, want: 0},
		{build: func(b *B, x, y Val) Val { return b.MAdd(x, y, b.K(-1)) }, x: 1 + 0x1p-30, y: 1 - 0x1p-30, want: -0x1p-60, fused: true},
		{build: func(b *B, x, y Val) Val { return b.RoundEven(x) }, x: 2.5, want: 2},
		{build: func(b *B, x, y Val) Val { return b.Floor(x) }, x: -2.5, want: -3},
		{build: func(b *B, x, y Val) Val { return b.Ceil(x) }, x: -2.5, want: -2},
		{build: func(b *B, x, y Val) Val { return b.Trunc(x) }, x: -2.5, want: -2},
		{build: func(b *B, x, y Val) Val { return b.IToF(b.FToI(x)) }, x: -7.9, want: -7},
		{build: func(b *B, x, y Val) Val {
			i := b.Sub(b.Add(b.Const(10), b.Mul(b.Const(3), b.Const(4))), b.Const(2))
			return b.IToF(b.Xor(b.Or(b.And(i, b.Const(0xff)), b.Const(1)), b.Const(1)))
		}, want: 20},
		{build: func(b *B, x, y Val) Val {
			return b.IToF(b.Add(b.Shr(b.Const(1<<63), 62), b.Sar(b.Shl(b.Const(1), 63), 62)))
		}, want: 0},
		{build: func(b *B, x, y Val) Val {
			return b.FAdd(b.LoadF(evalTab, b.Const(0)), b.IToF(b.LoadI(evalTab, b.Const(1))))
		}, want: 9.5},
		{build: func(b *B, x, y Val) Val { b.Exit(); return x }, exited: true},
	}
	for i, c := range cases {
		b := New("t", 2, c.fused)
		x, y := b.Arg(0), b.Arg(1)
		b.Return(c.build(b, x, y))
		f, err := b.Func()
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		got, exited := Eval(f, c.x, c.y)
		if exited != c.exited {
			t.Errorf("case %d: exited %v", i, exited)
			continue
		}
		if c.exited {
			continue
		}
		if c.bits != 0 {
			if math.Float64bits(got) != c.bits {
				t.Errorf("case %d: %x, want %x", i, math.Float64bits(got), c.bits)
			}
		} else if got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

func TestEvalComparisons(t *testing.T) {
	nan := math.NaN()
	for c := FEq; c <= UGe; c++ {
		for _, p := range [][2]float64{{1, 2}, {2, 1}, {1, 1}, {nan, 1}, {-1, 1}} {
			b := New("cmp", 2, false)
			x, y := b.Arg(0), b.Arg(1)
			v := b.Var(F)
			b.Set(v, b.K(0))
			if c.Float() {
				b.If(c, x, y, func() { b.Set(v, b.K(1)) }, func() { b.Set(v, b.K(2)) })
			} else {
				b.If(c, b.FToI(x), b.FToI(y), func() { b.Set(v, b.K(1)) }, func() { b.Set(v, b.K(2)) })
			}
			b.Return(v)
			f, err := b.Func()
			if err != nil {
				t.Fatal(err)
			}
			got, _ := Eval(f, p[0], p[1])
			var want bool
			a, bb := p[0], p[1]
			ia, ib := int64(a), int64(bb)
			switch c {
			case FEq:
				want = a == bb
			case FNe:
				want = a != bb
			case FLt:
				want = a < bb
			case FLe:
				want = a <= bb
			case FGt:
				want = a > bb
			case FGe:
				want = a >= bb
			case IEq:
				want = ia == ib
			case INe:
				want = ia != ib
			case ILt:
				want = ia < ib
			case ILe:
				want = ia <= ib
			case IGt:
				want = ia > ib
			case IGe:
				want = ia >= ib
			case ULt:
				want = uint64(ia) < uint64(ib)
			case ULe:
				want = uint64(ia) <= uint64(ib)
			case UGt:
				want = uint64(ia) > uint64(ib)
			default:
				want = uint64(ia) >= uint64(ib)
			}
			if (got == 1) != want {
				t.Errorf("%s(%v, %v) = %v", cmpNames[c], a, bb, got)
			}
		}
	}
}

func TestString(t *testing.T) {
	b := New("show", 1, true)
	x := b.Arg(0)
	b.If(FLt, x, b.K(0), func() { b.Exit() }, func() { b.Return(b.FSqrt(x)) })
	f, err := b.Func()
	if err != nil {
		t.Fatal(err)
	}
	if s := f.String(); !strings.Contains(s, "fsqrt") || !strings.Contains(s, "exit") || !strings.Contains(s, "} else {") {
		t.Errorf("String:\n%s", s)
	}
}
