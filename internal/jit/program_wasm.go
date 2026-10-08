//go:build wasm

package jit

import (
	"fmt"
	"math"
)

// More opcodes, for the program JIT.
const (
	opI32Eqz  byte = 0x45
	opI32Or   byte = 0x72
	opI64Eqz  byte = 0x50
	opI64Eq   byte = 0x51
	opI64Ne   byte = 0x52
	opI64LtS  byte = 0x53
	opI64Sub  byte = 0x7D
	opI64Mul  byte = 0x7E
	opI64DivS byte = 0x7F
	opI64RemS byte = 0x81
	opI64And  byte = 0x83
	opI64Or   byte = 0x84
	opI64Xor  byte = 0x85
)

// EmitProgram compiles an instruction list into a wasm row loop.
//
// Wasm has no goto, but the IR only jumps FORWARD (an OR or BETWEEN epilogue
// skips ahead; the row loop itself is the only back edge). So each IR index i
// gets a block that ends just before instruction i, nested so the earliest
// target is innermost; a jump from i to j is a br out of the blocks between
// them. A backward jump is declined, and the VDBE answers.
//
// Registers stay in the memory file, as on the native emitters: the caller
// reads the counts POpAccSum and friends leave there. The row index counts
// from -n up to 0 like the native loops, so the POpEmitRow selection means
// the same thing on every architecture.
func EmitProgram(insns []ProgInsn, nCols int) ([]byte, error) {
	if nCols > MaxProgCols {
		return nil, fmt.Errorf("jit: %d column blocks, max %d", nCols, MaxProgCols)
	}
	n := len(insns)
	for i, in := range insns {
		if in.Op == POpLoadCol && (in.B < 0 || in.B >= nCols) {
			return nil, fmt.Errorf("jit: column %d out of range", in.B)
		}
		switch in.Op {
		case POpJump, POpJumpIfZero, POpJumpIfNotZero:
			if in.A != ProgNextRow && in.A <= i {
				return nil, fmt.Errorf("jit: backward jump at %d", i)
			}
		}
	}
	w := &Wasm{}
	regs, idx, acc, ovf := w.Local(wI32), w.Local(wI64), w.Local(wI64), w.Local(wI64)
	var colEnd [MaxProgCols]uint32
	for c := range nCols {
		colEnd[c] = w.Local(wI32)
	}
	t1, t2, t3 := w.Local(wI64), w.Local(wI64), w.Local(wI64)

	w.LoadPtr(POffRegs)
	w.Set(regs)
	for _, in := range insns {
		if in.Op == POpAccMin {
			w.I64(math.MaxInt64)
			w.Set(acc)
		} else if in.Op == POpAccMax {
			w.I64(math.MinInt64)
			w.Set(acc)
		}
	}
	// colEnd = Col[c] + n*8; row i reads colEnd + idx*8 with idx in [-n, 0).
	for c := range nCols {
		w.LoadPtr(uint32(POffCol + c*8))
		w.Get(0)
		w.LoadI64(POffN)
		w.op(opI32WrapI64)
		w.I32(3)
		w.op(opI32Shl)
		w.op(opI32Add)
		w.Set(colEnd[c])
	}
	w.I64(0)
	w.Get(0)
	w.LoadI64(POffN)
	w.op(opI64Sub)
	w.Set(idx)

	reg := func(r int) uint32 { return uint32(r * 8) }
	loadReg := func(r int) { w.Get(regs); w.LoadI64(reg(r)) }
	// storeReg stores the i64 that pushVal pushes into r[r].
	storeReg := func(r int, pushVal func()) { w.Get(regs); pushVal(); w.StoreI64(reg(r)) }
	bool64 := func() { w.op(opI64ExtendI32U) }

	w.op(opBlock, opVoid) // $done
	w.Get(idx)
	w.op(opI64Eqz)
	w.BrIf(0)
	w.op(opLoop, opVoid) // $row
	// Blocks: outermost ends at "next", then one per instruction n-1 .. 1.
	for range n {
		w.op(opBlock, opVoid)
	}
	// depthTo is the br depth from inside instruction i to target t; the
	// enclosing blocks at i are those of i+1 .. n (n being "next").
	depthTo := func(i, t int) uint32 {
		if t == ProgNextRow || t >= n {
			t = n
		}
		return uint32(t - i - 1)
	}
	for i, in := range insns {
		switch in.Op {
		case POpLoadCol:
			storeReg(in.A, func() {
				w.Get(colEnd[in.B])
				w.Get(idx)
				w.op(opI32WrapI64)
				w.I32(3)
				w.op(opI32Shl)
				w.op(opI32Add)
				w.LoadI64(0)
			})
		case POpLoadReg:
			storeReg(in.A, func() { loadReg(in.B) })
		case POpCmp:
			storeReg(in.A, func() { loadReg(in.B); loadReg(in.C); w.op(scalarCmp(in.Cond)); bool64() })
		case POpAnd, POpOr:
			op := opI64And
			if in.Op == POpOr {
				op = opI64Or
			}
			storeReg(in.A, func() { loadReg(in.B); loadReg(in.C); w.op(op) })
		case POpSkipIfZero:
			loadReg(in.A)
			w.op(opI64Eqz)
			w.BrIf(depthTo(i, ProgNextRow))
		case POpSetConst:
			storeReg(in.A, func() { w.I64(int64(in.B)) })
		case POpJump:
			w.Br(depthTo(i, in.A))
		case POpJumpIfZero:
			loadReg(in.B)
			w.op(opI64Eqz)
			w.BrIf(depthTo(i, in.A))
		case POpJumpIfNotZero:
			loadReg(in.B)
			w.op(opI64Eqz)
			w.op(opI32Eqz)
			w.BrIf(depthTo(i, in.A))
		case POpNot:
			storeReg(in.A, func() { loadReg(in.B); w.op(opI64Eqz); bool64() })
		case POpRem:
			// rem_s traps on a zero divisor, so a zero is flagged (SQL NULL:
			// the caller declines) and replaced by 1. MinInt64 rem_s -1 is 0,
			// which is already vdbe.c's answer for a divisor of -1.
			loadReg(in.B)
			w.Set(t1)
			loadReg(in.C)
			w.Set(t2)
			w.Get(ovf)
			w.Get(t2)
			w.op(opI64Eqz)
			w.op(opI64ExtendI32U)
			w.op(opI64Or)
			w.Set(ovf)
			w.Get(t1)
			w.I64(1)
			w.Get(t2)
			w.Get(t2)
			w.op(opI64Eqz)
			w.op(opSelect)
			w.op(opI64RemS)
			w.Set(t3)
			storeReg(in.A, func() { w.Get(t3) })
		case POpAbs:
			loadReg(in.B)
			w.Set(t1)
			w.Get(ovf)
			w.Get(t1)
			w.I64(math.MinInt64)
			w.op(opI64Eq)
			w.op(opI64ExtendI32U)
			w.op(opI64Or)
			w.Set(ovf)
			storeReg(in.A, func() {
				w.I64(0)
				w.Get(t1)
				w.op(opI64Sub)
				w.Get(t1)
				w.Get(t1)
				w.I64(0)
				w.op(opI64LtS)
				w.op(opSelect) // t1 < 0 ? -t1 : t1
			})
		case POpAdd, POpSub, POpMul:
			loadReg(in.B)
			w.Set(t1)
			loadReg(in.C)
			w.Set(t2)
			emitCheckedArith(w, in.Op, t1, t2, t3, ovf)
			storeReg(in.A, func() { w.Get(t3) })
		case POpAccCount:
			w.Get(acc)
			w.I64(1)
			w.op(opI64Add)
			w.Set(acc)
		case POpEmitRow:
			// Sel[acc] = idx; acc++.
			w.LoadPtr(POffSel)
			w.Get(acc)
			w.op(opI32WrapI64)
			w.I32(3)
			w.op(opI32Shl)
			w.op(opI32Add)
			w.Get(idx)
			w.StoreI64(0)
			w.Get(acc)
			w.I64(1)
			w.op(opI64Add)
			w.Set(acc)
		case POpAccMin, POpAccMax:
			// acc = v when v beats it.
			loadReg(in.A)
			w.Set(t1)
			w.Get(t1)
			w.Get(acc)
			w.Get(t1)
			w.Get(acc)
			if in.Op == POpAccMin {
				w.op(opI64LtS) // v < acc
			} else {
				w.op(0x55) // i64.gt_s: v > acc
			}
			w.op(opSelect)
			w.Set(acc)
			storeReg(in.B, func() { loadReg(in.B); w.I64(1); w.op(opI64Add) })
		case POpAccSum:
			loadReg(in.A)
			w.Set(t2)
			w.Get(acc)
			w.Set(t1)
			emitCheckedArith(w, POpAdd, t1, t2, acc, ovf)
			storeReg(in.B, func() { loadReg(in.B); w.I64(1); w.op(opI64Add) })
		default:
			return nil, fmt.Errorf("jit: unknown program opcode %d", in.Op)
		}
		w.op(opEnd) // closes the block that targets instruction i+1
	}
	// "next": idx++, and loop while idx != 0.
	w.Get(idx)
	w.I64(1)
	w.op(opI64Add)
	w.Set(idx)
	w.Get(idx)
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	w.BrIf(0)
	w.op(opEnd, opEnd) // $row, $done

	w.LoadPtr(POffOut)
	w.Get(acc)
	w.StoreI64(0)
	w.LoadPtr(POffOverflow)
	w.Get(ovf)
	w.StoreI64(0)
	return w.Module(), nil
}

// emitCheckedArith sets dst = a op b with wrapping arithmetic and ORs 1 into
// ovf when the signed result overflowed, the sticky flag the native emitters
// keep in a register.
func emitCheckedArith(w *Wasm, op POp, a, b, dst, ovf uint32) {
	w.Get(a)
	w.Get(b)
	switch op {
	case POpAdd:
		w.op(opI64Add)
	case POpSub:
		w.op(opI64Sub)
	default:
		w.op(opI64Mul)
	}
	w.Set(dst)

	w.Get(ovf)
	switch op {
	case POpAdd: // ((a^r) & (b^r)) < 0
		w.Get(a)
		w.Get(dst)
		w.op(opI64Xor)
		w.Get(b)
		w.Get(dst)
		w.op(opI64Xor)
		w.op(opI64And)
		w.I64(0)
		w.op(opI64LtS)
	case POpSub: // ((a^b) & (a^r)) < 0
		w.Get(a)
		w.Get(b)
		w.op(opI64Xor)
		w.Get(a)
		w.Get(dst)
		w.op(opI64Xor)
		w.op(opI64And)
		w.I64(0)
		w.op(opI64LtS)
	default:
		// Wasm has no high multiply. a*b overflowed exactly when a == -1 and
		// b == MinInt64, or a is neither 0 nor -1 and r/a != b. The divisor is
		// replaced by 1 in the cases excluded, so div_s can never trap.
		//   ovf |= a == -1 ? b == Min : (a != 0 && r/d != b)
		w.Get(b)
		w.I64(math.MinInt64)
		w.op(opI64Eq) // i32: b == Min

		w.Get(dst)
		w.I64(1)
		w.Get(a)
		w.Get(a)
		w.op(opI64Eqz)
		w.Get(a)
		w.I64(-1)
		w.op(opI64Eq)
		w.op(opI32Or)
		w.op(opSelect) // d = (a == 0 || a == -1) ? 1 : a
		w.op(opI64DivS)
		w.Get(b)
		w.op(opI64Ne)
		w.Get(a)
		w.op(opI64Eqz)
		w.op(opI32Eqz)
		w.op(0x71) // i32.and: a != 0 && r/d != b

		w.Get(a)
		w.I64(-1)
		w.op(opI64Eq)
		w.op(opSelect) // a == -1 ? (b == Min) : (...)
	}
	w.op(opI64ExtendI32U)
	w.op(opI64Or)
	w.Set(ovf)
}
