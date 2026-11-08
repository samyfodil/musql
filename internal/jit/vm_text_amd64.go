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
	if in.Imm == 1 {
		// Under a numeric affinity a TEXT that is a whole number is compared
		// as one: the VDBE's.
		e.numericTextExit(pc, in.B)
		e.numericTextExit(pc, in.A)
	}
	if in.Cond != CondE && in.Cond != CondNE {
		e.orderedText(in, tail)
	} else {
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
	}
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

// orderedText leaves the flags as a signed compare of r[B]'s TEXT with
// r[A]'s, both TEXT, would: their bytes unsigned up to the shorter's length,
// then their lengths, as bytes.Compare orders them. It jumps to tail.
func (e *vmEmit) orderedText(in VInsn, tail string) {
	a, L := e.a, e.lay
	loop, diff, lens, out := e.vmLabel("ol"), e.vmLabel("od"), e.vmLabel("on"), e.vmLabel("oo")
	a.MovRegMemD(RAX, RSI, e.d(in.B, L.OffS+8))
	a.MovRegMemD(RCX, RSI, e.d(in.A, L.OffS+8))
	a.MovqXR(X0, RAX)
	a.MovqXR(X1, RCX)
	a.MovRegReg(R10, RAX)
	a.CmpRegReg(R10, RCX)
	a.Cmovcc(CondG, R10, RCX) // the shorter length
	a.MovRegMemD(RDX, RSI, e.d(in.B, L.OffS))
	a.MovRegMemD(R9, RSI, e.d(in.A, L.OffS))
	a.Label(loop)
	a.TestRegReg(R10, R10)
	a.Jcc(CondE, lens)
	a.MovzxByte(RAX, RDX)
	a.MovzxByte(RCX, R9)
	a.CmpRegReg(RAX, RCX)
	a.Jcc(CondNE, diff)
	a.IncReg(RDX)
	a.IncReg(R9)
	a.DecReg(R10)
	a.Jmp(loop)
	a.Label(diff)
	a.MovRegImm64(R10, 1)
	a.MovRegImm64(RDX, -1)
	a.Cmovcc(condCarry, R10, RDX) // below, unsigned
	a.Jmp(out)
	a.Label(lens)
	a.MovqRX(RAX, X0)
	a.MovqRX(RCX, X1)
	a.MovRegImm64(R10, 0)
	a.MovRegImm64(RDX, 1)
	a.MovRegImm64(R9, -1)
	a.CmpRegReg(RAX, RCX)
	a.Cmovcc(CondG, R10, RDX)
	a.Cmovcc(CondL, R10, R9)
	a.Label(out)
	a.CmpRegImm32(R10, 0)
	a.Jmp(tail)
}

// numericTextExit exits at pc when r[r]'s TEXT is a whole number under
// numeric affinity -- parseFullNumeric's: blanks, a sign, digits, a fraction,
// an exponent with digits, blanks, and nothing else, with a digit before or
// after the point -- which the VDBE compares as that number.
func (e *vmEmit) numericTextExit(pc, r int) {
	a, L := e.a, e.lay
	var lb [17]string
	for k := range lb {
		lb[k] = e.vmLabel("nt")
	}
	ws, wsAdv, sign, signAdv, d1, d1Loop, frac, d2, expo, eSign, eDigit, eLoop, trail, trailAdv, num, text :=
		lb[0], lb[1], lb[2], lb[3], lb[4], lb[5], lb[6], lb[7], lb[8], lb[9], lb[10], lb[11], lb[12], lb[13], lb[14], lb[15]
	// R9 the next byte, RAX the bytes left, RCX that byte, RDX the digits
	// before the point and X1 those after it; R10 is scratch.
	next := func() { a.IncReg(R9); a.DecReg(RAX) }
	load := func(end string) { // RCX = the next byte, or on to end at the end
		a.TestRegReg(RAX, RAX)
		a.Jcc(CondE, end)
		a.MovzxByte(RCX, R9)
	}
	isSpace := func(yes string) { // ' ', or '\t' to '\r'
		a.CmpRegImm32(RCX, ' ')
		a.Jcc(CondE, yes)
		a.LeaRegMemD(R10, RCX, -9)
		a.CmpRegImm32(R10, 4)
		a.Jcc(condBelowEq, yes)
	}
	notDigit := func(no string) {
		a.LeaRegMemD(R10, RCX, -'0')
		a.CmpRegImm32(R10, 9)
		a.Jcc(condAbove, no)
	}
	a.MovRegMemD(R9, RSI, e.d(r, L.OffS))
	a.MovRegMemD(RAX, RSI, e.d(r, L.OffS+8))
	a.Label(ws)
	load(text)
	isSpace(wsAdv)
	a.Jmp(sign)
	a.Label(wsAdv)
	next()
	a.Jmp(ws)
	a.Label(sign)
	a.CmpRegImm32(RCX, '+')
	a.Jcc(CondE, signAdv)
	a.CmpRegImm32(RCX, '-')
	a.Jcc(CondNE, d1)
	a.Label(signAdv)
	next()
	a.Label(d1)
	a.XorRegReg(RDX, RDX)
	a.Label(d1Loop)
	load(frac)
	notDigit(frac)
	next()
	a.IncReg(RDX)
	a.Jmp(d1Loop)
	a.Label(frac)
	a.XorRegReg(R10, R10)
	a.MovqXR(X1, R10)
	load(expo)
	a.CmpRegImm32(RCX, '.')
	a.Jcc(CondNE, expo)
	next()
	a.Label(d2)
	load(expo)
	notDigit(expo)
	next()
	a.MovqRX(R10, X1)
	a.IncReg(R10)
	a.MovqXR(X1, R10)
	a.Jmp(d2)
	a.Label(expo)
	a.MovqRX(R10, X1)
	a.OrRegReg(R10, RDX)
	a.Jcc(CondE, text) // no digit at all: no number
	// An exponent counts only with a digit, so it is looked at ahead of
	// moving: RDX the byte after the 'e', R10 the bytes left from there.
	load(trail)
	a.OrRegImm8Lower(RCX)
	a.CmpRegImm32(RCX, 'e')
	a.Jcc(CondNE, trail)
	a.LeaRegMemD(RDX, R9, 1)
	a.LeaRegMemD(R10, RAX, -1)
	a.TestRegReg(R10, R10)
	a.Jcc(CondE, trail)
	a.MovzxByte(RCX, RDX)
	a.CmpRegImm32(RCX, '+')
	a.Jcc(CondE, eSign)
	a.CmpRegImm32(RCX, '-')
	a.Jcc(CondNE, eDigit)
	a.Label(eSign)
	a.IncReg(RDX)
	a.DecReg(R10)
	a.TestRegReg(R10, R10)
	a.Jcc(CondE, trail)
	a.MovzxByte(RCX, RDX)
	a.Label(eDigit)
	a.LeaRegMemD(RCX, RCX, -'0')
	a.CmpRegImm32(RCX, 9)
	a.Jcc(condAbove, trail) // no digit: the 'e' is not the number's
	a.MovRegReg(R9, RDX)
	a.MovRegReg(RAX, R10)
	a.Label(eLoop)
	load(trail)
	notDigit(trail)
	next()
	a.Jmp(eLoop)
	a.Label(trail)
	load(num)
	isSpace(trailAdv)
	a.Jmp(text) // something after the number: TEXT
	a.Label(trailAdv)
	next()
	a.Jmp(trail)
	a.Label(num)
	e.jmpStub(pc)
	a.Label(text)
}

// OrRegImm8Lower is  dst |= 0x20, lowering an ASCII letter.
func (a *Asm) OrRegImm8Lower(dst Reg) {
	a.rex(1, dst)
	a.emit(0x83, modrm(0b11, 1, dst), 0x20)
}

// inText is VInText: each TEXT of the list whose length is r[A]'s is compared
// with it byte by byte. R9 the list entry (in X0 while it is a byte), X2 the
// entries left, RCX and RDX the two byte pointers, R10 the bytes left.
func (e *vmEmit) inText(pc int, in VInsn) {
	if in.Dead {
		e.clobber(in.C)
		return
	}
	a, L := e.a, e.lay
	e.destNoS(pc, in.C)
	ent, cmpb, diff, next, yes, no, done := e.vmLabel("it"), e.vmLabel("ib"), e.vmLabel("id"), e.vmLabel("in"),
		e.vmLabel("iy"), e.vmLabel("io"), e.vmLabel("ie")
	a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypText)
	e.jccStub(CondNE, pc)
	a.CmpMemDByte(RDI, POffUTF16, 0) // the engine compares UTF-16 TEXT
	e.jccStub(CondNE, pc)
	a.MovRegImm64(R9, in.Imm)
	a.MovRegImm64(RAX, int64(in.B))
	a.MovqXR(X2, RAX)
	a.Label(ent)
	a.MovqRX(RAX, X2)
	a.TestRegReg(RAX, RAX)
	a.Jcc(CondE, no)
	a.DecReg(RAX)
	a.MovqXR(X2, RAX)
	a.MovRegMemD(R10, R9, 8)
	a.AluRegMemD(AluCmp, R10, RSI, e.d(in.A, L.OffS+8))
	a.Jcc(CondNE, next)
	a.MovRegMemD(RCX, R9, 0)
	a.MovRegMemD(RDX, RSI, e.d(in.A, L.OffS))
	a.MovqXR(X0, R9)
	a.Label(cmpb)
	a.TestRegReg(R10, R10)
	a.Jcc(CondE, yes)
	a.MovzxByte(RAX, RCX)
	a.MovzxByte(R9, RDX)
	a.CmpRegReg(RAX, R9)
	a.Jcc(CondNE, diff)
	a.IncReg(RCX)
	a.IncReg(RDX)
	a.DecReg(R10)
	a.Jmp(cmpb)
	a.Label(diff)
	a.MovqRX(R9, X0)
	a.Label(next)
	a.AddRegImm32(R9, 16)
	a.Jmp(ent)
	a.Label(yes)
	a.MovRegImm64(R10, 1)
	a.Jmp(done)
	a.Label(no)
	a.MovRegImm64(R10, 0)
	a.Label(done)
	m := e.dest(pc, in.C, R10)
	if m != R10 {
		a.MovRegReg(m, R10)
	}
	e.storeInt(pc, in, in.C, m)
}
