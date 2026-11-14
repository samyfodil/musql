package engine

import (
	"math"

	"github.com/samyfodil/musql/internal/fir"
	"github.com/samyfodil/musql/internal/jit"
	"github.com/samyfodil/musql/internal/libm"
)

// The REAL arms of the VM JIT (jit.VReal to jit.VMath). The integer arms
// speculate an INTEGER and exit on anything else, which made every REAL
// operation a trip back to the VDBE. Where an operand is a REAL by what the
// program says -- a REAL literal, a column of REAL affinity, REAL arithmetic,
// a math function -- the instruction lowers to its REAL arm instead, which
// guards its operands the same way: the guess only picks the arm, and a wrong
// one exits.

// vmRealLowering is, per pc, the REAL arm the instruction there lowers to;
// ok false where it keeps lowerVM's.
//
// storesReal, when not nil, says whether a table's segments store a column
// as REALs (a PhysFloat64 or PhysNumeric block): an INTEGER-declared column
// of prices is guessed REAL too.
//
// storesText says the same of TEXT: arithmetic on a column stored as TEXT
// (numbers a VARCHAR column holds as text) converts it on every row, which
// only the VDBE does, so the instruction -- and arithmetic on its result --
// stays the VDBE's rather than enter native code that would exit at it.
func vmRealLowering(prog *Program, storesReal, storesText func(root uint32, col int) bool) (out []jit.VInsn, ok []bool) {
	if !jit.VMReal {
		return nil, nil
	}
	n := len(prog.Insns)
	out, ok = make([]jit.VInsn, n), make([]bool, n)
	tables := map[int]*resolvedTable{}
	real := map[int]bool{}
	text := map[int]bool{}
	in := func(r int) bool { return r >= 0 && r < prog.NReg }
	for pc := range prog.Insns {
		ins := &prog.Insns[pc]
		set := func(r int, isReal bool) {
			if isReal {
				real[r] = true
			} else {
				delete(real, r)
			}
			delete(text, r)
		}
		switch ins.Op {
		case OpOpenRead, OpOpenWrite:
			if t, isTbl := ins.P4.(*resolvedTable); isTbl {
				tables[ins.P1] = t
			}
		case OpReal:
			if f, isF := ins.P4.(float64); isF && in(ins.P2) {
				out[pc], ok[pc] = jit.VInsn{Op: jit.VReal, A: ins.P2, Imm: int64(math.Float64bits(f))}, true
			}
			set(ins.P2, true)
		case OpColumn:
			t := tables[ins.P1]
			set(ins.P3, t != nil && ins.P2 >= 0 && ins.P2 < len(t.cols) &&
				(t.cols[ins.P2].Aff == affReal || storesReal != nil && storesReal(t.root, ins.P2)))
			if t != nil && ins.P2 >= 0 && ins.P2 < len(t.cols) && !real[ins.P3] && storesText != nil && storesText(t.root, ins.P2) {
				text[ins.P3] = true
			}
		case OpSCopy, OpCopy:
			wasText := text[ins.P1]
			set(ins.P2, real[ins.P1])
			if wasText {
				text[ins.P2] = true
			}
		case OpInteger, OpInt64, OpNull, OpString8, OpBlob, OpVariable, OpNot, OpRowid:
			set(ins.P2, false)
		case OpAdd, OpSubtract, OpMultiply, OpDivide:
			isReal := real[ins.P1] || real[ins.P2]
			if text[ins.P1] || text[ins.P2] {
				// Its result is an INTEGER or a REAL by the text's spelling, a
				// guess neither arm makes: what it feeds stays the VDBE's too.
				out[pc], ok[pc] = jit.VInsn{Op: jit.VExit}, true
				set(ins.P3, false)
				text[ins.P3] = true
				break
			}
			if isReal && in(ins.P1) && in(ins.P2) && in(ins.P3) {
				op := map[OpCode]jit.VOp{OpAdd: jit.VFAdd, OpSubtract: jit.VFSub, OpMultiply: jit.VFMul, OpDivide: jit.VFDiv}[ins.Op]
				out[pc], ok[pc] = jit.VInsn{Op: op, A: ins.P1, B: ins.P2, C: ins.P3}, true
			}
			set(ins.P3, isReal)
		case OpRemainder, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
			if text[ins.P1] || text[ins.P2] {
				out[pc], ok[pc] = jit.VInsn{Op: jit.VExit}, true
			}
			set(ins.P3, false)
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			if (real[ins.P1] || real[ins.P3]) && affinity(ins.P5&p5AffMask) != affText && in(ins.P1) && in(ins.P3) {
				cond := vmCond[ins.Op]
				if ins.P5&p5StoreP2 == 0 {
					out[pc], ok[pc] = jit.VInsn{Op: jit.VFCmpJump, A: ins.P1, B: ins.P3, T: ins.P2, Cond: cond}, true
				} else if in(ins.P2) {
					out[pc], ok[pc] = jit.VInsn{Op: jit.VFCmpSet, A: ins.P1, B: ins.P3, C: ins.P2, Cond: cond}, true
				}
			}
			if ins.P5&p5StoreP2 != 0 {
				set(ins.P2, false)
			}
		case OpFunction:
			v, isReal := vmRealCall(ins, real, in)
			if v.Op != jit.VExit {
				out[pc], ok[pc] = v, true
			}
			set(ins.P3, isReal)
		}
	}
	return out, ok
}

// vmRealCall lowers a call to a math function or abs(): its VMath, or VExit
// where none applies; isReal is whether the call answers a REAL.
func vmRealCall(ins *Instruction, real map[int]bool, in func(int) bool) (jit.VInsn, bool) {
	exit := jit.VInsn{Op: jit.VExit}
	name, isName := ins.P4.(string)
	if !isName || !in(ins.P3) || (ins.P2 > 0 && (!in(ins.P1) || !in(ins.P1+ins.P2-1))) {
		return exit, false
	}
	argReal := ins.P2 >= 1 && real[ins.P1]
	switch name {
	case "pi":
		if ins.P2 == 0 {
			return jit.VInsn{Op: jit.VReal, A: ins.P3, Imm: int64(math.Float64bits(math.Pi))}, true
		}
		return exit, true
	case "abs":
		// abs() keeps its argument's type; its REAL arm is the kernel below.
		if ins.P2 != 1 || !argReal {
			return exit, false
		}
		return jit.VInsn{Op: jit.VMath, A: ins.P1, B: 1, C: ins.P3, Kernel: vmAbsKernel(), Imm: 1}, true
	case "ceil", "ceiling", "floor", "trunc":
		// Of an INTEGER, the INTEGER: only a REAL argument is native.
		if ins.P2 != 1 || !argReal {
			return exit, false
		}
		if k := segMathKernel(name, 1); k != nil {
			return jit.VInsn{Op: jit.VMath, A: ins.P1, B: 1, C: ins.P3, Kernel: k, Imm: 1}, true
		}
		return exit, true
	}
	if _, isMath := mathFuncArgs[name]; !isMath {
		return exit, false
	}
	if ins.P2 >= 1 && ins.P2 <= 2 {
		if k := segMathKernel(name, ins.P2); k != nil {
			return jit.VInsn{Op: jit.VMath, A: ins.P1, B: ins.P2, C: ins.P3, Kernel: k}, true
		}
	}
	return exit, true
}

// vmAbsKernel is abs() of a REAL as absFunc computes it: negated when below
// zero, so -0.0 stays -0.0.
var vmAbsKernel = func() func() *fir.Func {
	var f *fir.Func
	return func() *fir.Func {
		if f == nil {
			k := libm.NewKernel("sql:abs", 1)
			x := k.Arg(0)
			k.If(fir.FLt, x, k.K(0), func() { k.Return(k.FNeg(x)) }, nil)
			k.Return(x)
			f, _ = k.Func()
		}
		return f
	}
}()
