package jit

// like is VLike, as vm_like_amd64.go's: each piece of the constant pattern
// compared byte by byte against immediates, a letter by (t|0x20) == (c|0x20).
// txa is the text and txb its length; r is where the part left to search
// starts, end where it ends, and base the byte a piece is compared at.
func (e *vmEmitWasm) like(pc int, in VInsn) {
	w, L, p := e.w, e.lay, in.Pat
	if in.Dead {
		e.forgetReg(in.C)
		return
	}
	r, end, base := e.textLocals()
	e.guards(pc, e.dstNoS(in, in.C))
	e.loadTyp(in.A)
	w.I32(int32(L.TypText))
	w.op(opI32Ne)
	e.loadTyp(in.B)
	w.I32(int32(L.TypText))
	w.op(opI32Ne)
	w.op(opI32Or)
	e.loadI64(in.B, L.OffS)
	w.I64(int64(p.Src))
	w.op(opI64Ne)
	w.op(opI32Or)
	e.loadI64(in.B, L.OffS+8)
	w.I64(int64(p.SrcLen))
	w.op(opI64Ne)
	w.op(opI32Or)
	w.Get(0)
	w.LoadI64(POffLikeCS)
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	w.op(opI32Or)
	e.exitIf(pc)
	e.loadI64(in.A, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txa)
	e.loadI64(in.A, L.OffS+8)
	w.op(opI32WrapI64)
	w.Set(e.txb)
	set := func(l uint32, v int32) {
		w.I32(v)
		w.Set(l)
	}
	add := func(l, x uint32, v int32) { // l = x + v
		w.Get(x)
		w.I32(v)
		w.op(opI32Add)
		w.Set(l)
	}

	// A NUL ends the text as LIKE reads it: the VDBE's.
	set(r, 0)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(r)
	w.Get(e.txb)
	w.op(opI32GeS)
	w.BrIf(1)
	w.Get(e.txa)
	w.Get(r)
	w.op(opI32Add)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.op(opI32Eqz)
	e.exitIf(pc)
	add(r, r, 1)
	w.Br(0)
	w.op(opEnd, opEnd)
	set(r, 0)

	at := func(piece []byte, fail uint32) { // the piece at base, or on to fail
		for k, c := range piece {
			w.Get(base)
			w.op(opI32Load8U)
			w.memarg(0, uint32(k))
			if lc := c | 0x20; lc >= 'a' && lc <= 'z' {
				w.I32(0x20)
				w.op(opI32Or)
				w.I32(int32(lc))
			} else {
				w.I32(int32(c))
			}
			w.op(opI32Ne)
			w.BrIf(fail)
		}
	}
	yes, no := int64(1), int64(0)
	if p.Not {
		yes, no = 0, 1
	}
	w.I64(no)
	w.Set(e.t1)
	w.op(opBlock, opVoid) // done
	w.op(opBlock, opVoid) // no match: depth 0 here
	pieces := p.Pieces
	if p.Exact {
		w.Get(e.txb)
		w.I32(int32(len(pieces[0])))
		w.op(opI32Ne)
		w.BrIf(0)
		add(base, e.txa, 0)
		at(pieces[0], 0)
	} else {
		w.Get(e.txb)
		w.Set(end)
		if p.Head && len(pieces) > 0 {
			first := pieces[0]
			pieces = pieces[1:]
			w.Get(e.txb)
			w.I32(int32(len(first)))
			w.op(opI32LtS)
			w.BrIf(0)
			add(base, e.txa, 0)
			at(first, 0)
			set(r, int32(len(first)))
		}
		if p.Tail && len(pieces) > 0 {
			last := pieces[len(pieces)-1]
			pieces = pieces[:len(pieces)-1]
			add(end, e.txb, -int32(len(last)))
			w.Get(end)
			w.Get(r)
			w.op(opI32LtS)
			w.BrIf(0)
			w.Get(e.txa)
			w.Get(end)
			w.op(opI32Add)
			w.Set(base)
			at(last, 0)
		}
		// Each piece left at its first place from r on, before end.
		for _, piece := range pieces {
			w.op(opBlock, opVoid, opLoop, opVoid) // found, find
			w.Get(r)
			w.I32(int32(len(piece)))
			w.op(opI32Add)
			w.Get(end)
			w.op(opI32GtS)
			w.BrIf(2)
			w.op(opBlock, opVoid) // next
			w.Get(e.txa)
			w.Get(r)
			w.op(opI32Add)
			w.Set(base)
			at(piece, 0)
			add(r, r, int32(len(piece)))
			w.Br(2)
			w.op(opEnd)
			add(r, r, 1)
			w.Br(0)
			w.op(opEnd, opEnd)
		}
	}
	w.I64(yes)
	w.Set(e.t1)
	w.Br(1)
	w.op(opEnd, opEnd)
	e.storeInt(in, in.C, e.t1)
}
