package jit

import (
	"fmt"
	"math"

	"github.com/samyfodil/musql/internal/fir"
)

// A math kernel (internal/fir) in wasm. Wasm is a stack machine with typed
// locals, so there is nothing to allocate: each value is a local of its
// class, f64 or i64, and a constant is pushed at each use. Nearly every op
// fir has is one wasm instruction. Wasm has no fma, so fma is computed
// exactly from doubles (firWasm.fma), leaving to Go the inputs where that
// could overflow or underflow; fmod answers only |x| < |y|.

const (
	wF64 byte = 0x7C

	opF64Load           byte = 0x2B
	opF64Store          byte = 0x39
	opF64Const          byte = 0x44
	opF64Eq             byte = 0x61
	opF64Ne             byte = 0x62
	opF64Lt             byte = 0x63
	opF64Gt             byte = 0x64
	opF64Le             byte = 0x65
	opF64Ge             byte = 0x66
	opI64LtU            byte = 0x54
	opI64GtS            byte = 0x55
	opI64GeS            byte = 0x59
	opI64ShrU           byte = 0x88
	opF64Abs            byte = 0x99
	opF64Neg            byte = 0x9A
	opF64Ceil           byte = 0x9B
	opF64Floor          byte = 0x9C
	opF64Trunc          byte = 0x9D
	opF64Nearest        byte = 0x9E
	opF64Sqrt           byte = 0x9F
	opF64Add            byte = 0xA0
	opF64Sub            byte = 0xA1
	opF64Mul            byte = 0xA2
	opF64Div            byte = 0xA3
	opF64Copysign       byte = 0xA6
	opF64ConvertI64S    byte = 0xB9
	opI64ReinterpretF64 byte = 0xBD
	opF64ReinterpretI64 byte = 0xBF
	opMisc              byte = 0xFC
	miscI64TruncSatF64S byte = 0x06
)

// F64 pushes the double x.
func (w *Wasm) F64(x float64) {
	w.op(opF64Const)
	b := math.Float64bits(x)
	for range 8 {
		w.body = append(w.body, byte(b))
		b >>= 8
	}
}

func (w *Wasm) LoadF64(off uint32)  { w.op(opF64Load); w.memarg(3, off) }
func (w *Wasm) StoreF64(off uint32) { w.op(opF64Store); w.memarg(3, off) }

// firWasm emits one kernel.
type firWasm struct {
	w     *Wasm
	f     *fir.Func
	loc   []uint32
	konst map[fir.Val]uint64
	args  []uint32
	out   uint32
	depth uint32   // ifs open inside the kernel's two blocks
	fl    []uint32 // fma's f64 scratch, declared at first use
}

// emitFirWasm emits f inline: its arguments in the f64 locals args, its
// answer into the f64 local out. Its code is two blocks: an Exit runs
// onExit, and an answer goes on past both. onExit must leave the function or
// branch out of these blocks.
func emitFirWasm(w *Wasm, f *fir.Func, args []uint32, out uint32, onExit func()) error {
	if err := firWasmOK(f); err != nil {
		return err
	}
	if len(args) != f.NArgs {
		return fmt.Errorf("jit: kernel %s takes %d arguments, given %d", f.Name, f.NArgs, len(args))
	}
	e := &firWasm{w: w, f: f, loc: make([]uint32, f.NVals()), konst: map[fir.Val]uint64{}, args: args, out: out}
	e.consts(f.Body)
	for v := 1; v < f.NVals(); v++ {
		if _, ok := e.konst[fir.Val(v)]; ok {
			continue
		}
		t := wI64
		if f.Class[v] == fir.F {
			t = wF64
		}
		e.loc[v] = w.Local(t)
	}
	w.op(opBlock, opVoid) // the answer
	w.op(opBlock, opVoid) // an exit
	if err := e.steps(f.Body); err != nil {
		return err
	}
	w.op(opEnd)
	onExit()
	w.op(opEnd)
	return nil
}

// firWasmOK refuses what wasm has no instruction for.
func firWasmOK(f *fir.Func) error {
	if f.NArgs > 2 {
		return fmt.Errorf("jit: kernel %s takes %d arguments", f.Name, f.NArgs)
	}
	return nil
}

// KernelOK reports whether f compiles into a program here.
func KernelOK(f *fir.Func) bool { return firWasmOK(f) == nil }

// consts records the values that are constants, which hold no local.
func (e *firWasm) consts(steps []fir.Step) {
	for i := range steps {
		st := &steps[i]
		if st.Op == fir.OpConst && !e.f.Var[st.Dst] {
			e.konst[st.Dst] = st.Imm
		}
		e.consts(st.Then)
		e.consts(st.Else)
	}
}

// get pushes v as class c, moving it across classes when it lives in the
// other.
func (e *firWasm) get(v fir.Val, c fir.Class) {
	w := e.w
	if k, ok := e.konst[v]; ok {
		if c == fir.F {
			w.F64(math.Float64frombits(k))
		} else {
			w.I64(int64(k))
		}
		return
	}
	w.Get(e.loc[v])
	if have := e.f.Class[v]; have != c {
		if c == fir.F {
			w.op(opF64ReinterpretI64)
		} else {
			w.op(opI64ReinterpretF64)
		}
	}
}

// set stores the value of class c on the stack into v.
func (e *firWasm) set(v fir.Val, c fir.Class) {
	if have := e.f.Class[v]; have != c {
		if have == fir.F {
			e.w.op(opF64ReinterpretI64)
		} else {
			e.w.op(opI64ReinterpretF64)
		}
	}
	e.w.Set(e.loc[v])
}

func (e *firWasm) steps(steps []fir.Step) error {
	for i := range steps {
		if err := e.step(&steps[i]); err != nil {
			return err
		}
	}
	return nil
}

var firWasmF2 = map[fir.Op]byte{fir.OpFAdd: opF64Add, fir.OpFSub: opF64Sub, fir.OpFMul: opF64Mul, fir.OpFDiv: opF64Div}

var firWasmF1 = map[fir.Op]byte{
	fir.OpFSqrt: opF64Sqrt, fir.OpFAbs: opF64Abs, fir.OpFNeg: opF64Neg, fir.OpRoundEven: opF64Nearest,
	fir.OpFloor: opF64Floor, fir.OpCeil: opF64Ceil, fir.OpTrunc: opF64Trunc,
}

var firWasmI2 = map[fir.Op]byte{
	fir.OpAdd: opI64Add, fir.OpSub: opI64Sub, fir.OpMul: opI64Mul, fir.OpAnd: opI64And, fir.OpOr: opI64Or,
	fir.OpXor: opI64Xor,
}

var firWasmShift = map[fir.Op]byte{fir.OpShl: opI64Shl, fir.OpShr: opI64ShrU, fir.OpSar: opI64ShrS}

var firWasmCmp = map[fir.Cmp]byte{
	fir.FEq: opF64Eq, fir.FNe: opF64Ne, fir.FLt: opF64Lt, fir.FLe: opF64Le, fir.FGt: opF64Gt, fir.FGe: opF64Ge,
	fir.IEq: opI64Eq, fir.INe: opI64Ne, fir.ILt: opI64LtS, fir.ILe: opI64LeS, fir.IGt: opI64GtS, fir.IGe: opI64GeS,
	fir.ULt: opI64LtU, fir.ULe: opI64LeU, fir.UGt: opI64GtU, fir.UGe: opI64GeU,
}

func (e *firWasm) step(st *fir.Step) error {
	w := e.w
	switch st.Op {
	case fir.OpConst:
		if _, ok := e.konst[st.Dst]; !ok {
			w.I64(int64(st.Imm))
			e.set(st.Dst, fir.I)
		}
	case fir.OpArg:
		w.Get(e.args[st.Imm])
		e.set(st.Dst, fir.F)
	case fir.OpCopy:
		c := e.f.Class[st.Dst]
		e.get(st.A, c)
		e.set(st.Dst, c)
	case fir.OpFAdd, fir.OpFSub, fir.OpFMul, fir.OpFDiv:
		e.get(st.A, fir.F)
		e.get(st.B, fir.F)
		w.op(firWasmF2[st.Op])
		e.set(st.Dst, fir.F)
	case fir.OpFSqrt, fir.OpFAbs, fir.OpFNeg, fir.OpRoundEven, fir.OpFloor, fir.OpCeil, fir.OpTrunc:
		e.get(st.A, fir.F)
		w.op(firWasmF1[st.Op])
		e.set(st.Dst, fir.F)
	case fir.OpFMA, fir.OpMAdd:
		if st.Op == fir.OpFMA || e.f.Fused {
			e.fma(st.A, st.B, st.C)
		} else {
			e.get(st.A, fir.F)
			e.get(st.B, fir.F)
			w.op(opF64Mul)
			e.get(st.C, fir.F)
			w.op(opF64Add)
		}
		e.set(st.Dst, fir.F)
	case fir.OpFMod:
		e.fmod(st)
		e.set(st.Dst, fir.F)
	case fir.OpFToI:
		e.get(st.A, fir.F)
		w.op(opMisc, miscI64TruncSatF64S)
		e.set(st.Dst, fir.I)
	case fir.OpIToF:
		e.get(st.A, fir.I)
		w.op(opF64ConvertI64S)
		e.set(st.Dst, fir.F)
	case fir.OpAdd, fir.OpSub, fir.OpMul, fir.OpAnd, fir.OpOr, fir.OpXor:
		e.get(st.A, fir.I)
		e.get(st.B, fir.I)
		w.op(firWasmI2[st.Op])
		e.set(st.Dst, fir.I)
	case fir.OpShl, fir.OpShr, fir.OpSar:
		e.get(st.A, fir.I)
		w.I64(int64(st.Imm))
		w.op(firWasmShift[st.Op])
		e.set(st.Dst, fir.I)
	case fir.OpLoadF, fir.OpLoadI:
		// The table is Go memory that never moves, and a Go pointer is its
		// address in the linear memory.
		w.I32(int32(uintptr(st.Tab)))
		e.get(st.A, fir.I)
		w.op(opI32WrapI64)
		w.I32(3)
		w.op(opI32Shl)
		w.op(opI32Add)
		if st.Op == fir.OpLoadF {
			w.LoadF64(0)
			e.set(st.Dst, fir.F)
		} else {
			w.LoadI64(0)
			e.set(st.Dst, fir.I)
		}
	case fir.OpIf:
		c := fir.I
		if st.Cmp.Float() {
			c = fir.F
		}
		e.get(st.A, c)
		e.get(st.B, c)
		w.op(firWasmCmp[st.Cmp])
		w.op(opIf, opVoid)
		e.depth++
		if err := e.steps(st.Then); err != nil {
			return err
		}
		if st.Else != nil {
			w.op(opElse)
			if err := e.steps(st.Else); err != nil {
				return err
			}
		}
		w.op(opEnd)
		e.depth--
	case fir.OpExit:
		w.Br(e.depth)
	case fir.OpReturn:
		e.get(st.A, fir.F)
		w.Set(e.out)
		w.Br(e.depth + 1)
	default:
		return fmt.Errorf("jit: kernel %s: op %d", e.f.Name, st.Op)
	}
	return nil
}

// fmod pushes C's fmod(x, y), as fir_arm64.go's: x when |x| < |y|, else
// with q = trunc(|x/y|) below 2^53, fma(-q, |y|, |x|) -- exact, being the
// remainder or (q rounded up) it less |y| -- made positive and given x's
// sign. A larger quotient, which includes an infinity, a zero divisor and a
// NaN, exits.
func (e *firWasm) fmod(st *fir.Step) {
	w := e.w
	e.scratch()
	ax, ay := e.fl[15], e.fl[16]
	e.get(st.A, fir.F)
	w.op(opF64Abs)
	w.Set(ax)
	e.get(st.B, fir.F)
	w.op(opF64Abs)
	w.Set(ay)
	w.Get(ax)
	w.Get(ay)
	w.op(opF64Lt)
	w.op(opIf, wF64)
	e.get(st.A, fir.F)
	w.op(opElse)
	e.depth++
	w.Get(ax)
	w.Get(ay)
	w.op(opF64Div)
	w.Set(e.fl[0])
	w.Get(e.fl[0])
	w.F64(0x1p53)
	w.op(opF64Lt)
	w.op(opI32Eqz)
	w.BrIf(e.depth)
	w.Get(e.fl[0])
	w.op(opF64Trunc)
	w.op(opF64Neg)
	w.Set(e.fl[0])
	w.Get(ay)
	w.Set(e.fl[1])
	w.Get(ax)
	w.Set(e.fl[2])
	e.fmaLocals()
	w.Set(e.fl[14])
	w.Get(e.fl[14])
	w.Get(ay)
	w.op(opF64Add)
	w.Get(e.fl[14])
	w.Get(e.fl[14])
	w.F64(0)
	w.op(opF64Lt)
	w.op(opSelect)
	e.get(st.A, fir.F)
	w.op(opF64Copysign)
	e.depth--
	w.op(opEnd)
}

// scratch declares fma's and fmod's f64 locals at first use.
func (e *firWasm) scratch() {
	if e.fl == nil {
		for range 17 {
			e.fl = append(e.fl, e.w.Local(wF64))
		}
	}
}

// fma pushes a*b + c rounded once, as math.FMA answers it. Dekker's product
// splits a*b into uh + ul exactly, a two-sum splits c + uh into th + tl
// exactly, and th + RO(tl + ul), the inner sum rounded to odd, is rounded
// once (Boldo and Melquiond, "Emulation of FMA and correctly rounded sums:
// proved algorithms using rounding to odd", 2008). That holds where no step
// overflows and the product's error is a normal double, which the bounds
// below make so; a product too small for them is scaled up by 2^600, with
// c, and the answer down, which is exact unless the answer is subnormal.
// Any other input exits.
func (e *firWasm) fma(a, b, c fir.Val) {
	e.scratch()
	for i, v := range []fir.Val{a, b, c} {
		e.get(v, fir.F)
		e.w.Set(e.fl[i])
	}
	e.fmaLocals()
}

// fmaLocals is fma over the scratch locals 0, 1 and 2.
func (e *firWasm) fmaLocals() {
	w := e.w
	x, y, z, uh, sc, res := e.fl[0], e.fl[1], e.fl[2], e.fl[7], e.fl[13], e.fl[14]
	product := func() {
		w.Get(x)
		w.Get(y)
		w.op(opF64Mul)
		w.Set(uh)
	}
	product()
	inRange := func(v uint32, lo, hi float64) {
		w.Get(v)
		w.op(opF64Abs)
		w.F64(lo)
		w.op(opF64Ge)
		w.Get(v)
		w.op(opF64Abs)
		w.F64(hi)
		w.op(opF64Le)
		w.op(opI32And)
	}
	// A zero operand makes a*b exact, and so a*b + c the answer.
	w.Get(x)
	w.F64(0)
	w.op(opF64Eq)
	w.Get(y)
	w.F64(0)
	w.op(opF64Eq)
	w.op(opI32Or)
	w.op(opIf, wF64)
	w.Get(uh)
	w.Get(z)
	w.op(opF64Add)
	w.op(opElse)
	// A product below 2^-60 of a finite c of 2^-1000 or more is well inside
	// a quarter of c's ulp, and c is the answer.
	w.Get(uh)
	w.op(opF64Abs)
	w.Get(z)
	w.op(opF64Abs)
	w.F64(0x1p-60)
	w.op(opF64Mul)
	w.op(opF64Lt)
	inRange(z, 0x1p-1000, math.MaxFloat64)
	w.op(opI32And)
	w.op(opIf, wF64)
	w.Get(z)
	w.op(opElse)
	w.F64(0x1p600)
	w.F64(1)
	w.Get(uh)
	w.op(opF64Abs)
	w.F64(0x1p-950)
	w.op(opF64Lt)
	w.op(opSelect)
	w.Set(sc)
	for _, v := range []uint32{x, z} {
		w.Get(v)
		w.Get(sc)
		w.op(opF64Mul)
		w.Set(v)
	}
	product()
	// The exact path's bounds: |a|, |b| <= 2^995, so the split cannot
	// overflow; 2^-950 <= |a*b| <= 2^1000, so the product's error is a
	// normal double and no partial product overflows (Dekker's condition is
	// an exponent sum of at least -970); and c is 0 or 2^-960 <= |c| <=
	// 2^1000. A NaN or an infinity fails a comparison.
	inRange(x, 0, 0x1p995)
	inRange(y, 0, 0x1p995)
	w.op(opI32And)
	inRange(uh, 0x1p-950, 0x1p1000)
	w.op(opI32And)
	inRange(z, 0x1p-960, 0x1p1000)
	w.Get(z)
	w.F64(0)
	w.op(opF64Eq)
	w.op(opI32Or)
	w.op(opI32And)
	w.op(opIf, wF64)
	e.fmaExact(x, y, z, uh)
	w.F64(1)
	w.Get(sc)
	w.op(opF64Div)
	w.op(opF64Mul)
	w.Set(res)
	// Scaled down into the subnormals, it was rounded twice.
	w.Get(sc)
	w.F64(1)
	w.op(opF64Ne)
	w.Get(res)
	w.op(opF64Abs)
	w.F64(0x1p-1022)
	w.op(opF64Lt)
	w.op(opI32And)
	w.Get(res)
	w.F64(0)
	w.op(opF64Ne)
	w.op(opI32And)
	w.BrIf(e.depth + 3)
	w.Get(res)
	w.op(opElse)
	w.Br(e.depth + 3)
	w.op(opEnd)
	w.op(opEnd)
	w.op(opEnd)
}

// fmaExact pushes x*y + z rounded once, uh being x*y rounded, for operands
// fma's bounds admit.
func (e *firWasm) fmaExact(x, y, z, uh uint32) {
	w := e.w
	xh, xl, yh, yl := e.fl[3], e.fl[4], e.fl[5], e.fl[6]
	ul, th, tl, s, r := e.fl[8], e.fl[9], e.fl[10], e.fl[11], e.fl[12]
	mul := func(p, q uint32) {
		w.Get(p)
		w.Get(q)
		w.op(opF64Mul)
	}
	// Veltkamp's split: v = hi + lo, each of 26 bits.
	split := func(v, hi, lo uint32) {
		w.F64(0x1p27 + 1)
		w.Get(v)
		w.op(opF64Mul)
		w.Set(s)
		w.Get(s)
		w.Get(s)
		w.Get(v)
		w.op(opF64Sub)
		w.op(opF64Sub)
		w.Set(hi)
		w.Get(v)
		w.Get(hi)
		w.op(opF64Sub)
		w.Set(lo)
	}
	split(x, xh, xl)
	split(y, yh, yl)
	// ul = ((xh*yh - uh) + xh*yl + xl*yh) + xl*yl
	mul(xh, yh)
	w.Get(uh)
	w.op(opF64Sub)
	mul(xh, yl)
	w.op(opF64Add)
	mul(xl, yh)
	w.op(opF64Add)
	mul(xl, yl)
	w.op(opF64Add)
	w.Set(ul)
	// twoSum sets hi and lo to p + q rounded and its error.
	twoSum := func(p, q, hi, lo uint32) {
		w.Get(p)
		w.Get(q)
		w.op(opF64Add)
		w.Set(hi)
		w.Get(hi)
		w.Get(p)
		w.op(opF64Sub)
		w.Set(r) // the part of q in hi
		w.Get(p)
		w.Get(hi)
		w.Get(r)
		w.op(opF64Sub)
		w.op(opF64Sub)
		w.Get(q)
		w.Get(r)
		w.op(opF64Sub)
		w.op(opF64Add)
		w.Set(lo)
	}
	twoSum(z, uh, th, tl)
	twoSum(tl, ul, s, r)
	// Rounded to odd: s, or when the sum was inexact (r != 0) and s is even,
	// s one ulp toward r.
	w.Get(s)
	w.op(opI64ReinterpretF64)
	w.I64(1)
	w.op(opI64Add)
	w.Get(s)
	w.op(opI64ReinterpretF64)
	w.Get(r)
	w.op(opI64ReinterpretF64)
	w.op(opI64Xor)
	w.I64(63)
	w.op(opI64ShrU)
	w.I64(1)
	w.op(opI64Shl)
	w.op(opI64Sub) // bits(s) + 1, or - 1 when r's sign is not s's
	w.Get(s)
	w.op(opI64ReinterpretF64)
	w.Get(r)
	w.F64(0)
	w.op(opF64Ne)
	w.Get(s)
	w.op(opI64ReinterpretF64)
	w.I64(1)
	w.op(opI64And)
	w.op(opI64Eqz)
	w.op(opI32And)
	w.op(opSelect)
	w.op(opF64ReinterpretI64)
	w.Get(th)
	w.op(opF64Add)
}
