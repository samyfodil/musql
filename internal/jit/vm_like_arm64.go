//go:build arm64

package jit

// like is vm_like_amd64.go's VLike: X4 the text, X5 the end of the part left
// to search and X6 its start, X7 the piece's address; X8 and X10 are scratch.
func (e *vmEmitArm) like(pc int, in VInsn) {
	if in.Dead {
		e.clobber(in.C)
		return
	}
	a, L, p := e.a, e.lay, in.Pat
	e.destNoS(pc, in.C)
	match, nomatch, done := e.vmLabel("lm"), e.vmLabel("ln"), e.vmLabel("ld")
	for _, r := range []int{in.A, in.B} {
		e.ldrb(X10, r, L.OffTyp)
		a.CmpImm(X10, uint32(L.TypText))
		a.Bcond(CondNE, e.stub(pc))
	}
	e.ldr(X10, in.B, L.OffS)
	a.MovImm64(X8, int64(p.Src))
	a.Cmp(X10, X8)
	a.Bcond(CondNE, e.stub(pc))
	e.ldr(X10, in.B, L.OffS+8)
	a.MovImm64(X8, int64(p.SrcLen))
	a.Cmp(X10, X8)
	a.Bcond(CondNE, e.stub(pc))
	a.LdrImm(X10, X0, POffLikeCS)
	a.Cbnz(X10, e.stub(pc))
	e.ldr(X4, in.A, L.OffS)
	e.ldr(X5, in.A, L.OffS+8)

	// A NUL ends the text as LIKE reads it: the VDBE's.
	nul, clean := e.vmLabel("lz"), e.vmLabel("lc")
	a.MovImm16(X6, 0)
	a.Label(nul)
	a.Cmp(X6, X5)
	a.Bcond(CondGE, clean)
	a.LdrbReg(X8, X4, X6)
	a.Cbz(X8, e.stub(pc))
	a.AddImm(X6, X6, 1)
	a.B(nul)
	a.Label(clean)
	a.MovImm16(X6, 0)

	at := func(piece []byte, fail string) { // the piece at X7, or on to fail
		for k, c := range piece {
			a.LdrbImm(X8, X7, k)
			if lc := c | 0x20; lc >= 'a' && lc <= 'z' {
				a.MovImm16(X10, 0x20)
				a.OrrReg(X8, X8, X10)
				a.CmpImm(X8, uint32(lc))
			} else {
				a.CmpImm(X8, uint32(c))
			}
			a.Bcond(CondNE, fail)
		}
	}
	pieces := p.Pieces
	if p.Exact {
		a.CmpImm(X5, uint32(len(pieces[0])))
		a.Bcond(CondNE, nomatch)
		a.MovReg(X7, X4)
		at(pieces[0], nomatch)
		a.B(match)
	} else {
		if p.Head && len(pieces) > 0 {
			first := pieces[0]
			pieces = pieces[1:]
			a.CmpImm(X5, uint32(len(first)))
			a.Bcond(CondL, nomatch)
			a.MovReg(X7, X4)
			at(first, nomatch)
			a.MovImm16(X6, uint16(len(first)))
		}
		if p.Tail && len(pieces) > 0 {
			last := pieces[len(pieces)-1]
			pieces = pieces[:len(pieces)-1]
			a.SubsImm(X7, X5, uint32(len(last)))
			a.Cmp(X7, X6)
			a.Bcond(CondL, nomatch)
			a.MovReg(X5, X7)
			a.AddReg(X7, X4, X7)
			at(last, nomatch)
		}
		for _, piece := range pieces {
			find, next, found := e.vmLabel("lf"), e.vmLabel("lx"), e.vmLabel("lo")
			a.Label(find)
			a.AddImm(X7, X6, uint32(len(piece)))
			a.Cmp(X7, X5)
			a.Bcond(CondG, nomatch)
			a.AddReg(X7, X4, X6)
			at(piece, next)
			a.AddImm(X6, X6, uint32(len(piece)))
			a.B(found)
			a.Label(next)
			a.AddImm(X6, X6, 1)
			a.B(find)
			a.Label(found)
		}
		a.B(match)
	}
	yes, no := uint16(1), uint16(0)
	if p.Not {
		yes, no = 0, 1
	}
	a.Label(match)
	a.MovImm16(X8, yes)
	a.B(done)
	a.Label(nomatch)
	a.MovImm16(X8, no)
	a.Label(done)
	m := e.dest(pc, in.C, X8)
	if m != X8 {
		a.MovReg(m, X8)
	}
	e.storeInt(in, in.C, m)
}
