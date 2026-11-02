// Package fir is the form the JIT compiles a math kernel from: a function of
// float64 arguments to one float64, written once as a tree of steps and
// lowered to machine code by each backend (internal/jit).
//
// Every value is a 64-bit word, and each op says whether it reads its operands
// as IEEE doubles or as integers: a double lives in an integer's slot as its
// bits, the way V8 and warpjs keep a number, so asuint64 and asdouble cost
// nothing. Control flow is structured (If, with an optional Else), which every
// backend can lower, wasm included.
//
// A kernel leaves to Go what it does not compute. Exit hands the row back, and
// Go answers it with the function the kernel was transcribed from
// (internal/libm), which is how a kernel stays small: the special cases and
// the rare accurate paths are Go's.
package fir

import (
	"fmt"
	"math"
	"unsafe"
)

// Val is a value of a kernel: what one step defines, or a variable Set
// assigns. 0 is no value.
type Val int32

// Class is where a value lives: a floating-point register or an integer one.
// A use of the other class moves it across, which the backends do for
// asuint64/asdouble.
type Class uint8

const (
	F Class = iota
	I
)

// Op is a step's operation.
type Op uint8

const (
	opNone Op = iota
	// OpArg: the kernel's argument Imm.
	OpArg
	// OpConst: the bits Imm. A constant is materialized at each use, so it
	// holds no register.
	OpConst
	// OpCopy: A, into a variable (Set).
	OpCopy

	// Doubles, each rounded once to nearest-even.
	OpFAdd // A + B
	OpFSub // A - B
	OpFMul // A * B
	OpFDiv // A / B
	OpFSqrt
	OpFAbs
	OpFNeg
	// OpFMA: A*B + C rounded once: C's fma().
	OpFMA
	// OpMAdd: A*B + C as GCC compiled glibc's multiply-add: one rounding in
	// a kernel built Fused, two otherwise (libm.madd).
	OpMAdd
	OpRoundEven
	OpFloor
	OpCeil
	OpTrunc
	// OpFMod: C's fmod(A, B), exact.
	OpFMod
	// OpFToI: int64(A), truncated. A must be finite and inside int64's range.
	OpFToI
	// OpIToF: float64(int64(A)), rounded to nearest-even.
	OpIToF

	// 64-bit integers, wrapping.
	OpAdd
	OpSub
	OpMul
	OpAnd
	OpOr
	OpXor
	OpShl // A << Imm
	OpShr // A >> Imm, logical
	OpSar // A >> Imm, arithmetic

	// OpLoadF / OpLoadI: the word at Tab + 8*A, into a register of that class.
	// A is an index the kernel proved in range.
	OpLoadF
	OpLoadI

	// Control.
	OpIf     // when A Cmp B, Then, else Else
	OpExit   // leave the row to Go
	OpReturn // the kernel's answer is A
)

// Cmp is an If's comparison.
type Cmp uint8

const (
	// Doubles: false when either is NaN, except FNe.
	FEq Cmp = iota
	FNe
	FLt
	FLe
	FGt
	FGe
	// Signed integers.
	IEq
	INe
	ILt
	ILe
	IGt
	IGe
	// Unsigned integers.
	ULt
	ULe
	UGt
	UGe
)

// Float reports whether c compares doubles.
func (c Cmp) Float() bool { return c <= FGe }

// Step is one step of a kernel.
type Step struct {
	Op      Op
	Dst     Val
	A, B, C Val
	Imm     uint64
	// Tab is OpLoadF's and OpLoadI's table: the address of its first word,
	// in memory that never moves (a package-level array).
	Tab unsafe.Pointer
	// TabLen is the table's length in words, which Eval checks the index
	// against.
	TabLen int
	Cmp    Cmp
	Then   []Step
	Else   []Step
}

// Func is a finished kernel.
type Func struct {
	Name  string
	NArgs int
	// Fused is how OpMAdd rounds: once, as glibc's FMA build does, or twice.
	// A kernel is built for one of the two (libm's choice on this CPU),
	// since a function like log takes a different path in each.
	Fused bool
	Body  []Step
	// Class is each value's class, by Val; Class[0] is unused.
	Class []Class
	// Var marks the values Set assigns, which may be defined more than once.
	Var []bool
}

// NVals is one past the largest Val.
func (f *Func) NVals() int { return len(f.Class) }

// B builds a kernel.
type B struct {
	f     *Func
	cur   *[]Step
	scope []map[Val]bool // values defined in each open block, innermost last
	err   error
}

// New starts a kernel of nargs double arguments, whose OpMAdd steps round
// once when fused.
func New(name string, nargs int, fused bool) *B {
	b := &B{f: &Func{Name: name, NArgs: nargs, Fused: fused, Class: []Class{F}, Var: []bool{false}}}
	b.cur = &b.f.Body
	b.scope = []map[Val]bool{{}}
	return b
}

func (b *B) fail(format string, args ...any) {
	if b.err == nil {
		b.err = fmt.Errorf("fir: %s: "+format, append([]any{b.f.Name}, args...)...)
	}
}

func (b *B) newVal(c Class, isVar bool) Val {
	b.f.Class = append(b.f.Class, c)
	b.f.Var = append(b.f.Var, isVar)
	return Val(len(b.f.Class) - 1)
}

// use checks that v is visible here: defined in an enclosing block, or a
// variable.
func (b *B) use(v Val) {
	if v <= 0 || int(v) >= len(b.f.Class) {
		b.fail("no value %d", v)
		return
	}
	if b.f.Var[v] {
		return
	}
	for _, s := range b.scope {
		if s[v] {
			return
		}
	}
	b.fail("value %d used outside the block that defines it", v)
}

func (b *B) def(st Step, c Class) Val {
	for _, v := range []Val{st.A, st.B, st.C} {
		if v != 0 {
			b.use(v)
		}
	}
	st.Dst = b.newVal(c, false)
	b.scope[len(b.scope)-1][st.Dst] = true
	*b.cur = append(*b.cur, st)
	return st.Dst
}

// Arg is argument i.
func (b *B) Arg(i int) Val {
	if i < 0 || i >= b.f.NArgs {
		b.fail("argument %d of %d", i, b.f.NArgs)
	}
	return b.def(Step{Op: OpArg, Imm: uint64(i)}, F)
}

// Const is the 64 bits u.
func (b *B) Const(u uint64) Val { return b.def(Step{Op: OpConst, Imm: u}, I) }

// K is the double x.
func (b *B) K(x float64) Val { return b.def(Step{Op: OpConst, Imm: math.Float64bits(x)}, F) }

func (b *B) f2(op Op, x, y Val) Val     { return b.def(Step{Op: op, A: x, B: y}, F) }
func (b *B) f1(op Op, x Val) Val        { return b.def(Step{Op: op, A: x}, F) }
func (b *B) i2(op Op, x, y Val) Val     { return b.def(Step{Op: op, A: x, B: y}, I) }
func (b *B) sh(op Op, x Val, n int) Val { return b.def(Step{Op: op, A: x, Imm: uint64(n)}, I) }

func (b *B) FAdd(x, y Val) Val { return b.f2(OpFAdd, x, y) }
func (b *B) FSub(x, y Val) Val { return b.f2(OpFSub, x, y) }
func (b *B) FMul(x, y Val) Val { return b.f2(OpFMul, x, y) }
func (b *B) FDiv(x, y Val) Val { return b.f2(OpFDiv, x, y) }
func (b *B) FMod(x, y Val) Val { return b.f2(OpFMod, x, y) }
func (b *B) FSqrt(x Val) Val   { return b.f1(OpFSqrt, x) }
func (b *B) FAbs(x Val) Val    { return b.f1(OpFAbs, x) }
func (b *B) FNeg(x Val) Val    { return b.f1(OpFNeg, x) }

// FMA is x*y+z rounded once.
func (b *B) FMA(x, y, z Val) Val { return b.def(Step{Op: OpFMA, A: x, B: y, C: z}, F) }

// MAdd is x*y+z as glibc's GCC build computes it: libm's madd.
func (b *B) MAdd(x, y, z Val) Val { return b.def(Step{Op: OpMAdd, A: x, B: y, C: z}, F) }

func (b *B) RoundEven(x Val) Val { return b.f1(OpRoundEven, x) }
func (b *B) Floor(x Val) Val     { return b.f1(OpFloor, x) }
func (b *B) Ceil(x Val) Val      { return b.f1(OpCeil, x) }
func (b *B) Trunc(x Val) Val     { return b.f1(OpTrunc, x) }
func (b *B) FToI(x Val) Val      { return b.def(Step{Op: OpFToI, A: x}, I) }
func (b *B) IToF(x Val) Val      { return b.def(Step{Op: OpIToF, A: x}, F) }

func (b *B) Add(x, y Val) Val         { return b.i2(OpAdd, x, y) }
func (b *B) Sub(x, y Val) Val         { return b.i2(OpSub, x, y) }
func (b *B) Mul(x, y Val) Val         { return b.i2(OpMul, x, y) }
func (b *B) And(x, y Val) Val         { return b.i2(OpAnd, x, y) }
func (b *B) Or(x, y Val) Val          { return b.i2(OpOr, x, y) }
func (b *B) Xor(x, y Val) Val         { return b.i2(OpXor, x, y) }
func (b *B) Shl(x Val, n int) Val     { return b.sh(OpShl, x, n) }
func (b *B) Shr(x Val, n int) Val     { return b.sh(OpShr, x, n) }
func (b *B) Sar(x Val, n int) Val     { return b.sh(OpSar, x, n) }
func (b *B) AndK(x Val, k uint64) Val { return b.And(x, b.Const(k)) }
func (b *B) AddK(x Val, k uint64) Val { return b.Add(x, b.Const(k)) }

// LoadF is the double tab[i]; LoadI the word.
func (b *B) LoadF(tab []uint64, i Val) Val { return b.load(OpLoadF, F, tab, i) }
func (b *B) LoadI(tab []uint64, i Val) Val { return b.load(OpLoadI, I, tab, i) }

func (b *B) load(op Op, c Class, tab []uint64, i Val) Val {
	if len(tab) == 0 {
		b.fail("empty table")
		return 0
	}
	return b.def(Step{Op: op, A: i, Tab: unsafe.Pointer(&tab[0]), TabLen: len(tab)}, c)
}

// Var is a variable of class c, which Set assigns and steps read like any
// value.
func (b *B) Var(c Class) Val { return b.newVal(c, true) }

// Set assigns x to the variable v.
func (b *B) Set(v, x Val) {
	if v <= 0 || int(v) >= len(b.f.Var) || !b.f.Var[v] {
		b.fail("Set of %d, which is not a variable", v)
		return
	}
	b.use(x)
	*b.cur = append(*b.cur, Step{Op: OpCopy, Dst: v, A: x})
}

// If runs then when x c y, and els (which may be nil) when not.
func (b *B) If(c Cmp, x, y Val, then, els func()) {
	b.use(x)
	b.use(y)
	st := Step{Op: OpIf, A: x, B: y, Cmp: c}
	st.Then = b.block(then)
	if els != nil {
		st.Else = b.block(els)
	}
	*b.cur = append(*b.cur, st)
}

func (b *B) block(fn func()) []Step {
	var out []Step
	saved := b.cur
	b.cur = &out
	b.scope = append(b.scope, map[Val]bool{})
	fn()
	b.scope = b.scope[:len(b.scope)-1]
	b.cur = saved
	return out
}

// Exit leaves the row to Go.
func (b *B) Exit() { *b.cur = append(*b.cur, Step{Op: OpExit}) }

// ExitIf leaves the row to Go when x c y.
func (b *B) ExitIf(c Cmp, x, y Val) { b.If(c, x, y, b.Exit, nil) }

// Return makes x the kernel's answer and ends it.
func (b *B) Return(x Val) {
	b.use(x)
	*b.cur = append(*b.cur, Step{Op: OpReturn, A: x})
}

// Fused is the kernel's OpMAdd rounding (New).
func (b *B) Fused() bool { return b.f.Fused }

// Func finishes the kernel. Every path must end in a Return or an Exit.
func (b *B) Func() (*Func, error) {
	if b.err != nil {
		return nil, b.err
	}
	if !ends(b.f.Body) {
		return nil, fmt.Errorf("fir: %s: a path falls off the end", b.f.Name)
	}
	return b.f, nil
}

// ends reports whether every path through steps reaches a Return or an Exit.
func ends(steps []Step) bool {
	for _, st := range steps {
		switch st.Op {
		case OpReturn, OpExit:
			return true
		case OpIf:
			if st.Else != nil && ends(st.Then) && ends(st.Else) {
				return true
			}
		}
	}
	return false
}
