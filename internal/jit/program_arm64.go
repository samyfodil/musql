//go:build arm64

package jit

import (
	"fmt"
	"math"
)

// EmitProgram compiles an instruction list into a native row loop (arm64 version).
// Separate from program_amd64.go because the architectures differ at instruction level.
func emitProgram(insns []ProgInsn, nCols int) ([]byte, error) {
	if nCols > MaxProgCols {
		return nil, fmt.Errorf("jit: %d column blocks, max %d", nCols, MaxProgCols)
	}
	for _, in := range insns {
		if (in.Op == POpLoadCol || in.Op == POpTextLen || in.Op == POpTextMatch || in.BCol) && (in.B < 0 || in.B >= nCols) {
			return nil, fmt.Errorf("jit: column %d out of range", in.B)
		}
	}
	colReg := [MaxProgCols]Reg64{X5, X6, X7, X8}

	a := NewArm()
	// IR registers live in the column registers this program does not read,
	// X15-X17 and X19-X25 (allocProgRegs); X9-X14 are scratch, the text
	// scans' included. The callee-saved X19-X25 bind a C caller only; under
	// the ABI0 trampoline every one of them is the kernel's
	// (trampoline_arm64.s). X26 (Go's closure context) is left alone, as are
	// X18, X27 and X28.
	pool := append(append([]Reg64(nil), colReg[nCols:]...), X15, X16, X17, X19, X20, X21, X22, X23, X24, X25)
	pa := allocProgRegs(insns, len(pool))
	phys := func(r int) (Reg64, bool) {
		if k := pa.at(r); k >= 0 {
			return pool[k], true
		}
		return 0, false
	}
	// val is a register holding r's value: its own, or scratch loaded.
	val := func(r int, scratch Reg64) Reg64 {
		if p, ok := phys(r); ok {
			return p
		}
		a.LdrImm(scratch, X1, r*8)
		return scratch
	}
	// into puts r's value in dst, which the caller then changes.
	into := func(dst Reg64, r int) {
		if p, ok := phys(r); ok {
			a.MovReg(dst, p)
			return
		}
		a.LdrImm(dst, X1, r*8)
	}
	// dst is where to compute r: its own register, or scratch; fin then
	// stores scratch to the file.
	dst := func(r int, scratch Reg64) Reg64 {
		if p, ok := phys(r); ok {
			return p
		}
		return scratch
	}
	fin := func(r int, d Reg64) {
		if _, ok := phys(r); !ok {
			a.StrImm(d, X1, r*8)
		}
	}
	set := func(r int, src Reg64) {
		if p, ok := phys(r); ok {
			if p != src {
				a.MovReg(p, src)
			}
			return
		}
		a.StrImm(src, X1, r*8)
	}
	inc := func(r int) {
		if p, ok := phys(r); ok {
			a.AddImm(p, p, 1)
			return
		}
		a.LdrImm(X9, X1, r*8)
		a.AddImm(X9, X9, 1)
		a.StrImm(X9, X1, r*8)
	}
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
	for _, r := range pa.load {
		p, _ := phys(r)
		a.LdrImm(p, X1, r*8)
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

	padArm(a, loopAlign)
	a.Label("loop")
	ps, pred := progPredicated(insns)
	var prev Cond
	for idx, in := range insns {
		a.Label(progLabel(idx))
		if pred && idx >= ps {
			// The predicated row (progPredicated): the filters' conjunction
			// in the flags through CCMP, then 1 or 0 in X12 and its mask in
			// X13, which the accumulators apply.
			switch in.Op {
			case POpCmpSkip:
				x := X9
				if in.BCol {
					a.LdrRegIdx(X9, colReg[in.B], X2)
				} else {
					x = val(in.B, X9)
				}
				y := val(in.C, X10)
				if idx == ps {
					a.Cmp(x, y)
				} else {
					a.Ccmp(x, y, nzcvFalseFor(in.Cond), prev)
				}
				prev = in.Cond
				if insns[idx+1].Op != POpCmpSkip {
					a.Cset(X12, in.Cond)
					a.NegReg(X13, X12)
				}
			case POpLoadCol:
				d := dst(in.A, X9)
				a.LdrRegIdx(d, colReg[in.B], X2)
				fin(in.A, d)
			case POpAccCount:
				a.AddReg(X3, X3, X12)
			case POpAccSum:
				a.AndReg(X11, val(in.A, X9), X13)
				a.AddsReg(X3, X3, X11)
				a.CsetOverflow(X10)
				a.OrrReg(X4, X4, X10)
				if p, ok := phys(in.B); ok {
					a.AddReg(p, p, X12)
				} else {
					a.LdrImm(X9, X1, in.B*8)
					a.AddReg(X9, X9, X12)
					a.StrImm(X9, X1, in.B*8)
				}
			}
			continue
		}
		switch in.Op {
		case POpLoadCol:
			d := dst(in.A, X9)
			a.LdrRegIdx(d, colReg[in.B], X2)
			fin(in.A, d)
		case POpLoadReg:
			set(in.A, val(in.B, X9))
		case POpCmp:
			a.Cmp(val(in.B, X9), val(in.C, X10))
			d := dst(in.A, X11)
			a.Cset(d, in.Cond)
			fin(in.A, d)
		case POpAnd, POpOr:
			x, y, d := val(in.B, X9), val(in.C, X10), dst(in.A, X9)
			if in.Op == POpAnd {
				a.AndReg(d, x, y)
			} else {
				a.OrrReg(d, x, y)
			}
			fin(in.A, d)
		case POpSkipIfZero:
			a.Cbz(val(in.A, X9), "next")
		case POpCmpSkip, POpCmpJump:
			x := X9
			if in.BCol {
				a.LdrRegIdx(X9, colReg[in.B], X2)
			} else {
				x = val(in.B, X9)
			}
			a.Cmp(x, val(in.C, X10))
			if in.Op == POpCmpSkip {
				a.Bcond(in.Cond.Negate(), "next")
			} else {
				a.Bcond(in.Cond, progTarget(in.A, len(insns)))
			}
		case POpSetConst:
			d := dst(in.A, X9)
			a.MovImm64(d, int64(in.B))
			fin(in.A, d)
		case POpJump:
			a.B(progTarget(in.A, len(insns)))
		case POpJumpIfZero:
			a.Cbz(val(in.B, X9), progTarget(in.A, len(insns)))
		case POpJumpIfNotZero:
			a.Cbnz(val(in.B, X9), progTarget(in.A, len(insns)))
		case POpNot:
			a.Cmp(val(in.B, X9), xzr)
			d := dst(in.A, X10)
			a.Cset(d, CondE)
			fin(in.A, d)
		case POpRem:
			// SDIV does not trap, and MSUB then gives C's truncating remainder,
			// 0 for a divisor of -1 included; only a zero divisor (SQL NULL) is
			// flagged, for the caller to decline.
			nz, st := "rn"+itoa(idx), "rs"+itoa(idx)
			into(X9, in.B)
			into(X10, in.C)
			a.Cbnz(X10, nz)
			a.MovImm16(X11, 1)
			a.OrrReg(X4, X4, X11)
			a.B(st)
			a.Label(nz)
			a.Sdiv(X11, X9, X10)
			a.Msub(X9, X11, X10, X9)
			a.Label(st)
			set(in.A, X9)
		case POpAbs:
			into(X9, in.B)
			a.MovImm64(X11, math.MinInt64)
			a.Cmp(X9, X11)
			a.Cset(X11, CondE)
			a.OrrReg(X4, X4, X11)
			a.NegReg(X10, X9)
			a.Cmp(X9, xzr)
			a.Csel(X9, X10, X9, CondL)
			set(in.A, X9)
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
			x, y, d := val(in.B, X9), val(in.C, X10), dst(in.A, X9)
			switch in.Op {
			case POpAdd:
				a.AddsReg(d, x, y)
			case POpSub:
				a.SubsReg(d, x, y)
			default:
				// AArch64 has no flag-setting multiply. SMULH gives the high 64
				// bits; the product overflowed exactly when they are not the
				// sign extension of the low half, which is what this compares.
				a.Smulh(X11, x, y)
				a.Mul(d, x, y)
				a.CmpAsr63(X11, d)
				a.Cset(X10, CondNE)
				a.OrrReg(X4, X4, X10)
				fin(in.A, d)
				continue
			}
			a.CsetOverflow(X10)
			a.OrrReg(X4, X4, X10)
			fin(in.A, d)
		case POpAccCount:
			a.AddImm(X3, X3, 1)
		case POpEmitRow:
			// Sel[acc] = X2 (the loop index); acc++.
			a.LdrImm(X9, X0, POffSel)
			a.AddRegLSL3(X9, X9, X3)
			a.StrImm(X2, X9, 0)
			a.AddImm(X3, X3, 1)
		case POpAccMin, POpAccMax:
			x := val(in.A, X9)
			a.Cmp(X3, x)
			cond := CondG // min: acc > v, so take v
			if in.Op == POpAccMax {
				cond = CondL
			}
			a.Csel(X3, x, X3, cond)
			inc(in.B)
		case POpAccSum:
			a.AddsReg(X3, X3, val(in.A, X9))
			// CSET reads the flags ADDS just set; OR-ing into X4 keeps the
			// answer sticky for the whole loop without a branch.
			a.CsetOverflow(X10)
			a.OrrReg(X4, X4, X10)
			inc(in.B)
		default:
			return nil, fmt.Errorf("jit: unknown program opcode %d", in.Op)
		}
	}
	a.Label("next")
	a.AddImm(X2, X2, 1)
	a.Cbnz(X2, "loop")

	a.Label("done")
	for _, r := range pa.store {
		p, _ := phys(r)
		a.StrImm(p, X1, r*8)
	}
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
	case TextSuffix:
		// The text ends at its first NUL; find it, then the last m bytes
		// before it must be the pattern. X13/X14 scan, X11 becomes the
		// effective length.
		lp, chunk, tail, done := "el"+sfx, "ec"+sfx, "et"+sfx, "ed"+sfx
		a.MovReg(X14, X10)        // the scan pointer
		a.AddReg(X11, X11, X10)   // X11 = the text's end
		a.Label(lp)
		a.AddImm(X13, X14, 16)
		a.Cmp(X13, X11)
		a.BcondRaw(0x8, tail) // HI: 16 more bytes would leave the text
		a.LdrQ(v(0), X14)
		a.Cmeq0_16B(v(1), v(0))
		a.Umaxv16B(v(1), v(1))
		a.UmovB0(X13, v(1))
		a.Cbnz(X13, chunk)
		a.AddImm(X14, X14, 16)
		a.B(lp)
		a.Label(chunk) // a NUL in these 16: the byte loop finds it
		a.AddImm(X11, X14, 16)
		a.Label(tail)
		a.Cmp(X14, X11)
		a.Bcond(CondE, done)
		a.LdrbImm(X13, X14, 0)
		a.Cbz(X13, "ez"+sfx)
		a.AddImm(X14, X14, 1)
		a.B(tail)
		a.Label("ez" + sfx)
		a.MovReg(X11, X14)
		a.Label(done)
		a.SubsReg(X11, X11, X10) // the effective length
		a.CmpImm(X11, uint32(m))
		a.Bcond(CondL, no)
		a.AddReg(X10, X10, X11)
		a.SubsImm(X10, X10, uint32(m))
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
