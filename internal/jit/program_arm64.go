//go:build arm64

package jit

import (
	"fmt"
	"math"
)

// EmitProgram compiles an instruction list into a native row loop (arm64 version).
// Separate from program_amd64.go because the architectures differ at instruction level.
func EmitProgram(insns []ProgInsn, nCols int) ([]byte, error) {
	if nCols > MaxProgCols {
		return nil, fmt.Errorf("jit: %d column blocks, max %d", nCols, MaxProgCols)
	}
	for _, in := range insns {
		if in.Op == POpLoadCol && (in.B < 0 || in.B >= nCols) {
			return nil, fmt.Errorf("jit: column %d out of range", in.B)
		}
	}
	colReg := [MaxProgCols]Reg64{X5, X6, X7, X8}

	a := NewArm()
	a.LdrImm(X1, X0, POffRegs)
	a.LdrImm(X2, X0, POffN)
	a.MovImm16(X3, 0)
	a.MovImm16(X4, 0)
	// Min/max programs seed the accumulator to avoid a first-row check.
	for _, in := range insns {
		if in.Op == POpAccMin {
			a.MovImm64(X3, math.MaxInt64)
		} else if in.Op == POpAccMax {
			a.MovImm64(X3, math.MinInt64)
		}
	}
	for c := 0; c < nCols; c++ {
		a.LdrImm(colReg[c], X0, POffCol+c*8)
	}
	a.Cbz(X2, "done")
	// The index counts up from -n to 0, so one ADD advances every column read
	// and tests for the end at once.
	for c := 0; c < nCols; c++ {
		a.AddRegLSL3(colReg[c], colReg[c], X2)
	}
	a.NegReg(X2, X2)

	a.Label("loop")
	for idx, in := range insns {
		a.Label(progLabel(idx))
		switch in.Op {
		case POpLoadCol:
			a.LdrRegIdx(X9, colReg[in.B], X2)
			a.StrImm(X9, X1, in.A*8)
		case POpLoadReg:
			a.LdrImm(X9, X1, in.B*8)
			a.StrImm(X9, X1, in.A*8)
		case POpCmp:
			a.LdrImm(X9, X1, in.B*8)
			a.LdrImm(X10, X1, in.C*8)
			a.Cmp(X9, X10)
			a.Cset(X11, in.Cond)
			a.StrImm(X11, X1, in.A*8)
		case POpAnd, POpOr:
			a.LdrImm(X9, X1, in.B*8)
			a.LdrImm(X10, X1, in.C*8)
			if in.Op == POpAnd {
				a.AndReg(X9, X9, X10)
			} else {
				a.OrrReg(X9, X9, X10)
			}
			a.StrImm(X9, X1, in.A*8)
		case POpSkipIfZero:
			a.LdrImm(X9, X1, in.A*8)
			a.Cbz(X9, "next")
		case POpSetConst:
			a.MovImm64(X9, int64(in.B))
			a.StrImm(X9, X1, in.A*8)
		case POpJump:
			a.B(progTarget(in.A, len(insns)))
		case POpJumpIfZero:
			a.LdrImm(X9, X1, in.B*8)
			a.Cbz(X9, progTarget(in.A, len(insns)))
		case POpJumpIfNotZero:
			a.LdrImm(X9, X1, in.B*8)
			a.Cbnz(X9, progTarget(in.A, len(insns)))
		case POpNot:
			a.LdrImm(X9, X1, in.B*8)
			a.Cmp(X9, xzr)
			a.Cset(X10, CondE)
			a.StrImm(X10, X1, in.A*8)
		case POpRem:
			// SDIV does not trap, and MSUB then gives C's truncating remainder,
			// 0 for a divisor of -1 included; only a zero divisor (SQL NULL) is
			// flagged, for the caller to decline.
			nz, st := "rn"+itoa(idx), "rs"+itoa(idx)
			a.LdrImm(X9, X1, in.B*8)
			a.LdrImm(X10, X1, in.C*8)
			a.Cbnz(X10, nz)
			a.MovImm16(X11, 1)
			a.OrrReg(X4, X4, X11)
			a.B(st)
			a.Label(nz)
			a.Sdiv(X11, X9, X10)
			a.Msub(X9, X11, X10, X9)
			a.Label(st)
			a.StrImm(X9, X1, in.A*8)
		case POpAdd, POpSub, POpMul:
			a.LdrImm(X9, X1, in.B*8)
			a.LdrImm(X10, X1, in.C*8)
			switch in.Op {
			case POpAdd:
				a.AddsReg(X9, X9, X10)
			case POpSub:
				a.SubsReg(X9, X9, X10)
			default:
				// AArch64 has no flag-setting multiply. SMULH gives the high 64
				// bits; the product overflowed exactly when they are not the
				// sign extension of the low half, which is what this compares.
				a.Smulh(X11, X9, X10)
				a.Mul(X9, X9, X10)
				a.CmpAsr63(X11, X9)
				a.Cset(X10, CondNE)
				a.OrrReg(X4, X4, X10)
				a.StrImm(X9, X1, in.A*8)
				continue
			}
			a.CsetOverflow(X10)
			a.OrrReg(X4, X4, X10)
			a.StrImm(X9, X1, in.A*8)
		case POpAccCount:
			a.AddImm(X3, X3, 1)
		case POpEmitRow:
			// Sel[acc] = X2 (the loop index); acc++.
			a.LdrImm(X9, X0, POffSel)
			a.AddRegLSL3(X9, X9, X3)
			a.StrImm(X2, X9, 0)
			a.AddImm(X3, X3, 1)
		case POpAccMin, POpAccMax:
			a.LdrImm(X9, X1, in.A*8)
			a.Cmp(X3, X9)
			cond := CondG // min: acc > v, so take v
			if in.Op == POpAccMax {
				cond = CondL
			}
			a.Csel(X3, X9, X3, cond)
			a.LdrImm(X10, X1, in.B*8)
			a.AddImm(X10, X10, 1)
			a.StrImm(X10, X1, in.B*8)
		case POpAccSum:
			a.LdrImm(X9, X1, in.A*8)
			a.AddsReg(X3, X3, X9)
			// CSET reads the flags ADDS just set; OR-ing into X4 keeps the
			// answer sticky for the whole loop without a branch.
			a.CsetOverflow(X10)
			a.OrrReg(X4, X4, X10)
			a.LdrImm(X9, X1, in.B*8)
			a.AddImm(X9, X9, 1)
			a.StrImm(X9, X1, in.B*8)
		default:
			return nil, fmt.Errorf("jit: unknown program opcode %d", in.Op)
		}
	}
	a.Label("next")
	a.AddImm(X2, X2, 1)
	a.Cbnz(X2, "loop")

	a.Label("done")
	a.LdrImm(X9, X0, POffOut)
	a.StrImm(X3, X9, 0)
	a.LdrImm(X9, X0, POffOverflow)
	a.StrImm(X4, X9, 0)
	a.Ret()
	return a.Code()
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
