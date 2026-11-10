//go:build arm64

package jit

// The VM JIT's TEXT arms on AArch64, as vm_text_amd64.go's. X4-X10 are
// scratch: ldr and str keep X11, the guards X12, ldrAt X13.

// substr emits VSubstr.
func (e *vmEmitArm) substr(pc int, in VInsn) {
	a, L := e.a, e.lay
	if in.Dead {
		e.clobber(in.C)
		return
	}
	e.branchWB(pc)
	for k, typ := range []byte{L.TypText, L.TypInt, L.TypInt} {
		e.ldrb(X9, in.A+k, L.OffTyp)
		a.CmpImm(X9, uint32(typ))
		a.Bcond(CondNE, e.stub(pc))
	}
	e.ldr(X9, in.A+2, L.OffI)
	a.CmpImm(X9, 1)
	a.Bcond(CondL, e.stub(pc))
	// y from 1 to the length: y-1 below the length, unsigned.
	e.ldr(X4, in.A+1, L.OffI)
	a.SubsImm(X5, X4, 1)
	e.ldr(X6, in.A, L.OffS+8)
	a.Cmp(X5, X6)
	a.BcondRaw(armHS, e.stub(pc))
	// The answer ends at y-1 + min(z, length-(y-1)).
	clamp := e.vmLabel("sz")
	a.SubsReg(X6, X6, X5)
	a.Cmp(X9, X6)
	a.BcondRaw(armLS, clamp)
	a.MovReg(X9, X6)
	a.Label(clamp)
	a.AddReg(X4, X5, X9)
	// The bytes to the end, past those this entry checked at the same pointer:
	// X7 walks them, X4 counts them.
	fresh, scan, have, got := e.vmLabel("sf"), e.vmLabel("ss"), e.vmLabel("sh"), e.vmLabel("sg")
	e.ldr(X7, in.A, L.OffS)
	a.LdrImm(X5, X0, POffTextPtr)
	a.Cmp(X7, X5)
	a.Bcond(CondNE, fresh)
	a.LdrImm(X5, X0, POffTextLen)
	a.Cmp(X4, X5)
	a.BcondRaw(armLS, have)
	a.StrImm(X4, X0, POffTextLen)
	a.SubsReg(X4, X4, X5)
	a.AddReg(X7, X7, X5)
	a.B(scan)
	a.Label(have)
	a.AddReg(X7, X7, X4)
	a.B(got)
	a.Label(fresh)
	a.StrImm(X7, X0, POffTextPtr)
	a.StrImm(X4, X0, POffTextLen)
	a.Label(scan)
	words, bytes, done := e.vmLabel("sw"), e.vmLabel("sb"), got
	a.MovImm64(X8, 0x0101010101010101)
	a.MovImm64(X10, -0x7f7f7f7f7f7f7f80) // 0x8080808080808080
	a.Label(words)
	a.CmpImm(X4, 8)
	a.Bcond(CondL, bytes)
	a.LdrImm(X9, X7, 0)
	a.AndReg(X5, X9, X10)
	a.Cbnz(X5, e.stub(pc))
	a.SubsReg(X9, X9, X8)
	a.AndReg(X5, X9, X10)
	a.Cbnz(X5, e.stub(pc))
	a.AddImm(X7, X7, 8)
	a.SubsImm(X4, X4, 8)
	a.B(words)
	a.Label(bytes)
	a.Cbz(X4, done)
	a.LdrbImm(X9, X7, 0)
	a.SubsImm(X9, X9, 1)
	a.CmpImm(X9, 0x7f)
	a.BcondRaw(armHS, e.stub(pc)) // unsigned: a NUL is huge
	a.AddImm(X7, X7, 1)
	a.SubsImm(X4, X4, 1)
	a.B(bytes)
	a.Label(done)
	// The source's own bytes from y-1, the capacity capped at the length: a
	// register's bytes are never written in place.
	keep := e.vmLabel("sk")
	e.ldr(X5, in.A+1, L.OffI)
	a.SubsImm(X5, X5, 1)
	e.ldr(X6, in.A, L.OffS+8)
	a.SubsReg(X6, X6, X5)
	e.ldr(X9, in.A+2, L.OffI)
	a.Cmp(X9, X6)
	a.BcondRaw(armHS, keep)
	a.MovReg(X6, X9)
	a.Label(keep)
	e.ldr(X10, in.A, L.OffS)
	a.AddReg(X10, X10, X5)
	a.MovImm16(X9, uint16(L.TypText))
	e.str(X9, in.C, 0)
	e.str(xzr, in.C, L.OffI)
	e.str(xzr, in.C, L.OffF)
	e.str(X10, in.C, L.OffS)
	e.str(X6, in.C, L.OffS+8)
	e.str(X6, in.C, L.OffS+16)
	e.unmap(in.C)
	e.know.wroteAny(in.C, false)
}

// textCmp emits VTextCmpJump and VTextCmpSet: both arms end at tail with Z
// set when the operands are equal.
func (e *vmEmitArm) textCmp(pc int, in VInsn) {
	a, L := e.a, e.lay
	if in.Op == VTextCmpSet {
		if in.Dead {
			e.clobber(in.C)
			return
		}
		e.destNoS(pc, in.C)
	}
	text, loop, tail := e.vmLabel("tt"), e.vmLabel("tl"), e.vmLabel("te")
	e.ldrb(X9, in.A, L.OffTyp)
	e.ldrb(X10, in.B, L.OffTyp)
	a.CmpImm(X9, uint32(L.TypInt))
	a.Bcond(CondNE, text)
	a.CmpImm(X10, uint32(L.TypInt))
	a.Bcond(CondNE, e.stub(pc))
	e.ldr(X9, in.B, L.OffI)
	e.ldr(X10, in.A, L.OffI)
	a.Cmp(X9, X10)
	a.B(tail)
	a.Label(text)
	a.CmpImm(X9, uint32(L.TypText))
	a.Bcond(CondNE, e.stub(pc))
	a.CmpImm(X10, uint32(L.TypText))
	a.Bcond(CondNE, e.stub(pc))
	a.LdrImm(X9, X0, POffUTF16)
	a.Cbnz(X9, e.stub(pc))
	if in.Imm == 1 {
		// Under a numeric affinity a TEXT that is a whole number is compared
		// as one: the VDBE's.
		e.numericTextExit(pc, in.B)
		e.numericTextExit(pc, in.A)
	}
	if in.Cond != CondE && in.Cond != CondNE {
		e.orderedText(in, tail)
	} else {
		e.ldr(X4, in.A, L.OffS+8)
		e.ldr(X5, in.B, L.OffS+8)
		a.Cmp(X4, X5)
		a.Bcond(CondNE, tail)
		e.ldr(X6, in.A, L.OffS)
		e.ldr(X7, in.B, L.OffS)
		a.Label(loop)
		a.CmpImm(X4, 0)
		a.Bcond(CondE, tail)
		a.LdrbImm(X9, X6, 0)
		a.LdrbImm(X10, X7, 0)
		a.Cmp(X9, X10)
		a.Bcond(CondNE, tail)
		a.AddImm(X6, X6, 1)
		a.AddImm(X7, X7, 1)
		a.SubsImm(X4, X4, 1)
		a.B(loop)
	}
	a.Label(tail)
	if in.Op == VTextCmpJump {
		e.jccBlock(in.Cond, pc, in.T)
		return
	}
	m := e.dest(pc, in.C, X9)
	a.Cset(m, in.Cond)
	e.storeInt(in, in.C, m)
}
