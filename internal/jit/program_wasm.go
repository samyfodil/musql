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
		if (in.Op == POpLoadCol || in.Op == POpTextLen || in.Op == POpTextMatch) && (in.B < 0 || in.B >= nCols) {
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
	tp, tl, tc, tb := w.Local(wI32), w.Local(wI32), w.Local(wI64), w.Local(wI32) // POpTextLen
	te, ts := w.Local(wI32), w.Local(wI32)                                      // POpTextMatch: the heap's end, a scan pointer

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
	// Re-entry after a POpService (the protocol is in jit.go): restore the
	// loop state the exit stored, and remember which service to resume after.
	svc := progHasService(insns)
	var resume uint32
	if svc {
		resume = w.Local(wI32)
		w.Get(0)
		w.LoadI64(POffPC)
		w.op(opI32WrapI64)
		w.Set(resume)
		w.op(opBlock, opVoid)
		w.Get(resume)
		w.op(opI32Eqz)
		w.BrIf(0)
		w.Get(0)
		w.LoadI64(POffRow)
		w.Set(idx)
		w.LoadPtr(POffOut)
		w.LoadI64(0)
		w.Set(acc)
		w.LoadPtr(POffOverflow)
		w.LoadI64(0)
		w.Set(ovf)
		w.op(opEnd)
	}
	w.Get(idx)
	w.op(opI64Eqz)
	w.BrIf(0)
	w.op(opLoop, opVoid) // $row
	// Blocks: outermost ends at "next", then one per instruction n-1 .. 1.
	for range n {
		w.op(opBlock, opVoid)
	}
	if svc {
		// The first pass through the loop may resume mid-row: one more block,
		// innermost, whose end is instruction 0, and a br_table on the
		// service to resume after -- 0 falls through to instruction 0,
		// service k branches to the instruction after it. The local is
		// cleared as it is read, so every later row starts at the top.
		w.op(opBlock, opVoid)
		maxSvc := 0
		for _, in := range insns {
			if in.Op == POpService {
				maxSvc = max(maxSvc, in.A+1)
			}
		}
		depths := make([]uint32, maxSvc+1) // entries with no service fall through
		for i, in := range insns {
			if in.Op == POpService {
				// From inside the extra block, instruction t is t+1 levels out.
				depths[in.A+1] = uint32(i + 1)
			}
		}
		w.Get(resume)
		w.I32(0)
		w.Set(resume)
		w.BrTable(depths, 0)
		w.op(opEnd)
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
		case POpTextLen:
			emitTextLenWasm(w, in, regs, colEnd[in.B], idx, tp, tl, tc, tb, depthTo(i, in.C))
		case POpTextMatch:
			if len(in.Pat) == 0 || len(in.Pat) > 16 {
				return nil, fmt.Errorf("jit: text pattern of %d bytes", len(in.Pat))
			}
			emitTextMatchWasm(w, in, regs, colEnd[in.B], idx, t1, tp, tl, te, ts, tb)
		case POpService:
			// Stop: PC = service+1, Row = idx, then out to $done past the
			// "finished" store, so the epilogue saves acc and ovf as usual.
			w.Get(0)
			w.I64(int64(in.A) + 1)
			w.StoreI64(POffPC)
			w.Get(0)
			w.Get(idx)
			w.StoreI64(POffRow)
			w.Br(uint32(n-i) + 1)
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
	w.op(opEnd) // $row
	// Finished every row: PC = 0. A service exit branches past this.
	w.Get(0)
	w.I64(0)
	w.StoreI64(POffPC)
	w.op(opEnd) // $done

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

// emitTextLenWasm is emitTextLenAmd64's algorithm in SIMD128: i8x16.bitmask
// of the bytes is their high bits, the non-ASCII test, and of an i8x16.eq
// against zero the NULs, whose first one i32.ctz finds. The chunk is loaded
// twice rather than kept in a v128 local. fallbackDepth is the branch depth
// from instruction i to its fallback target. Structure:
//
//	block $after
//	  block $fb
//	    block $done
//	      loop $lp
//	        block $tail  -- 16-byte chunks while 16 remain
//	          block $nonul
//	          end
//	        end
//	        loop $tl2    -- then one byte at a time
//	        end
//	      end
//	    end
//	    r[A] = count; br $after
//	  end
//	  br fallback
//	end
func emitTextLenWasm(w *Wasm, in ProgInsn, regs, colEnd, idx, tp, tl, tc, tb uint32, fallbackDepth uint32) {
	const (
		i32Load8U  = 0x2D
		i32Sub     = 0x6B
		i32Ctz     = 0x68
		i32LtU     = 0x49
		i64ShrU    = 0x88
		i8x16Splat = 0x0F
		i8x16Eq    = 0x23
		i8x16Bmask = 0x64
	)
	v128Load := func() { w.Get(tp); w.op(opSIMD, simdV128Load, 0, 0) }
	// cell = Col[B][row]; tp = heap + offset; tl = length; tc = 0.
	w.Get(colEnd)
	w.Get(idx)
	w.op(opI32WrapI64)
	w.I32(3)
	w.op(opI32Shl)
	w.op(opI32Add)
	w.LoadI64(0)
	w.Set(tc)
	w.LoadPtr(uint32(POffHeap + in.B*8))
	w.Get(tc)
	w.op(opI32WrapI64)
	w.op(opI32Add)
	w.Set(tp)
	w.Get(tc)
	w.I64(32)
	w.op(i64ShrU)
	w.op(opI32WrapI64)
	w.Set(tl)
	w.I64(0)
	w.Set(tc)

	w.op(opBlock, opVoid) // $after
	w.op(opBlock, opVoid) // $fb
	w.op(opBlock, opVoid) // $done
	w.op(opLoop, opVoid)  // $lp
	w.op(opBlock, opVoid) // $tail: $tail 0, $lp 1, $done 2, $fb 3
	w.Get(tl)
	w.I32(16)
	w.op(i32LtU)
	w.BrIf(0)
	v128Load()
	w.Simd(i8x16Bmask)
	w.BrIf(3) // a byte >= 0x80: $fb
	w.op(opBlock, opVoid) // $nonul: $nonul 0, $tail 1, $lp 2, $done 3
	v128Load()
	w.I32(0)
	w.Simd(i8x16Splat)
	w.Simd(i8x16Eq)
	w.Simd(i8x16Bmask)
	w.Set(tb)
	w.Get(tb)
	w.op(opI32Eqz)
	w.BrIf(0)
	w.Get(tc)
	w.Get(tb)
	w.op(i32Ctz)
	w.op(opI64ExtendI32U)
	w.op(opI64Add)
	w.Set(tc)
	w.Br(3) // $done
	w.op(opEnd) // $nonul
	w.Get(tc)
	w.I64(16)
	w.op(opI64Add)
	w.Set(tc)
	w.Get(tp)
	w.I32(16)
	w.op(opI32Add)
	w.Set(tp)
	w.Get(tl)
	w.I32(16)
	w.op(i32Sub)
	w.Set(tl)
	w.Br(1) // $lp
	w.op(opEnd) // $tail
	w.op(opLoop, opVoid) // $tl2: $tl2 0, $lp 1, $done 2, $fb 3
	w.Get(tl)
	w.op(opI32Eqz)
	w.BrIf(2)
	w.Get(tp)
	w.op(i32Load8U, 0, 0)
	w.Set(tb)
	w.Get(tb)
	w.I32(0x80)
	w.op(opI32GeU)
	w.BrIf(3)
	w.Get(tb)
	w.op(opI32Eqz)
	w.BrIf(2)
	w.Get(tc)
	w.I64(1)
	w.op(opI64Add)
	w.Set(tc)
	w.Get(tp)
	w.I32(1)
	w.op(opI32Add)
	w.Set(tp)
	w.Get(tl)
	w.I32(1)
	w.op(i32Sub)
	w.Set(tl)
	w.Br(0)
	w.op(opEnd) // $tl2
	w.op(opEnd) // $lp
	w.op(opEnd) // $done
	w.Get(regs)
	w.Get(tc)
	w.StoreI64(uint32(in.A * 8))
	w.Br(1) // $after
	w.op(opEnd) // $fb
	w.Br(fallbackDepth + 1)
	w.op(opEnd) // $after
}

// wasmBlocks names the open blocks of a structured emission so a branch says
// where it goes rather than how deep that is.
type wasmBlocks struct {
	w     *Wasm
	stack []string
}

func (b *wasmBlocks) open(op byte, name string) { b.w.op(op, opVoid); b.stack = append(b.stack, name) }
func (b *wasmBlocks) close()                   { b.w.op(opEnd); b.stack = b.stack[:len(b.stack)-1] }
func (b *wasmBlocks) depth(name string) uint32 {
	for i := len(b.stack) - 1; i >= 0; i-- {
		if b.stack[i] == name {
			return uint32(len(b.stack) - 1 - i)
		}
	}
	panic("jit: no open wasm block " + name)
}
func (b *wasmBlocks) br(name string)   { b.w.Br(b.depth(name)) }
func (b *wasmBlocks) brIf(name string) { b.w.BrIf(b.depth(name)) }

// emitTextMatchWasm is emitTextMatchAmd64 in SIMD128. The pattern, its lane
// mask and the fold's vectors are v128.const immediates; the fold is an
// unsigned i8x16.lt_u, and a position matches when (text ^ pattern) & mask has
// no bit set (v128.any_true).
func emitTextMatchWasm(w *Wasm, in ProgInsn, regs, colEnd, idx, t1, tp, tl, te, ts, tb uint32) {
	const (
		i32Load8U  = 0x2D
		i32Sub     = 0x6B
		i32GtU     = 0x4B
		i32LtU     = 0x49
		i32Ne      = 0x47
		i64ShrU    = 0x88
		v128Const  = 0x0C
		i8x16Sub   = 0x71
		i8x16LtU   = 0x26
		v128And    = 0x4E
		v128Or     = 0x50
		v128Xor    = 0x51
		v128AnyTru = 0x53
	)
	m := len(in.Pat)
	b := &wasmBlocks{w: w}
	vconst := func(v [16]byte) {
		w.op(opSIMD)
		w.body = uleb(w.body, v128Const)
		w.op(v[:]...)
	}
	splat := func(c byte) (v [16]byte) {
		for i := range v {
			v[i] = c
		}
		return v
	}
	var pat, mask [16]byte
	copy(pat[:], in.Pat)
	for j := range in.Pat {
		mask[j] = 0xFF
	}
	byteAt := func(off int) {
		w.Get(tp)
		w.op(i32Load8U, 0)
		w.body = uleb(w.body, uint64(off))
	}

	// t1 = the cell; tp = the text, tl = its length, te = the heap's end.
	w.Get(colEnd)
	w.Get(idx)
	w.op(opI32WrapI64)
	w.I32(3)
	w.op(opI32Shl)
	w.op(opI32Add)
	w.LoadI64(0)
	w.Set(t1)
	w.LoadPtr(uint32(POffHeap + in.B*8))
	w.Set(te)
	w.Get(te)
	w.Get(t1)
	w.op(opI32WrapI64)
	w.op(opI32Add)
	w.Set(tp)
	w.Get(te)
	w.Get(0)
	w.LoadI64(uint32(POffHeapLen + in.B*8))
	w.op(opI32WrapI64)
	w.op(opI32Add)
	w.Set(te)
	w.Get(t1)
	w.I64(32)
	w.op(i64ShrU)
	w.op(opI32WrapI64)
	w.Set(tl)

	// matchAt branches to fail unless the pattern is at tp.
	matchAt := func(fail string) {
		b.open(opBlock, "done")
		b.open(opBlock, "scalar")
		w.Get(tp)
		w.I32(16)
		w.op(opI32Add)
		w.Get(te)
		w.op(i32GtU)
		b.brIf("scalar")
		w.Get(tp)
		w.op(opSIMD, simdV128Load, 0, 0)
		if in.Fold {
			// v | ((v - 'A' <u 26) & 0x20), the chunk loaded again rather
			// than kept in a v128 local.
			w.Get(tp)
			w.op(opSIMD, simdV128Load, 0, 0)
			vconst(splat('A'))
			w.Simd(i8x16Sub)
			vconst(splat(26))
			w.Simd(i8x16LtU)
			vconst(splat(0x20))
			w.Simd(v128And)
			w.Simd(v128Or)
		}
		vconst(pat)
		w.Simd(v128Xor)
		vconst(mask)
		w.Simd(v128And)
		w.Simd(v128AnyTru)
		b.brIf(fail)
		b.br("done")
		b.close() // scalar
		for j, p := range in.Pat {
			if in.Fold && p >= 'a' && p <= 'z' {
				// (byte | 0x20) == p, sound because p is a letter: only 'A'+n
				// and 'a'+n map onto it.
				byteAt(j)
				w.I32(0x20)
				w.op(0x72) // i32.or
			} else {
				byteAt(j)
			}
			w.I32(int32(p))
			w.op(i32Ne)
			b.brIf(fail)
		}
		b.close() // done
	}

	b.open(opBlock, "end")
	b.open(opBlock, "no")
	b.open(opBlock, "yes")
	w.Get(tl)
	w.I32(int32(m))
	w.op(i32LtU)
	b.brIf("no")
	switch in.Mode {
	case TextEq:
		b.open(opBlock, "eqok")
		w.Get(tl)
		w.I32(int32(m))
		w.op(0x46) // i32.eq
		b.brIf("eqok")
		byteAt(m)
		b.brIf("no") // a byte other than NUL at m: the text is longer
		b.close()
		matchAt("no")
		b.br("yes")
	case TextPrefix:
		matchAt("no")
		b.br("yes")
	case TextSuffix:
		// The text ends at its first NUL: scan for it (16 bytes at a time
		// while they lie inside the text, then byte by byte) with ts, then
		// the last m bytes before it must be the pattern.
		w.Get(tp)
		w.Set(ts)
		w.Get(tp)
		w.Get(tl)
		w.op(opI32Add)
		w.Set(tl) // tl = the text's end, while scanning
		b.open(opBlock, "eff")
		b.open(opLoop, "slp")
		b.open(opBlock, "stail")
		w.Get(ts)
		w.I32(16)
		w.op(opI32Add)
		w.Get(tl)
		w.op(i32GtU)
		b.brIf("stail")
		w.Get(ts)
		w.op(opSIMD, simdV128Load, 0, 0)
		w.I32(0)
		w.Simd(0x0F) // i8x16.splat
		w.Simd(0x23) // i8x16.eq
		w.Simd(0x64) // i8x16.bitmask
		w.Set(tb)
		b.open(opBlock, "nonul")
		w.Get(tb)
		w.op(opI32Eqz)
		b.brIf("nonul")
		w.Get(ts)
		w.Get(tb)
		w.op(0x68) // i32.ctz
		w.op(opI32Add)
		w.Set(ts)
		b.br("eff")
		b.close() // nonul
		w.Get(ts)
		w.I32(16)
		w.op(opI32Add)
		w.Set(ts)
		b.br("slp")
		b.close() // stail
		b.open(opLoop, "sbyte")
		w.Get(ts)
		w.Get(tl)
		w.op(0x46) // i32.eq
		b.brIf("eff")
		w.Get(ts)
		w.op(i32Load8U, 0, 0)
		w.op(opI32Eqz)
		b.brIf("eff")
		w.Get(ts)
		w.I32(1)
		w.op(opI32Add)
		w.Set(ts)
		b.br("sbyte")
		b.close() // sbyte
		b.close() // slp
		b.close() // eff
		w.Get(ts)
		w.Get(tp)
		w.op(i32Sub)
		w.Set(tl) // the effective length
		w.Get(tl)
		w.I32(int32(m))
		w.op(i32LtU)
		b.brIf("no")
		w.Get(tp)
		w.Get(tl)
		w.op(opI32Add)
		w.I32(int32(m))
		w.op(i32Sub)
		w.Set(tp)
		matchAt("no")
		b.br("yes")
	case TextContains:
		b.open(opLoop, "lp")
		w.Get(tl)
		w.I32(int32(m))
		w.op(i32LtU)
		b.brIf("no")
		byteAt(0)
		w.op(opI32Eqz)
		b.brIf("no")
		b.open(opBlock, "next")
		matchAt("next")
		b.br("yes")
		b.close() // next
		w.Get(tp)
		w.I32(1)
		w.op(opI32Add)
		w.Set(tp)
		w.Get(tl)
		w.I32(1)
		w.op(i32Sub)
		w.Set(tl)
		b.br("lp")
		b.close() // lp
	}
	b.close() // yes
	w.Get(regs)
	w.I64(1)
	w.StoreI64(uint32(in.A * 8))
	b.br("end")
	b.close() // no
	w.Get(regs)
	w.I64(0)
	w.StoreI64(uint32(in.A * 8))
	b.close() // end
}
