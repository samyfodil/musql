package jit

// like is VLike. The pattern is the program's constant, so each piece is
// compared byte by byte against immediates: a letter by (t|0x20) ==
// (c|0x20), which holds for exactly its two cases, any other byte as itself.
// RDX the text, RAX the end of the part left to search and R9 its start; RCX
// and R10 are scratch.
func (e *vmEmit) like(pc int, in VInsn) {
	if in.Dead {
		e.clobber(in.C)
		return
	}
	a, L, p := e.a, e.lay, in.Pat
	e.destNoS(pc, in.C)
	match, nomatch, done := e.vmLabel("lm"), e.vmLabel("ln"), e.vmLabel("ld")
	a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypText)
	e.jccStub(CondNE, pc)
	a.CmpMemDByte(RSI, e.d(in.B, L.OffTyp), L.TypText)
	e.jccStub(CondNE, pc)
	a.MovRegImm64(RAX, int64(p.Src))
	a.AluRegMemD(AluCmp, RAX, RSI, e.d(in.B, L.OffS))
	e.jccStub(CondNE, pc)
	a.MovRegImm64(RAX, int64(p.SrcLen))
	a.AluRegMemD(AluCmp, RAX, RSI, e.d(in.B, L.OffS+8))
	e.jccStub(CondNE, pc)
	a.CmpMemDImm8(RDI, POffLikeCS, 0)
	e.jccStub(CondNE, pc)
	a.MovRegMemD(RDX, RSI, e.d(in.A, L.OffS))
	a.MovRegMemD(RAX, RSI, e.d(in.A, L.OffS+8))

	// A NUL ends the text as LIKE reads it: the VDBE's.
	nul, clean := e.vmLabel("lz"), e.vmLabel("lc")
	a.XorRegReg(R9, R9)
	a.Label(nul)
	a.CmpRegReg(R9, RAX)
	a.Jcc(CondGE, clean)
	a.MovRegReg(R10, RDX)
	a.AddRegReg(R10, R9)
	a.MovzxByte(RCX, R10)
	a.TestRegReg(RCX, RCX)
	e.jccStub(CondE, pc)
	a.IncReg(R9)
	a.Jmp(nul)
	a.Label(clean)
	a.XorRegReg(R9, R9)

	at := func(piece []byte, fail string) { // the piece at R10, or on to fail
		for k, c := range piece {
			a.MovzxByteDisp(RCX, R10, int8(k))
			if lc := c | 0x20; lc >= 'a' && lc <= 'z' {
				a.OrRegImm8Lower(RCX)
				a.CmpRegImm32(RCX, int32(lc))
			} else {
				a.CmpRegImm32(RCX, int32(c))
			}
			a.Jcc(CondNE, fail)
		}
	}
	pieces := p.Pieces
	if p.Exact {
		a.CmpRegImm32(RAX, int32(len(pieces[0])))
		a.Jcc(CondNE, nomatch)
		a.MovRegReg(R10, RDX)
		at(pieces[0], nomatch)
		a.Jmp(match)
	} else {
		if p.Head && len(pieces) > 0 {
			first := pieces[0]
			pieces = pieces[1:]
			a.CmpRegImm32(RAX, int32(len(first)))
			a.Jcc(CondL, nomatch)
			a.MovRegReg(R10, RDX)
			at(first, nomatch)
			a.MovRegImm64(R9, int64(len(first)))
		}
		if p.Tail && len(pieces) > 0 {
			last := pieces[len(pieces)-1]
			pieces = pieces[:len(pieces)-1]
			a.MovRegReg(R10, RAX)
			a.SubRegImm32(R10, int32(len(last)))
			a.CmpRegReg(R10, R9)
			a.Jcc(CondL, nomatch)
			a.MovRegReg(RAX, R10)
			a.AddRegReg(R10, RDX)
			at(last, nomatch)
		}
		// Each piece left at its first place from R9 on, before RAX.
		for _, piece := range pieces {
			find, next, found := e.vmLabel("lf"), e.vmLabel("lx"), e.vmLabel("lo")
			a.Label(find)
			a.LeaRegMemD(R10, R9, int32(len(piece)))
			a.CmpRegReg(R10, RAX)
			a.Jcc(CondG, nomatch)
			a.MovRegReg(R10, RDX)
			a.AddRegReg(R10, R9)
			at(piece, next)
			a.AddRegImm32(R9, int32(len(piece)))
			a.Jmp(found)
			a.Label(next)
			a.IncReg(R9)
			a.Jmp(find)
			a.Label(found)
		}
		a.Jmp(match)
	}
	yes, no := int64(1), int64(0)
	if p.Not {
		yes, no = 0, 1
	}
	a.Label(match)
	a.MovRegImm64(R10, yes)
	a.Jmp(done)
	a.Label(nomatch)
	a.MovRegImm64(R10, no)
	a.Label(done)
	m := e.dest(pc, in.C, R10)
	if m != R10 {
		a.MovRegReg(m, R10)
	}
	e.storeInt(pc, in, in.C, m)
}
