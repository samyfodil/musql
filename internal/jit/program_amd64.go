//go:build amd64

package jit

import (
	"fmt"
	"math"
)

// EmitProgram compiles an instruction list into a native row loop. The loop
// is emitted once per program; the body is whatever the instruction list says.
func EmitProgram(insns []ProgInsn, nCols int) ([]byte, error) {
	if nCols > MaxProgCols {
		return nil, fmt.Errorf("jit: %d column blocks, max %d", nCols, MaxProgCols)
	}
	for _, in := range insns {
		if in.Op == POpLoadCol && (in.B < 0 || in.B >= nCols) {
			return nil, fmt.Errorf("jit: column %d out of range", in.B)
		}
	}
	colReg := [MaxProgCols]Reg{RBX, R12, R13, R15}

	a := NewAsm()
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
	a.XorRegReg(R9, R9)
	// R11 is the SETO landing pad for POpAccSum. SETcc writes only the LOW
	// BYTE, so the other seven have to start at zero or the sticky OR into R9
	// reports overflow that never happened -- it read 43497009332224 for a sum
	// that did not overflow at all.
	a.XorRegReg(R11, R11)
	for c := 0; c < nCols; c++ {
		a.MovRegMem(colReg[c], RDI, int8(POffCol+c*8))
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

	a.Label("loop")
	// One label per IR instruction, so a lowered OpIf/OpIfNot/OpGoto becomes an
	// ordinary branch. Without these the IR could only express a straight line
	// of predicates -- which is exactly the limit the fixed kernels had.
	for idx, in := range insns {
		a.Label(progLabel(idx))
		switch in.Op {
		case POpLoadCol:
			a.MovRegMemIdx(RAX, colReg[in.B], RCX)
			a.MovMemReg32(RSI, in.A, RAX)
		case POpLoadReg:
			a.MovRegMem32(RAX, RSI, in.B)
			a.MovMemReg32(RSI, in.A, RAX)
		case POpCmp:
			a.MovRegMem32(RAX, RSI, in.B)
			a.MovRegMem32(RDX, RSI, in.C)
			// R10 is zeroed BEFORE the compare. XOR sets the flags, so zeroing
			// it afterwards destroyed the comparison SETcc was about to read --
			// every predicate came back constant, which the differential caught
			// as "program 0, Go 1339" and "program 5000, Go 1588": all rows
			// rejected under AND, all accepted under OR.
			a.XorRegReg(R10, R10)
			a.CmpRegReg(RAX, RDX)
			a.Setcc(in.Cond, R10)
			a.MovMemReg32(RSI, in.A, R10)
		case POpAnd, POpOr:
			a.MovRegMem32(RAX, RSI, in.B)
			a.MovRegMem32(RDX, RSI, in.C)
			if in.Op == POpAnd {
				a.AndRegReg(RAX, RDX)
			} else {
				a.OrRegReg(RAX, RDX)
			}
			a.MovMemReg32(RSI, in.A, RAX)
		case POpSkipIfZero:
			a.MovRegMem32(RAX, RSI, in.A)
			a.TestRegReg(RAX, RAX)
			a.Jcc(CondE, "next")
		case POpSetConst:
			a.MovRegImm64(RAX, int64(in.B))
			a.MovMemReg32(RSI, in.A, RAX)
		case POpJump:
			a.Jmp(progTarget(in.A, len(insns)))
		case POpJumpIfZero, POpJumpIfNotZero:
			a.MovRegMem32(RAX, RSI, in.B)
			a.TestRegReg(RAX, RAX)
			cond := CondE
			if in.Op == POpJumpIfNotZero {
				cond = CondNE
			}
			a.Jcc(cond, progTarget(in.A, len(insns)))
		case POpNot:
			a.MovRegMem32(RAX, RSI, in.B)
			a.XorRegReg(R10, R10)
			a.TestRegReg(RAX, RAX)
			a.Setcc(CondE, R10)
			a.MovMemReg32(RSI, in.A, R10)
		case POpAdd, POpSub, POpMul:
			a.MovRegMem32(RAX, RSI, in.B)
			a.MovRegMem32(RDX, RSI, in.C)
			switch in.Op {
			case POpAdd:
				a.AddRegReg(RAX, RDX)
			case POpSub:
				a.SubRegReg(RAX, RDX)
			default:
				a.ImulRegReg(RAX, RDX)
			}
			a.Setcc(condOverflow, R11)
			a.OrRegReg(R9, R11)
			a.MovMemReg32(RSI, in.A, RAX)
		case POpRem:
			// RCX is the loop index and R9 the overflow flag, so the divisor
			// goes in R10; idiv clobbers RDX, which is scratch here.
			z, m1, st := "rz"+itoa(idx), "rm"+itoa(idx), "rs"+itoa(idx)
			a.MovRegMem32(RAX, RSI, in.B)
			a.MovRegMem32(R10, RSI, in.C)
			a.TestRegReg(R10, R10)
			a.Jcc(CondE, z)
			a.CmpRegImm32(R10, -1)
			a.Jcc(CondE, m1)
			a.Cqo()
			a.IdivReg(R10)
			a.MovRegReg(RAX, RDX)
			a.Jmp(st)
			a.Label(z)
			a.MovRegImm64(R11, 1)
			a.OrRegReg(R9, R11)
			a.Label(m1)
			a.XorRegReg(RAX, RAX)
			a.Label(st)
			a.MovMemReg32(RSI, in.A, RAX)
		case POpAbs:
			// NEG sets OF exactly for MinInt64, and SF when the original was
			// positive, in which case the original is the answer.
			a.MovRegMem32(RAX, RSI, in.B)
			a.MovRegReg(RDX, RAX)
			a.NegReg(RDX)
			a.Setcc(condOverflow, R11)   // SETcc and CMOVcc leave the flags alone;
			a.Cmovcc(condSign, RDX, RAX) // the OR below does not, so it goes last
			a.OrRegReg(R9, R11)
			a.MovMemReg32(RSI, in.A, RDX)
		case POpService:
			a.MovRegImm64(RAX, int64(in.A)+1)
			a.MovMemReg(RDI, POffPC, RAX)
			a.MovMemReg(RDI, POffRow, RCX)
			a.Jmp("fin")
			a.Label(svcLabel(in.A))
		case POpAccCount:
			a.IncReg(R8)
		case POpEmitRow:
			// Sel[acc] = the loop index; acc++. RAX is scratch, and RCX holds
			// the index the loop is already counting on.
			a.MovRegMem(RAX, RDI, POffSel)
			a.MovMemIdxReg(RAX, R8, RCX)
			a.IncReg(R8)
		case POpAccMin, POpAccMax:
			a.MovRegMem32(RAX, RSI, in.A)
			a.CmpRegReg(R8, RAX)
			cond := CondG // min: acc > v, so take v
			if in.Op == POpAccMax {
				cond = CondL
			}
			a.Cmovcc(cond, R8, RAX)
			a.MovRegMem32(RDX, RSI, in.B)
			a.IncReg(RDX)
			a.MovMemReg32(RSI, in.B, RDX)
		case POpAccSum:
			a.MovRegMem32(RAX, RSI, in.A)
			a.AddRegRegOverflow(R8, RAX, R9)
			a.MovRegMem32(RDX, RSI, in.B)
			a.IncReg(RDX)
			a.MovMemReg32(RSI, in.B, RDX)
		default:
			return nil, fmt.Errorf("jit: unknown program opcode %d", in.Op)
		}
	}
	a.Label("next")
	a.IncReg(RCX)
	a.Jcc(CondNE, "loop")

	a.Label("done")
	a.XorRegReg(RAX, RAX)
	a.MovMemReg(RDI, POffPC, RAX)
	a.Label("fin")
	a.MovRegMem(RDX, RDI, POffOut)
	a.MovMemReg(RDX, 0, R8)
	a.MovRegMem(RDX, RDI, POffOverflow)
	a.MovMemReg(RDX, 0, R9)
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
		if in.Op == POpService {
			a.CmpRegImm32(RAX, int32(in.A)+1)
			a.Jcc(CondE, svcLabel(in.A))
		}
	}
	a.Jmp("done") // no such service: finish rather than guess
}
