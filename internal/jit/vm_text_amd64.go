//go:build amd64

package jit

// The VM JIT's TEXT arms (vm.go's VSubstr to VTextCmpSet) on x86-64. RAX,
// RDX, R9, R10 and RCX are scratch; every check runs before anything is
// written.

// substr emits VSubstr.
func (e *vmEmit) substr(pc int, in VInsn) {
	a, L := e.a, e.lay
	if in.Dead {
		e.clobber(in.C)
		return
	}
	e.jccWB(pc)
	for k, typ := range []byte{L.TypText, L.TypInt, L.TypInt} {
		a.CmpMemDByte(RSI, e.d(in.A+k, L.OffTyp), typ)
		e.jccStub(CondNE, pc)
	}
	a.CmpMemDImm8(RSI, e.d(in.A+2, L.OffI), 1)
	e.jccStub(CondNE, pc)
	// y from 1 to the length: y-1 below the length, unsigned.
	a.MovRegMemD(RAX, RSI, e.d(in.A+1, L.OffI))
	a.LeaRegMemD(RDX, RAX, -1)
	a.AluRegMemD(AluCmp, RDX, RSI, e.d(in.A, L.OffS+8))
	e.jccStub(condAboveEq, pc)
	// The first y bytes, past those this entry checked at the same pointer
	// (ProgArgs.TextPtr): recorded before they are checked, as a failure
	// exits and the next entry starts afresh.
	fresh, scan, have, got := e.vmLabel("sf"), e.vmLabel("ss"), e.vmLabel("sh"), e.vmLabel("sg")
	a.MovRegMemD(R9, RSI, e.d(in.A, L.OffS))
	a.AluRegMemD(AluCmp, R9, RDI, POffTextPtr)
	a.Jcc(CondNE, fresh)
	a.MovRegMemD(RDX, RDI, POffTextLen)
	a.CmpRegReg(RAX, RDX)
	a.Jcc(condBelowEq, have)
	a.MovMemDReg(RDI, POffTextLen, RAX)
	a.SubRegReg(RAX, RDX)
	a.AddRegReg(R9, RDX)
	a.Jmp(scan)
	a.Label(have)
	a.AddRegReg(R9, RAX)
	a.Jmp(got)
	a.Label(fresh)
	a.MovMemDReg(RDI, POffTextPtr, R9)
	a.MovMemDReg(RDI, POffTextLen, RAX)
	a.Label(scan)
	// Eight at a time while eight are left, none with its high bit set and
	// then none 0 (with no high bit set, a byte borrows when 0x01 is taken
	// from each only where one is 0), then one at a time.
	words, bytes, done := e.vmLabel("sw"), e.vmLabel("sb"), got
	a.MovRegImm64(R10, 0x0101010101010101)
	a.MovRegImm64(RCX, -0x7f7f7f7f7f7f7f80) // 0x8080808080808080
	a.Label(words)
	a.CmpRegImm32(RAX, 8)
	a.Jcc(CondL, bytes)
	a.MovRegMemD(RDX, R9, 0)
	a.TestRegReg(RDX, RCX)
	e.jccStub(CondNE, pc)
	a.SubRegReg(RDX, R10)
	a.TestRegReg(RDX, RCX)
	e.jccStub(CondNE, pc)
	a.AddRegImm8(R9, 8)
	a.AddRegImm8(RAX, -8)
	a.Jmp(words)
	a.Label(bytes)
	a.TestRegReg(RAX, RAX)
	a.Jcc(CondE, done)
	a.MovzxByte(RDX, R9)
	a.DecReg(RDX)
	a.CmpRegImm32(RDX, 0x7f)
	e.jccStub(condAboveEq, pc) // unsigned: a NUL is huge
	a.IncReg(R9)
	a.DecReg(RAX)
	a.Jmp(bytes)
	a.Label(done)
	a.MovzxByteDisp(RDX, R9, -1)
	a.MovRegImm64(RAX, in.Imm)
	a.AddRegReg(RAX, RDX)
	c := e.d(in.C, 0)
	a.MovMemDImm(RSI, c, int32(L.TypText))
	a.MovMemDImm(RSI, c+int32(L.OffI), 0)
	a.MovMemDImm(RSI, c+int32(L.OffF), 0)
	a.MovMemDReg(RSI, c+int32(L.OffS), RAX)
	a.MovMemDImm(RSI, c+int32(L.OffS+8), 1)
	a.MovMemDImm(RSI, c+int32(L.OffS+16), 1)
	e.unmap(in.C)
	e.know.wroteAny(in.C, false)
}

// textCmp emits VTextCmpJump and VTextCmpSet. Both arms end at the same
// place with ZF set when the operands are equal: two INTEGERs by a compare,
// two TEXTs by their lengths and then a byte at a time.
func (e *vmEmit) textCmp(pc int, in VInsn) {
	a, L := e.a, e.lay
	if in.Op == VTextCmpSet {
		if in.Dead {
			e.clobber(in.C)
			return
		}
		e.destNoS(pc, in.C)
	}
	text, loop, tail := e.vmLabel("tt"), e.vmLabel("tl"), e.vmLabel("te")
	a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypInt)
	a.Jcc(CondNE, text)
	a.CmpMemDByte(RSI, e.d(in.B, L.OffTyp), L.TypInt)
	e.jccStub(CondNE, pc)
	a.MovRegMemD(RAX, RSI, e.d(in.B, L.OffI))
	a.AluRegMemD(AluCmp, RAX, RSI, e.d(in.A, L.OffI))
	a.Jmp(tail)
	a.Label(text)
	a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypText)
	e.jccStub(CondNE, pc)
	a.CmpMemDByte(RSI, e.d(in.B, L.OffTyp), L.TypText)
	e.jccStub(CondNE, pc)
	a.CmpMemDByte(RDI, POffUTF16, 0)
	e.jccStub(CondNE, pc)
	a.MovRegMemD(RAX, RSI, e.d(in.A, L.OffS+8))
	a.AluRegMemD(AluCmp, RAX, RSI, e.d(in.B, L.OffS+8))
	a.Jcc(CondNE, tail)
	a.MovRegMemD(RDX, RSI, e.d(in.A, L.OffS))
	a.MovRegMemD(R9, RSI, e.d(in.B, L.OffS))
	a.Label(loop)
	a.TestRegReg(RAX, RAX)
	a.Jcc(CondE, tail)
	a.MovzxByte(R10, RDX)
	a.MovzxByte(RCX, R9)
	a.CmpRegReg(R10, RCX)
	a.Jcc(CondNE, tail)
	a.IncReg(RDX)
	a.IncReg(R9)
	a.DecReg(RAX)
	a.Jmp(loop)
	a.Label(tail)
	if in.Op == VTextCmpJump {
		e.jccBlock(in.Cond, pc, in.T)
		return
	}
	a.MovRegImm64(R10, 0) // a MOV leaves the flags alone
	a.Setcc(in.Cond, R10)
	m := e.dest(pc, in.C, R10)
	if m != R10 {
		a.MovRegReg(m, R10)
	}
	e.storeInt(pc, in, in.C, m)
}
