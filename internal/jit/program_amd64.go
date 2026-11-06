//go:build amd64

package jit

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
)

// EmitProgram compiles an instruction list into a native row loop. The loop
// is emitted once per program; the body is whatever the instruction list says.
func emitProgram(insns []ProgInsn, nCols int) ([]byte, error) {
	a0 := NewAsm()
	if nCols > MaxProgCols {
		return nil, fmt.Errorf("jit: %d column blocks, max %d", nCols, MaxProgCols)
	}
	for _, in := range insns {
		if (in.Op == POpLoadCol || in.Op == POpTextLen || in.Op == POpTextMatch || in.BCol) && (in.B < 0 || in.B >= nCols) {
			return nil, fmt.Errorf("jit: column %d out of range", in.B)
		}
		if in.CCol && (in.C < 0 || in.C >= nCols) {
			return nil, fmt.Errorf("jit: column %d out of range", in.C)
		}
	}
	colReg := [MaxProgCols]Reg{RBX, R12, R13, R15}
	// The column registers this program does not read hold its IR registers
	// (allocProgRegs); the trampoline saves all four either way. So do R11,
	// which only the text steps use as scratch, and R9, the overflow flag,
	// unless a service may resume the run: the flag then lives in the file
	// the caller reads (POffOverflow), written as the run ends at ovf.
	pool := append([]Reg(nil), colReg[nCols:]...)
	text := slices.ContainsFunc(insns, func(in ProgInsn) bool { return in.Op == POpTextLen || in.Op == POpTextMatch })
	flagInFile := !progHasService(insns)
	if !text {
		pool = append(pool, R11)
	}
	if flagInFile {
		pool = append(pool, R9)
	}
	// R8 is the accumulator; a program that accumulates only in registers
	// (POpRAccSum and kin) has it to spare, and answers 0.
	accFree := !slices.ContainsFunc(insns, usesAcc)
	if accFree {
		pool = append(pool, R8)
	}
	// R10 is scratch for a predicated row and the steps below; a program
	// with none of them has it to spare too.
	ps, pred := progPredicated(insns)
	if !pred && !slices.ContainsFunc(insns, func(in ProgInsn) bool {
		switch in.Op {
		case POpCmp, POpNot, POpRemK, POpRem, POpFCmp, POpFIsNull, POpTextLen, POpTextMatch, POpMath:
			return true
		}
		return false
	}) {
		pool = append(pool, R10)
	}
	pa := allocProgRegs(insns, len(pool))
	phys := func(r int) (Reg, bool) {
		if k := pa.at(r); k >= 0 {
			return pool[k], true
		}
		return 0, false
	}
	consts := newFirPool()
	// fval puts r's REAL in x; fset makes x r's value.
	fval := func(r int, x XReg) {
		if p, ok := phys(r); ok {
			a0.MovqXR(x, p)
			return
		}
		a0.MovsdLoad(x, RSI, int32(r*8))
	}
	fset := func(r int, x XReg) {
		if p, ok := phys(r); ok {
			a0.MovqRX(p, x)
			return
		}
		a0.MovsdStore(RSI, int32(r*8), x)
	}
	// val is a register holding r's value: its own, or scratch loaded from
	// the file.
	val := func(r int, scratch Reg) Reg {
		if p, ok := phys(r); ok {
			return p
		}
		a0.MovRegMem32(scratch, RSI, r)
		return scratch
	}
	// set makes r's value src.
	set := func(r int, src Reg) {
		if p, ok := phys(r); ok {
			if p != src {
				a0.MovRegReg(p, src)
			}
			return
		}
		a0.MovMemReg32(RSI, r, src)
	}
	// into is val, but the value lands in dst (which the caller then changes).
	into := func(dst Reg, r int) {
		if p, ok := phys(r); ok {
			if p != dst {
				a0.MovRegReg(dst, p)
			}
			return
		}
		a0.MovRegMem32(dst, RSI, r)
	}

	// operand is into, or a column slot's value at the row when col.
	operand := func(dst Reg, r int, col bool) {
		if col {
			a0.MovRegMemIdx(dst, colReg[r], RCX)
			return
		}
		into(dst, r)
	}

	a := a0
	// incReg is r++, a row counter.
	incReg := func(r int) {
		if p, ok := phys(r); ok {
			a.IncReg(p)
			return
		}
		a.MovRegMem32(RDX, RSI, r)
		a.IncReg(RDX)
		a.MovMemReg32(RSI, r, RDX)
	}
	a.MovRegMem(RSI, RDI, POffRegs)
	a.MovRegMem(RCX, RDI, POffN)
	a.XorRegReg(R8, R8)
	// Min/max programs seed the accumulator to avoid a first-row check.
	for _, in := range insns {
		if in.Op == POpAccMin {
			a.MovRegImm64(R8, math.MaxInt64)
		} else if in.Op == POpAccMax {
			a.MovRegImm64(R8, math.MinInt64)
		}
	}
	// A step that would flag an overflow jumps to ovf instead, which ends the
	// run: the caller declines a flagged one whatever else it holds, so the
	// rows after cost nothing and the hot path carries no flag.
	if flagInFile {
		a.MovRegMem(RDX, RDI, POffOverflow)
		a.MovMemDImm(RDX, 0, 0)
	} else {
		a.XorRegReg(R9, R9)
	}
	for c := 0; c < nCols; c++ {
		a.MovRegMem(colReg[c], RDI, int8(POffCol+c*8))
	}
	// A REAL accumulator lives in X14 (and its compensation in X13) for the
	// whole call; kernels leave both alone.
	hasFloat := progHasFloat(insns)
	if hasFloat {
		a.MovsdLoad(X14, RDI, POffFAcc)
		a.MovsdLoad(X13, RDI, POffFAcc+8)
	}
	for _, r := range pa.load {
		p, _ := phys(r)
		a.MovRegMem32(p, RSI, r)
	}
	a.TestRegReg(RCX, RCX)
	a.Jcc(CondE, "done")
	// The row index counts UP from -n to 0 so one INC both advances every
	// column read and tests for the end -- the same trick the fixed kernels
	// use, generalized to however many columns this program reads.
	for c := 0; c < nCols; c++ {
		a.LeaIdx(colReg[c], colReg[c], RCX)
	}
	a.NegReg(RCX)
	emitProgResume(a, insns)

	padNops(a, loopAlign)
	a.Label("loop")
	// One label per IR instruction, so a lowered OpIf/OpIfNot/OpGoto becomes an
	// ordinary branch. Without these the IR could only express a straight line
	// of predicates -- which is exactly the limit the fixed kernels had.
	for idx, in := range insns {
		a.Label(progLabel(idx))
		if pred && idx >= ps {
			// The predicated row (progPredicated): R10 is 1 while every filter
			// so far passed -- SETcc for the first, then a CMOV of zero for
			// each failing one -- and the accumulators apply it.
			switch in.Op {
			case POpCmpSkip:
				x := RAX
				if in.BCol {
					a.MovRegMemIdx(RAX, colReg[in.B], RCX)
				} else {
					x = val(in.B, RAX)
				}
				y := val(in.C, RDX)
				if idx == ps {
					a.XorRegReg(R10, R10) // before the compare: XOR sets the flags
					a.CmpRegReg(x, y)
					a.Setcc(in.Cond, R10)
				} else {
					a.CmpRegReg(x, y)
					a.MovRegImm64(RDX, 0) // MOV leaves the flags alone
					a.Cmovcc(in.Cond.Negate(), R10, RDX)
				}
			case POpLoadCol:
				if p, ok := phys(in.A); ok {
					a.MovRegMemIdx(p, colReg[in.B], RCX)
					break
				}
				a.MovRegMemIdx(RAX, colReg[in.B], RCX)
				a.MovMemReg32(RSI, in.A, RAX)
			case POpAccCount:
				a.AddRegReg(R8, R10)
			case POpAccSum:
				x := val(in.A, RAX)
				a.MovRegReg(RDX, R10)
				a.NegReg(RDX)
				a.AndRegReg(RDX, x)
				a.AddRegReg(R8, RDX)
				a.Jcc(condOverflow, "ovf")
				if p, ok := phys(in.B); ok {
					a.AddRegReg(p, R10)
				} else {
					a.MovRegMem32(RDX, RSI, in.B)
					a.AddRegReg(RDX, R10)
					a.MovMemReg32(RSI, in.B, RDX)
				}
			}
			continue
		}
		switch in.Op {
		case POpLoadCol:
			if p, ok := phys(in.A); ok {
				a.MovRegMemIdx(p, colReg[in.B], RCX)
				break
			}
			a.MovRegMemIdx(RAX, colReg[in.B], RCX)
			a.MovMemReg32(RSI, in.A, RAX)
		case POpLoadReg:
			set(in.A, val(in.B, RAX))
		case POpCmp:
			x, y := val(in.B, RAX), val(in.C, RDX)
			// R10 is zeroed BEFORE the compare. XOR sets the flags, so zeroing
			// it afterwards destroyed the comparison SETcc was about to read --
			// every predicate came back constant, which the differential caught
			// as "program 0, Go 1339" and "program 5000, Go 1588": all rows
			// rejected under AND, all accepted under OR.
			a.XorRegReg(R10, R10)
			a.CmpRegReg(x, y)
			a.Setcc(in.Cond, R10)
			set(in.A, R10)
		case POpAnd, POpOr:
			op := a.AndRegReg
			if in.Op == POpOr {
				op = a.OrRegReg
			}
			// In place when the result overwrites an operand that lives in
			// a register: both are commutative.
			if p, ok := phys(in.A); ok && (in.A == in.B || in.A == in.C) {
				other := in.C
				if in.A == in.C {
					other = in.B
				}
				op(p, val(other, RDX))
				break
			}
			into(RAX, in.B)
			op(RAX, val(in.C, RDX))
			set(in.A, RAX)
		case POpSkipIfZero:
			x := val(in.A, RAX)
			a.TestRegReg(x, x)
			a.Jcc(CondE, "next")
		case POpCmpSkip, POpCmpJump:
			x := RAX
			if in.BCol {
				a.MovRegMemIdx(RAX, colReg[in.B], RCX)
			} else {
				x = val(in.B, RAX)
			}
			a.CmpRegReg(x, val(in.C, RDX))
			if in.Op == POpCmpSkip {
				a.Jcc(in.Cond.Negate(), "next")
			} else {
				a.Jcc(in.Cond, progTarget(in.A, len(insns)))
			}
		case POpSetConst:
			if p, ok := phys(in.A); ok {
				a.MovRegImm64(p, int64(in.B))
				break
			}
			a.MovRegImm64(RAX, int64(in.B))
			a.MovMemReg32(RSI, in.A, RAX)
		case POpJump:
			a.Jmp(progTarget(in.A, len(insns)))
		case POpJumpIfZero, POpJumpIfNotZero:
			x := val(in.B, RAX)
			a.TestRegReg(x, x)
			cond := CondE
			if in.Op == POpJumpIfNotZero {
				cond = CondNE
			}
			a.Jcc(cond, progTarget(in.A, len(insns)))
		case POpNot:
			x := val(in.B, RAX)
			a.XorRegReg(R10, R10)
			a.TestRegReg(x, x)
			a.Setcc(CondE, R10)
			set(in.A, R10)
		case POpAdd, POpSub, POpMul:
			// x86's two-operand form computes in the result's own register
			// when it has one -- its left operand there already when the two
			// are one (r = r + c) or the operand died into it (readsFirst) --
			// unless that register holds c. A column operand (BCol, CCol) is
			// read at the row, c as a memory operand; a constant one (CImm) is
			// an immediate.
			c := RDX
			if !in.CCol && !in.CImm {
				c = val(in.C, RDX)
			}
			dst := RAX
			if p, ok := phys(in.A); ok && (in.CCol || in.CImm || p != c) {
				dst = p
			}
			switch {
			case in.CImm && in.Op == POpMul:
				if in.BCol {
					a.ImulRegMemIdxImm32(dst, colReg[in.B], RCX, int32(in.Imm))
				} else {
					a.ImulRegRegImm32(dst, val(in.B, dst), int32(in.Imm))
				}
				a.Jcc(condOverflow, "ovf")
				set(in.A, dst)
				continue
			case in.BCol:
				a.MovRegMemIdx(dst, colReg[in.B], RCX)
			default:
				into(dst, in.B)
			}
			switch {
			case in.CImm && in.Op == POpAdd:
				a.AddRegImm32(dst, int32(in.Imm))
			case in.CImm:
				a.SubRegImm32(dst, int32(in.Imm))
			case in.CCol && in.Op == POpAdd:
				a.AddRegMemIdx(dst, colReg[in.C], RCX)
			case in.CCol && in.Op == POpSub:
				a.SubRegMemIdx(dst, colReg[in.C], RCX)
			case in.CCol:
				a.ImulRegMemIdx(dst, colReg[in.C], RCX)
			case in.Op == POpAdd:
				a.AddRegReg(dst, c)
			case in.Op == POpSub:
				a.SubRegReg(dst, c)
			default:
				a.ImulRegReg(dst, c)
			}
			a.Jcc(condOverflow, "ovf")
			set(in.A, dst)
		case POpRemK:
			// A multiply by Imm's magic number and shifts for the truncated
			// quotient (vm_div_amd64.go's divConst), then n - q*Imm.
			d := in.Imm
			operand(R10, in.B, in.BCol)
			if d&(d-1) == 0 {
				k := byte(bits.TrailingZeros64(uint64(d)))
				a.MovRegReg(RDX, R10)
				a.SarRegImm8(RDX, 63)
				a.ShrRegImm8(RDX, 64-k)
				a.AddRegReg(RDX, R10)
				a.SarRegImm8(RDX, k)
			} else {
				m, s := sdivMagic(d)
				a.MovRegImm64(RAX, m)
				a.ImulRDXRAX(R10)
				if m < 0 {
					a.AddRegReg(RDX, R10)
				}
				if s > 0 {
					a.SarRegImm8(RDX, byte(s))
				}
				a.MovRegReg(RAX, R10)
				a.ShrRegImm8(RAX, 63)
				a.AddRegReg(RDX, RAX)
			}
			a.ImulRegRegImm32(RDX, RDX, int32(d))
			a.SubRegReg(R10, RDX)
			set(in.A, R10)
		case POpShl, POpShr:
			// x86 shifts by CL, and RCX is the row index: the amount goes in
			// RDX and is swapped into RCX around the shift.
			operand(RAX, in.B, in.BCol)
			operand(RDX, in.C, in.CCol)
			a.CmpRegImm32(RDX, 63)
			a.Jcc(condAbove, "ovf") // unsigned: a negative amount is huge
			a.XchgRegReg(RCX, RDX)
			if in.Op == POpShl {
				a.ShlRegCL(RAX)
			} else {
				a.SarRegCL(RAX)
			}
			a.XchgRegReg(RCX, RDX)
			set(in.A, RAX)
		case POpRem:
			// RCX is the loop index and R9 the overflow flag, so the divisor
			// goes in R10; idiv clobbers RDX, which is scratch here. A zero
			// divisor (a NULL) is the overflow.
			m1, st := "rm"+itoa(idx), "rs"+itoa(idx)
			operand(RAX, in.B, in.BCol)
			operand(R10, in.C, in.CCol)
			a.TestRegReg(R10, R10)
			a.Jcc(CondE, "ovf")
			a.CmpRegImm32(R10, -1)
			a.Jcc(CondE, m1)
			a.Cqo()
			a.IdivReg(R10)
			a.MovRegReg(RAX, RDX)
			a.Jmp(st)
			a.Label(m1)
			a.XorRegReg(RAX, RAX)
			a.Label(st)
			set(in.A, RAX)
		case POpAbs:
			// NEG sets OF exactly for MinInt64, and SF when the original was
			// positive, in which case the original is the answer.
			into(RAX, in.B)
			a.MovRegReg(RDX, RAX)
			a.NegReg(RDX)
			a.Jcc(condOverflow, "ovf") // a jump leaves the flags for the CMOVcc
			a.Cmovcc(condSign, RDX, RAX)
			set(in.A, RDX)
		case POpService:
			a.MovRegImm64(RAX, int64(in.A)+1)
			a.MovMemReg(RDI, POffPC, RAX)
			a.MovMemReg(RDI, POffRow, RCX)
			a.Jmp("fin")
			a.Label(svcLabel(in.A))
		case POpTextLen:
			emitTextLenAmd64(a, in, colReg[in.B], progTarget(in.C, len(insns)), idx)
		case POpTextMatch:
			if len(in.Pat) == 0 || len(in.Pat) > 16 {
				return nil, fmt.Errorf("jit: text pattern of %d bytes", len(in.Pat))
			}
			emitTextMatchAmd64(a, in, colReg[in.B], idx)
		case POpAccCount:
			a.IncReg(R8)
		case POpEmitRow:
			// Sel[acc] = the loop index; acc++. RAX is scratch, and RCX holds
			// the index the loop is already counting on.
			a.MovRegMem(RAX, RDI, POffSel)
			a.MovMemIdxReg(RAX, R8, RCX)
			a.IncReg(R8)
		case POpEmitVal:
			// Sel[acc] = r[A]; acc++. The value may be in RAX, so the base
			// goes in RDX.
			x := val(in.A, RAX)
			a.MovRegMem(RDX, RDI, POffSel)
			a.MovMemIdxReg(RDX, R8, x)
			a.IncReg(R8)
		case POpAccMin, POpAccMax:
			x := val(in.A, RAX)
			a.CmpRegReg(R8, x)
			cond := CondG // min: acc > v, so take v
			if in.Op == POpAccMax {
				cond = CondL
			}
			a.Cmovcc(cond, R8, x)
			incReg(in.B)
		case POpAccSum:
			a.AddRegReg(R8, val(in.A, RAX))
			a.Jcc(condOverflow, "ovf")
			incReg(in.B)
		case POpRAccCount:
			incReg(in.A)
		case POpRAccSum, POpRAccMin, POpRAccMax:
			x := val(in.B, RDX)
			acc := RAX
			if p, ok := phys(in.A); ok && p != x {
				acc = p
			}
			into(acc, in.A)
			if in.Op == POpRAccSum {
				a.AddRegReg(acc, x)
				a.Jcc(condOverflow, "ovf")
			} else {
				a.CmpRegReg(acc, x)
				cond := CondG // min: acc > v, so take v
				if in.Op == POpRAccMax {
					cond = CondL
				}
				a.Cmovcc(cond, acc, x)
			}
			set(in.A, acc)
		case POpI2F:
			x := val(in.B, RAX)
			a.Xorps(X0, X0)
			a.Cvtsi2sdq(X0, x)
			fset(in.A, X0)
		case POpF2I:
			// CVTTSD2SI answers MinInt64 for a NaN and past either end; CAST
			// answers MaxInt64 from 2^63 up, and a NULL has no INTEGER.
			nan, big, done := "fn"+itoa(idx), "fb"+itoa(idx), "fi"+itoa(idx)
			fval(in.B, X0)
			a.Cvttsd2siq(RAX, X0)
			a.MovsdRIP(X1, consts.label(math.Float64bits(0x1p63), 0))
			a.Ucomisd(X0, X1)
			a.Jcc(ccP, nan)
			a.Jcc(ccAE, big)
			a.Jmp(done)
			a.Label(big)
			a.MovRegImm64(RAX, math.MaxInt64)
			a.Jmp(done)
			a.Label(nan)
			a.Jmp("ovf")
			a.Label(done)
			set(in.A, RAX)
		case POpFAdd, POpFSub, POpFMul, POpFDiv:
			fval(in.B, X0)
			fval(in.C, X1)
			switch in.Op {
			case POpFAdd:
				a.Addsd(X0, X1)
			case POpFSub:
				a.Subsd(X0, X1)
			case POpFMul:
				a.Mulsd(X0, X1)
			default:
				// A divisor equal to zero (either sign) is NULL; a NaN one
				// divides to NaN anyway.
				ok, done := "dv"+itoa(idx), "dd"+itoa(idx)
				a.Xorps(X2, X2)
				a.Ucomisd(X1, X2)
				a.Jcc(ccP, ok)
				a.Jcc(CondNE, ok)
				a.MovsdRIP(X0, consts.label(progNaN, 0))
				a.Jmp(done)
				a.Label(ok)
				a.Divsd(X0, X1)
				a.Label(done)
			}
			fset(in.A, X0)
		case POpFCmp:
			fval(in.B, X0)
			fval(in.C, X1)
			unord, done := "fu"+itoa(idx), "fd"+itoa(idx)
			a.XorRegReg(R10, R10)
			a.Ucomisd(X0, X1)
			a.Jcc(ccP, unord)
			a.Setcc(floatCond(in.Cond), R10)
			a.Jmp(done)
			a.Label(unord)
			if in.Imm == ProgNullDeclines {
				a.Jmp("ovf")
			} else if in.Imm != 0 {
				a.MovRegImm64(R10, in.Imm)
			}
			a.Label(done)
			set(in.A, R10)
		case POpFIsNull:
			fval(in.B, X0)
			a.XorRegReg(R10, R10)
			a.Ucomisd(X0, X0)
			a.Setcc(ccP, R10)
			set(in.A, R10)
		case POpFAbs:
			keep := "fa" + itoa(idx)
			fval(in.B, X0)
			a.Xorps(X1, X1)
			a.Ucomisd(X1, X0) // 0 > x
			a.Jcc(ccBE, keep)
			a.XorpdRIP(X0, consts.label(1<<63, 0))
			a.Label(keep)
			fset(in.A, X0)
		case POpFAccSum:
			// kahanBabuskaNeumaierStep: t = s + r; the error of whichever
			// of s and r is larger in magnitude goes into the compensation.
			skip, small, step := "ks"+itoa(idx), "kl"+itoa(idx), "kt"+itoa(idx)
			fval(in.A, X0)
			a.Ucomisd(X0, X0)
			a.Jcc(ccP, skip)
			a.Movapd(X1, X14)
			a.Addsd(X1, X0) // t
			a.Movapd(X2, X14)
			a.AndpdRIP(X2, consts.label(math.MaxInt64, 0))
			a.Movapd(X3, X0)
			a.AndpdRIP(X3, consts.label(math.MaxInt64, 0))
			a.Ucomisd(X2, X3) // |s| > |r|
			a.Jcc(ccBE, small)
			a.Movapd(X2, X14)
			a.Subsd(X2, X1)
			a.Addsd(X2, X0) // (s - t) + r
			a.Jmp(step)
			a.Label(small)
			a.Movapd(X2, X0)
			a.Subsd(X2, X1)
			a.Addsd(X2, X14) // (r - t) + s
			a.Label(step)
			a.Addsd(X13, X2)
			a.Movapd(X14, X1)
			incReg(in.B)
			a.Label(skip)
		case POpFAccMin, POpFAccMax:
			skip, keep := "ms"+itoa(idx), "mk"+itoa(idx)
			fval(in.A, X0)
			a.Ucomisd(X0, X0)
			a.Jcc(ccP, skip)
			if in.Op == POpFAccMin {
				a.Ucomisd(X14, X0) // acc > x
			} else {
				a.Ucomisd(X0, X14) // x > acc
			}
			a.Jcc(ccBE, keep)
			a.Movapd(X14, X0)
			a.Label(keep)
			incReg(in.B)
			a.Label(skip)
		case POpFAccCount:
			skip := "cs" + itoa(idx)
			fval(in.A, X0)
			a.Ucomisd(X0, X0)
			a.Jcc(ccP, skip)
			a.IncReg(R8)
			a.Label(skip)
		case POpMath:
			if in.Kernel == nil || in.Kernel.NArgs > 2 {
				return nil, fmt.Errorf("jit: POpMath without a kernel")
			}
			svc := int(in.Imm)
			env := &firEnv{
				args:      []firLoc{firMem(RSI, int32(in.B*8)), firMem(RSI, int32(in.C*8))}[:in.Kernel.NArgs],
				out:       firMem(RSI, int32(in.A*8)),
				exit:      "mx" + itoa(idx),
				gprs:      progMathGPRs(pool),
				xmms:      []XReg{X0, X1, X2, X3, X4, X5, X6, X7, X8, X9, X10, X11, X12},
				spillBase: RDI, spillDisp: POffScratch, spillN: ProgScratch,
				prefix: "m" + itoa(idx) + "_", pool: consts,
			}
			if len(pa.slot) != 0 {
				return nil, fmt.Errorf("jit: POpMath in a program with allocated registers")
			}
			if err := emitFirAmd64(a, in.Kernel, env, cpuFeat); err != nil {
				return nil, err
			}
			if in.Imm == ProgMathSelects {
				// An input the kernel cannot judge selects the row.
				at := progEmitAt(insns)
				if at < 0 {
					return nil, fmt.Errorf("jit: POpMath selects in a program with no POpEmitRow")
				}
				after := "ma" + itoa(idx)
				a.Jmp(after)
				a.Label(env.exit)
				a.Jmp(progLabel(at))
				a.Label(after)
				break
			}
			a.Jmp(svcLabel(svc))
			a.Label(env.exit)
			a.MovRegImm64(RAX, in.Imm+1)
			a.MovMemReg(RDI, POffPC, RAX)
			a.MovMemReg(RDI, POffRow, RCX)
			a.Jmp("fin")
			a.Label(svcLabel(svc))
		default:
			return nil, fmt.Errorf("jit: unknown program opcode %d", in.Op)
		}
	}
	a.Label("next")
	a.IncReg(RCX)
	a.Jcc(CondNE, "loop")

	a.Jmp("done")
	a.Label("ovf")
	if flagInFile {
		a.MovRegMem(RDX, RDI, POffOverflow)
		a.MovMemDImm(RDX, 0, 1)
	} else {
		a.MovRegImm64(R9, 1)
	}

	a.Label("done")
	for _, r := range pa.store {
		p, _ := phys(r)
		a.MovMemReg32(RSI, r, p)
	}
	a.XorRegReg(RAX, RAX)
	a.MovMemReg(RDI, POffPC, RAX)
	a.Label("fin")
	a.MovRegMem(RDX, RDI, POffOut)
	if accFree {
		a.MovMemDImm(RDX, 0, 0)
	} else {
		a.MovMemReg(RDX, 0, R8)
	}
	if !flagInFile {
		a.MovRegMem(RDX, RDI, POffOverflow)
		a.MovMemReg(RDX, 0, R9)
	}
	if hasFloat {
		a.MovsdStore(RDI, POffFAcc, X14)
		a.MovsdStore(RDI, POffFAcc+8, X13)
	}
	a.Ret()
	emitTextPool(a, insns)
	consts.emit(a)
	return a.Code()
}

// progMathGPRs is the integer registers a POpMath kernel may use: the scratch
// ones and the pool, R11 among them once.
func progMathGPRs(pool []Reg) []Reg {
	gprs := []Reg{RAX, RDX, R10}
	if !slices.Contains(pool, R11) {
		gprs = append(gprs, R11)
	}
	return append(gprs, pool...)
}

// progNaN is the NaN a program writes for a NULL REAL.
const progNaN = 0x7ff8000000000000

// floatCond is the condition UCOMISD x, y leaves for x c y, given an ordered
// pair: it compares like unsigned integers.
func floatCond(c Cond) Cond {
	switch c {
	case CondG:
		return ccA
	case CondGE:
		return ccAE
	case CondL:
		return ccB
	case CondLE:
		return ccBE
	}
	return c
}

// progLabel names the emitted label for one IR index.
func progLabel(i int) string { return "i" + itoa(i) }

// progTarget resolves an IR jump target to a label: ProgNextRow and anything
// past the last instruction both mean "start the next row".
func progTarget(target, n int) string {
	if target == ProgNextRow || target >= n {
		return "next"
	}
	return progLabel(target)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// emitProgResume re-enters a program a POpService stopped: with PC non-zero,
// restore the loop index, the accumulator and the overflow flag the exit
// stored, and jump to the instruction after that service. The column bases
// were just rebuilt from N exactly as for a fresh start, which is what the
// stored index is relative to.
func emitProgResume(a *Asm, insns []ProgInsn) {
	if !progHasService(insns) {
		return
	}
	a.MovRegMem(RAX, RDI, POffPC)
	a.TestRegReg(RAX, RAX)
	a.Jcc(CondE, "loop")
	a.MovRegMem(RCX, RDI, POffRow)
	a.MovRegMem(RDX, RDI, POffOut)
	a.MovRegMem(R8, RDX, 0)
	a.MovRegMem(RDX, RDI, POffOverflow)
	a.MovRegMem(R9, RDX, 0)
	for _, in := range insns {
		if k, ok := progService(in); ok {
			a.CmpRegImm32(RAX, int32(k)+1)
			a.Jcc(CondE, svcLabel(k))
		}
	}
	a.Jmp("done") // no such service: finish rather than guess
}

// emitTextLenAmd64 counts the TEXT cell's characters before its first NUL, 16
// bytes at a time while 16 remain in the cell (so it never reads past it),
// then a byte at a time. PMOVMSKB of the bytes is their high bits, the
// non-ASCII test; against zero it finds the NUL. RAX, RDX, R10 and R11 are
// scratch.
func emitTextLenAmd64(a *Asm, in ProgInsn, cells Reg, fallback string, idx int) {
	lp, nul, tail, done, fb, after := "tl"+itoa(idx), "tn"+itoa(idx), "tt"+itoa(idx), "td"+itoa(idx), "tf"+itoa(idx), "ta"+itoa(idx)
	a.MovRegMemIdx(RAX, cells, RCX)
	a.MovRegMem(R10, RDI, int8(POffHeap+in.B*8))
	a.MovRegReg32(R11, RAX)
	a.AddRegReg(R10, R11)
	a.MovRegReg(RDX, RAX)
	a.ShrRegImm8(RDX, 32)
	a.XorRegReg(R11, R11)
	a.Label(lp)
	a.CmpRegImm32(RDX, 16)
	a.Jcc(CondL, tail)
	a.MovdquLoad(X0, R10)
	a.Pmovmskb(RAX, X0)
	a.TestRegReg(RAX, RAX)
	a.Jcc(CondNE, fb)
	a.Pxor(X1, X1)
	a.Pcmpeqb(X1, X0)
	a.Pmovmskb(RAX, X1)
	a.TestRegReg(RAX, RAX)
	a.Jcc(CondNE, nul)
	a.AddRegImm8(R11, 16)
	a.AddRegImm8(R10, 16)
	a.AddRegImm8(RDX, -16)
	a.Jmp(lp)
	a.Label(nul)
	a.BsfRegReg(RAX, RAX)
	a.AddRegReg(R11, RAX)
	a.Jmp(done)
	a.Label(tail)
	a.TestRegReg(RDX, RDX)
	a.Jcc(CondE, done)
	a.MovzxByte(RAX, R10)
	a.TestRegImm8(RAX, 0x80)
	a.Jcc(CondNE, fb)
	a.TestRegReg(RAX, RAX)
	a.Jcc(CondE, done)
	a.IncReg(R11)
	a.IncReg(R10)
	a.DecReg(RDX)
	a.Jmp(tail)
	a.Label(done)
	a.MovMemReg32(RSI, in.A, R11)
	a.Jmp(after)
	a.Label(fb)
	a.Jmp(fallback)
	a.Label(after)
}

// emitTextPool lays out the constants POpTextMatch loads RIP-relative: each
// pattern padded to 16 bytes, and the three vectors the ASCII fold uses.
func emitTextPool(a *Asm, insns []ProgInsn) {
	fold := false
	for idx, in := range insns {
		if in.Op != POpTextMatch {
			continue
		}
		fold = fold || in.Fold
		a.Align(16)
		a.Label("pat" + itoa(idx))
		var p [16]byte
		copy(p[:], in.Pat)
		a.emit(p[:]...)
	}
	if fold {
		for _, k := range []struct {
			l string
			b byte
		}{{"kA", 'A'}, {"k26", 26}, {"k20", 0x20}} {
			a.Align(16)
			a.Label(k.l)
			for range 16 {
				a.emit(k.b)
			}
		}
	}
}

// emitTextFoldX0 lower-cases the ASCII letters in X0: a byte b is one when
// b-'A' is 0..25 as a signed byte, which two PCMPGTB decide without an
// unsigned compare (SSE2 has none).
func emitTextFoldX0(a *Asm) {
	a.Movdqa(X2, X0)
	a.MovdquRIP(X3, "kA")
	a.Psubb(X2, X3)  // v = b - 'A'
	a.MovdquRIP(X3, "k26")
	a.Pcmpgtb(X3, X2) // 26 > v
	a.Pxor(X1, X1)
	a.Pcmpgtb(X1, X2) // 0 > v
	a.Pandn(X1, X3)   // 0 <= v < 26
	a.MovdquRIP(X3, "k20")
	a.Pand(X1, X3)
	a.Por(X0, X1)
}

// emitTextMatchAmd64 tests the cell in slot in.B against in.Pat. RAX, RDX,
// R10 and R11 are scratch. The pattern is at most 16 bytes, so one comparison at a
// position is one 16-byte load when 16 bytes remain in the heap, and an
// unrolled run of byte compares against immediates otherwise.
func emitTextMatchAmd64(a *Asm, in ProgInsn, cells Reg, idx int) {
	m := len(in.Pat)
	sfx := itoa(idx)
	yes, no, end := "my"+sfx, "mn"+sfx, "me"+sfx
	// R10 = the text, RDX = its length, R11 = the heap's end.
	a.MovRegMemIdx(RAX, cells, RCX)
	a.MovRegMem32(R10, RDI, (POffHeap+in.B*8)/8)
	a.MovRegReg(R11, R10)
	a.MovRegMem32(RDX, RDI, (POffHeapLen+in.B*8)/8)
	a.AddRegReg(R11, RDX)
	a.MovRegReg32(RDX, RAX)
	a.AddRegReg(R10, RDX)
	a.MovRegReg(RDX, RAX)
	a.ShrRegImm8(RDX, 32)

	// matchAt jumps to fail unless Pat is at [R10]; R10 has >= m bytes of
	// text. Clobbers RAX.
	matchAt := func(fail string, k int) {
		scalar, done := "ms"+sfx+"_"+itoa(k), "md"+sfx+"_"+itoa(k)
		a.LeaDisp(RAX, R10, 16)
		a.CmpRegReg(RAX, R11)
		a.Jcc(condAbove, scalar)
		a.MovdquLoad(X0, R10)
		if in.Fold {
			emitTextFoldX0(a)
		}
		a.MovdquRIP(X1, "pat"+itoa(idx))
		a.Pcmpeqb(X0, X1)
		a.Pmovmskb(RAX, X0)
		mask := int32(1)<<m - 1
		a.AndRegImm32(RAX, mask)
		a.CmpRegImm32(RAX, mask)
		a.Jcc(CondNE, fail)
		a.Jmp(done)
		a.Label(scalar)
		for j, p := range in.Pat {
			a.MovzxByteDisp(RAX, R10, int8(j))
			if in.Fold && p >= 'a' && p <= 'z' {
				ok := "mk" + sfx + "_" + itoa(k) + "_" + itoa(j)
				a.CmpALImm8(p)
				a.Jcc(CondE, ok)
				a.CmpALImm8(p - 0x20)
				a.Jcc(CondNE, fail)
				a.Label(ok)
				continue
			}
			a.CmpALImm8(p)
			a.Jcc(CondNE, fail)
		}
		a.Label(done)
	}

	a.CmpRegImm32(RDX, int32(m))
	a.Jcc(CondL, no)
	switch in.Mode {
	case TextEq:
		// The text ends at m, or holds a NUL there (it is a C string to LIKE).
		eqOK := "mq" + sfx
		a.Jcc(CondE, eqOK)
		a.MovzxByteDisp(RAX, R10, int8(m))
		a.TestRegReg(RAX, RAX)
		a.Jcc(CondNE, no)
		a.Label(eqOK)
		matchAt(no, 0)
		a.Jmp(yes)
	case TextPrefix:
		matchAt(no, 0)
		a.Jmp(yes)
	case TextSuffix:
		// The text ends at its first NUL: find it, then the last m bytes
		// before it must be the pattern.
		emitTextEffLenAmd64(a, in.B, sfx)
		a.CmpRegImm32(RDX, int32(m))
		a.Jcc(CondL, no)
		a.AddRegReg(R10, RDX)
		a.AddRegImm8(R10, int8(-m))
		matchAt(no, 0)
		a.Jmp(yes)
	case TextContains:
		// Every start position whose m bytes are inside the text, stopping at
		// the first NUL: a match cannot span it, the pattern holding none.
		loop, next := "mc"+sfx, "mx"+sfx
		a.Label(loop)
		a.CmpRegImm32(RDX, int32(m))
		a.Jcc(CondL, no)
		a.MovzxByteDisp(RAX, R10, 0)
		a.TestRegReg(RAX, RAX)
		a.Jcc(CondE, no)
		// A first-byte test turns most positions away before the full one.
		p0 := in.Pat[0]
		if in.Fold && p0 >= 'a' && p0 <= 'z' {
			first := "mf" + sfx
			a.CmpALImm8(p0)
			a.Jcc(CondE, first)
			a.CmpALImm8(p0 - 0x20)
			a.Jcc(CondNE, next)
			a.Label(first)
		} else {
			a.CmpALImm8(p0)
			a.Jcc(CondNE, next)
		}
		matchAt(next, 1)
		a.Jmp(yes)
		a.Label(next)
		a.IncReg(R10)
		a.DecReg(RDX)
		a.Jmp(loop)
	}
	a.Label(yes)
	a.MovRegImm64(RAX, 1)
	a.MovMemReg32(RSI, in.A, RAX)
	a.Jmp(end)
	a.Label(no)
	a.XorRegReg(RAX, RAX)
	a.MovMemReg32(RSI, in.A, RAX)
	a.Label(end)
}

// emitTextEffLenAmd64 sets RDX to the text's length before its first NUL,
// given R10 = the text and RDX = its stored length. The scan reads 16 bytes at
// a time only while they lie inside the text (so it needs no heap bound), then
// a byte at a time. It uses RAX and R11 as scratch and X0, X1, then sets R11
// back to the heap's end, which the comparison after it relies on.
func emitTextEffLenAmd64(a *Asm, slot int, sfx string) {
	lp, tail, nul, zero, done := "el"+sfx, "et"+sfx, "en"+sfx, "ez"+sfx, "ed"+sfx
	a.MovRegReg(RAX, R10) // the scan pointer
	a.AddRegReg(RDX, R10) // RDX = the text's end
	a.Label(lp)
	a.LeaDisp(R11, RAX, 16)
	a.CmpRegReg(R11, RDX)
	a.Jcc(condAbove, tail)
	a.MovdquLoad(X0, RAX)
	a.Pxor(X1, X1)
	a.Pcmpeqb(X1, X0)
	a.Pmovmskb(R11, X1)
	a.TestRegReg(R11, R11)
	a.Jcc(CondNE, nul)
	a.AddRegImm8(RAX, 16)
	a.Jmp(lp)
	a.Label(nul)
	a.BsfRegReg(R11, R11)
	a.AddRegReg(RAX, R11)
	a.MovRegReg(RDX, RAX)
	a.Jmp(done)
	a.Label(tail)
	a.CmpRegReg(RAX, RDX)
	a.Jcc(CondE, done)
	a.MovzxByte(R11, RAX)
	a.TestRegReg(R11, R11)
	a.Jcc(CondE, zero)
	a.IncReg(RAX)
	a.Jmp(tail)
	a.Label(zero)
	a.MovRegReg(RDX, RAX)
	a.Label(done)
	a.SubRegReg(RDX, R10)
	a.MovRegMem32(R11, RDI, (POffHeap+slot*8)/8)
	a.MovRegMem32(RAX, RDI, (POffHeapLen+slot*8)/8)
	a.AddRegReg(R11, RAX)
}
