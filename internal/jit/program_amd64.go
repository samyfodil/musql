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
		if (in.Op == POpLoadCol || in.Op == POpTextLen || in.Op == POpTextMatch) && (in.B < 0 || in.B >= nCols) {
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
	emitTextPool(a, insns)
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

// emitTextLenAmd64 counts the TEXT cell's characters before its first NUL, 16
// bytes at a time while 16 remain in the cell (so it never reads past it),
// then a byte at a time. PMOVMSKB of the bytes is their high bits, the
// non-ASCII test; against zero it finds the NUL. RAX, RDX, R10 and R11 are
// scratch; R11 is re-zeroed after, because POpAccSum's SETO relies on its
// upper bytes being zero.
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
	a.XorRegReg(R11, R11)
	a.Jmp(after)
	a.Label(fb)
	a.XorRegReg(R11, R11)
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
// R10 and R11 are scratch (R11 re-zeroed after: POpAccSum's SETO relies on
// its upper bytes). The pattern is at most 16 bytes, so one comparison at a
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
	a.XorRegReg(R11, R11)
}
