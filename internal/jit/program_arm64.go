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
		if (in.Op == POpLoadCol || in.Op == POpTextLen || in.Op == POpTextMatch) && (in.B < 0 || in.B >= nCols) {
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
	if progHasService(insns) {
		// Re-entry after a POpService: see emitProgResume (program_amd64.go).
		a.LdrImm(X9, X0, POffPC)
		a.Cbz(X9, "loop")
		a.LdrImm(X2, X0, POffRow)
		a.LdrImm(X10, X0, POffOut)
		a.LdrImm(X3, X10, 0)
		a.LdrImm(X10, X0, POffOverflow)
		a.LdrImm(X4, X10, 0)
		for _, in := range insns {
			if in.Op == POpService {
				a.CmpImm(X9, uint32(in.A)+1)
				a.Bcond(CondE, svcLabel(in.A))
			}
		}
		a.B("done")
	}

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
		case POpAbs:
			a.LdrImm(X9, X1, in.B*8)
			a.MovImm64(X11, math.MinInt64)
			a.Cmp(X9, X11)
			a.Cset(X11, CondE)
			a.OrrReg(X4, X4, X11)
			a.NegReg(X10, X9)
			a.Cmp(X9, xzr)
			a.Csel(X9, X10, X9, CondL)
			a.StrImm(X9, X1, in.A*8)
		case POpTextLen:
			emitTextLenArm64(a, in, colReg[in.B], progTarget(in.C, len(insns)), idx)
		case POpTextMatch:
			if len(in.Pat) == 0 || len(in.Pat) > 16 {
				return nil, fmt.Errorf("jit: text pattern of %d bytes", len(in.Pat))
			}
			emitTextMatchArm64(a, in, colReg[in.B], idx)
		case POpService:
			a.MovImm64(X9, int64(in.A)+1)
			a.StrImm(X9, X0, POffPC)
			a.StrImm(X2, X0, POffRow)
			a.B("fin")
			a.Label(svcLabel(in.A))
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
	a.MovImm16(X9, 0)
	a.StrImm(X9, X0, POffPC)
	a.Label("fin")
	a.LdrImm(X9, X0, POffOut)
	a.StrImm(X3, X9, 0)
	a.LdrImm(X9, X0, POffOverflow)
	a.StrImm(X4, X9, 0)
	a.Ret()
	emitTextPoolArm64(a, insns)
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

// emitTextLenArm64 is emitTextLenAmd64's algorithm with NEON: UMAXV over the
// 16 bytes is the non-ASCII test (a maximum >= 0x80), and CMEQ #0 then UMAXV
// says whether the chunk holds the NUL, whose position the byte loop finds.
// X9..X13 are scratch.
func emitTextLenArm64(a *Arm, in ProgInsn, cells Reg64, fallback string, idx int) {
	lp, chunk, tail, done, after := "tl"+itoa(idx), "tc"+itoa(idx), "tt"+itoa(idx), "td"+itoa(idx), "ta"+itoa(idx)
	a.LdrRegIdx(X9, cells, X2)
	a.LdrImm(X10, X0, POffHeap+in.B*8)
	a.MovW(X13, X9)
	a.AddReg(X10, X10, X13)
	a.LsrImm(X11, X9, 32)
	a.MovImm16(X12, 0)
	a.Label(lp)
	a.CmpImm(X11, 16)
	a.Bcond(CondL, tail)
	a.LdrQ(VReg(0), X10)
	a.Umaxv16B(VReg(1), VReg(0))
	a.UmovB0(X13, VReg(1))
	a.CmpImm(X13, 0x80)
	a.Bcond(CondGE, fallback)
	a.Cmeq0_16B(VReg(1), VReg(0))
	a.Umaxv16B(VReg(1), VReg(1))
	a.UmovB0(X13, VReg(1))
	a.Cbnz(X13, chunk)
	a.AddImm(X12, X12, 16)
	a.AddImm(X10, X10, 16)
	a.SubsImm(X11, X11, 16)
	a.B(lp)
	a.Label(chunk)
	a.MovImm16(X11, 16) // the NUL is in these 16 bytes: let the byte loop find it
	a.Label(tail)
	a.Cbz(X11, done)
	a.LdrbImm(X13, X10, 0)
	a.CmpImm(X13, 0x80)
	a.Bcond(CondGE, fallback)
	a.Cbz(X13, done)
	a.AddImm(X12, X12, 1)
	a.AddImm(X10, X10, 1)
	a.SubsImm(X11, X11, 1)
	a.B(tail)
	a.Label(done)
	a.StrImm(X12, X1, in.A*8)
	a.Label(after)
}

// emitTextPoolArm64 lays out POpTextMatch's constants after the code: each
// pattern and its lane mask (m leading 0xFF bytes), and the fold's vectors.
func emitTextPoolArm64(a *Arm, insns []ProgInsn) {
	fold := false
	for idx, in := range insns {
		if in.Op != POpTextMatch {
			continue
		}
		fold = fold || in.Fold
		var p, mask [16]byte
		copy(p[:], in.Pat)
		for j := range in.Pat {
			mask[j] = 0xFF
		}
		a.Label("pat" + itoa(idx))
		a.Data(p[:])
		a.Label("msk" + itoa(idx))
		a.Data(mask[:])
	}
	if fold {
		for _, k := range []struct {
			l string
			b byte
		}{{"kA", 'A'}, {"k26", 26}, {"k20", 0x20}} {
			a.Label(k.l)
			var v [16]byte
			for j := range v {
				v[j] = k.b
			}
			a.Data(v[:])
		}
	}
}

// emitTextMatchArm64 is emitTextMatchAmd64 with NEON. The fold needs no
// signed trick: CMHI is an unsigned compare, so a letter is 26 >u b-'A'. A
// position matches when (text ^ pattern) & mask is all zero, which UMAXV
// tests. X9..X14 are scratch; V0..V4.
func emitTextMatchArm64(a *Arm, in ProgInsn, cells Reg64, idx int) {
	m := len(in.Pat)
	sfx := itoa(idx)
	yes, no, end := "my"+sfx, "mn"+sfx, "me"+sfx
	v := func(n int) VReg { return VReg(n) }
	// X10 = the text, X11 = its length, X12 = the heap's end.
	a.LdrRegIdx(X9, cells, X2)
	a.LdrImm(X10, X0, POffHeap+in.B*8)
	a.LdrImm(X12, X0, POffHeapLen+in.B*8)
	a.AddReg(X12, X12, X10)
	a.MovW(X13, X9)
	a.AddReg(X10, X10, X13)
	a.LsrImm(X11, X9, 32)

	matchAt := func(fail string, k int) {
		scalar, done := "ms"+sfx+"_"+itoa(k), "md"+sfx+"_"+itoa(k)
		a.AddImm(X13, X10, 16)
		a.Cmp(X13, X12)
		a.BcondRaw(0x8, scalar) // HI: unsigned >, past the heap's end
		a.LdrQ(v(0), X10)
		if in.Fold {
			a.Adr(X14, "kA")
			a.LdrQ(v(2), X14)
			a.SubV16B(v(2), v(0), v(2)) // b - 'A'
			a.Adr(X14, "k26")
			a.LdrQ(v(3), X14)
			a.Cmhi16B(v(3), v(3), v(2)) // 26 >u b-'A'
			a.Adr(X14, "k20")
			a.LdrQ(v(4), X14)
			a.AndV(v(3), v(3), v(4))
			a.OrrV(v(0), v(0), v(3))
		}
		a.Adr(X14, "pat"+itoa(idx))
		a.LdrQ(v(1), X14)
		a.EorV(v(0), v(0), v(1))
		a.Adr(X14, "msk"+itoa(idx))
		a.LdrQ(v(1), X14)
		a.AndV(v(0), v(0), v(1))
		a.Umaxv16B(v(1), v(0))
		a.UmovB0(X13, v(1))
		a.Cbnz(X13, fail)
		a.B(done)
		a.Label(scalar)
		for j, p := range in.Pat {
			a.LdrbImm(X13, X10, j)
			if in.Fold && p >= 'a' && p <= 'z' {
				ok := "mk" + sfx + "_" + itoa(k) + "_" + itoa(j)
				a.CmpImm(X13, uint32(p))
				a.Bcond(CondE, ok)
				a.CmpImm(X13, uint32(p-0x20))
				a.Bcond(CondNE, fail)
				a.Label(ok)
				continue
			}
			a.CmpImm(X13, uint32(p))
			a.Bcond(CondNE, fail)
		}
		a.Label(done)
	}

	a.CmpImm(X11, uint32(m))
	a.Bcond(CondL, no)
	switch in.Mode {
	case TextEq:
		eqOK := "mq" + sfx
		a.Bcond(CondE, eqOK)
		a.LdrbImm(X13, X10, m)
		a.Cbnz(X13, no)
		a.Label(eqOK)
		matchAt(no, 0)
		a.B(yes)
	case TextPrefix:
		matchAt(no, 0)
		a.B(yes)
	case TextContains:
		loop, next := "mc"+sfx, "mx"+sfx
		a.Label(loop)
		a.CmpImm(X11, uint32(m))
		a.Bcond(CondL, no)
		a.LdrbImm(X13, X10, 0)
		a.Cbz(X13, no)
		p0 := in.Pat[0]
		if in.Fold && p0 >= 'a' && p0 <= 'z' {
			first := "mf" + sfx
			a.CmpImm(X13, uint32(p0))
			a.Bcond(CondE, first)
			a.CmpImm(X13, uint32(p0-0x20))
			a.Bcond(CondNE, next)
			a.Label(first)
		} else {
			a.CmpImm(X13, uint32(p0))
			a.Bcond(CondNE, next)
		}
		matchAt(next, 1)
		a.B(yes)
		a.Label(next)
		a.AddImm(X10, X10, 1)
		a.SubsImm(X11, X11, 1)
		a.B(loop)
	}
	a.Label(yes)
	a.MovImm16(X13, 1)
	a.StrImm(X13, X1, in.A*8)
	a.B(end)
	a.Label(no)
	a.StrImm(xzr, X1, in.A*8)
	a.Label(end)
}
