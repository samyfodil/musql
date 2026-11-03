package jit

import (
	"fmt"
	"math"

	"github.com/samyfodil/musql/internal/fir"
)

// A math kernel (internal/fir) in wasm. Wasm is a stack machine with typed
// locals, so there is nothing to allocate: each value is a local of its
// class, f64 or i64, and a constant is pushed at each use. Every op fir has
// is one wasm instruction, but for fma, which wasm lacks: a kernel with an
// FMA, or a fused multiply-add, is refused, and fmod answers only |x| < |y|.

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
	depth uint32 // ifs open inside the kernel's two blocks
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
	if f.NArgs > 2 || firHasOp(f.Body, fir.OpFMA) || f.Fused && firHasOp(f.Body, fir.OpMAdd) {
		return fmt.Errorf("jit: kernel %s needs FMA", f.Name)
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
	case fir.OpMAdd:
		// Unfused: firWasmOK refused a fused one.
		e.get(st.A, fir.F)
		e.get(st.B, fir.F)
		w.op(opF64Mul)
		e.get(st.C, fir.F)
		w.op(opF64Add)
		e.set(st.Dst, fir.F)
	case fir.OpFMod:
		// fmod(x, y) is x when |x| < |y|; anything else is Go's.
		e.get(st.A, fir.F)
		w.op(opF64Abs)
		e.get(st.B, fir.F)
		w.op(opF64Abs)
		w.op(opF64Lt)
		w.op(opI32Eqz)
		w.BrIf(e.depth)
		e.get(st.A, fir.F)
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
