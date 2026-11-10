package jit

// The VM JIT's TEXT arms in wasm, as vm_text_amd64.go's, a byte at a time.
// txa and txb are the two byte addresses; t1 to t3 are free as in any arm.

// substr emits VSubstr.
func (e *vmEmitWasm) substr(pc int, in VInsn) {
	w, L := e.w, e.lay
	if in.Dead {
		e.forgetReg(in.C)
		return
	}
	w.Get(e.wb)
	e.exitIf(pc)
	for k, typ := range []byte{L.TypText, L.TypInt, L.TypInt} {
		e.loadTyp(in.A + k)
		w.I32(int32(typ))
		w.op(opI32Ne)
		if k > 0 {
			w.op(opI32Or)
		}
	}
	e.loadI64(in.A+2, L.OffI)
	w.I64(1)
	w.op(opI64LtS)
	w.op(opI32Or)
	e.exitIf(pc)
	// y from 1 to the length: y-1 (t1) below the length, unsigned.
	e.loadI64(in.A+1, L.OffI)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.t1)
	w.Get(e.t1)
	e.loadI64(in.A, L.OffS+8)
	w.op(opI64GeU)
	e.exitIf(pc)
	// The answer's length (t2): min(z, length-(y-1)).
	e.loadI64(in.A, L.OffS+8)
	w.Get(e.t1)
	w.op(opI64Sub)
	w.Set(e.t2)
	e.loadI64(in.A+2, L.OffI)
	w.Get(e.t2)
	e.loadI64(in.A+2, L.OffI)
	w.Get(e.t2)
	w.op(opI64LtU)
	w.op(opSelect)
	w.Set(e.t2)
	// The bytes to the end, none NUL or above 0x7f: b-1 below 0x7f, unsigned.
	e.loadI64(in.A, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txa)
	w.Get(e.t1)
	w.Get(e.t2)
	w.op(opI64Add)
	w.op(opI32WrapI64)
	w.Set(e.txb)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.txb)
	w.op(opI32Eqz)
	w.BrIf(1)
	w.Get(e.txa)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.I32(1)
	w.op(opI32Sub)
	w.I32(0x7f)
	w.op(opI32GeU)
	e.exitIf(pc)
	w.Get(e.txa)
	w.I32(1)
	w.op(opI32Add)
	w.Set(e.txa)
	w.Get(e.txb)
	w.I32(1)
	w.op(opI32Sub)
	w.Set(e.txb)
	w.Br(0)
	w.op(opEnd, opEnd)
	// The source's own bytes from y-1, the capacity capped at the length: a
	// register's bytes are never written in place. S first: C may be A.
	w.Get(e.regs)
	e.loadI64(in.A, L.OffS)
	w.Get(e.t1)
	w.op(opI64Add)
	w.StoreI64(e.addr(in.C, L.OffS))
	for _, f := range [][2]int64{{0, int64(L.TypText)}, {int64(L.OffI), 0}, {int64(L.OffF), 0}} {
		w.Get(e.regs)
		w.I64(f[1])
		w.StoreI64(e.addr(in.C, int(f[0])))
	}
	for _, off := range []int{L.OffS + 8, L.OffS + 16} {
		w.Get(e.regs)
		w.Get(e.t2)
		w.StoreI64(e.addr(in.C, off))
	}
	e.forgetReg(in.C)
}

// textCmp emits VTextCmpJump and VTextCmpSet. Both arms leave t1 as r[B]
// against r[A], negative, zero or positive: two INTEGERs by their order, two
// TEXTs by their lengths and then a byte at a time, or ordered as
// bytes.Compare orders them.
func (e *vmEmitWasm) textCmp(pc int, in VInsn) {
	w, L := e.w, e.lay
	if in.Op == VTextCmpSet {
		if in.Dead {
			e.forgetReg(in.C)
			return
		}
		e.guards(pc, e.dstNoS(in, in.C))
	}
	e.loadTyp(in.A)
	w.I32(int32(L.TypInt))
	w.op(opI32Eq)
	w.op(opIf, opVoid)
	e.loadTyp(in.B)
	w.I32(int32(L.TypInt))
	w.op(opI32Ne)
	e.exitIf(pc)
	e.loadI64(in.B, L.OffI)
	e.loadI64(in.A, L.OffI)
	w.op(opI64GtS)
	e.loadI64(in.B, L.OffI)
	e.loadI64(in.A, L.OffI)
	w.op(opI64LtS)
	w.op(opI32Sub)
	w.op(opI64ExtendI32S)
	w.Set(e.t1)
	w.op(opElse)
	e.loadTyp(in.A)
	w.I32(int32(L.TypText))
	w.op(opI32Ne)
	e.loadTyp(in.B)
	w.I32(int32(L.TypText))
	w.op(opI32Ne)
	w.op(opI32Or)
	w.Get(0)
	w.LoadI64(POffUTF16)
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	w.op(opI32Or)
	e.exitIf(pc)
	if in.Imm == 1 {
		// Under a numeric affinity a TEXT that is a whole number is compared
		// as one: the VDBE's.
		e.numericTextExit(pc, in.B)
		e.numericTextExit(pc, in.A)
	}
	if in.Cond != CondE && in.Cond != CondNE {
		e.orderedText(in)
	} else {
		e.equalText(in)
	}
	w.op(opEnd) // INTEGERs or TEXTs
	w.Get(e.t1)
	w.I64(0)
	w.op(scalarCmp(in.Cond))
	if in.Op == VTextCmpJump {
		e.jumpIf(in.T)
		return
	}
	w.op(opI64ExtendI32U)
	w.Set(e.t1)
	e.storeInt(in, in.C, e.t1)
}

// equalText sets t1 to 0 when r[A]'s and r[B]'s TEXTs are the same bytes,
// else 1.
func (e *vmEmitWasm) equalText(in VInsn) {
	w, L := e.w, e.lay
	w.I64(1)
	w.Set(e.t1)
	e.loadI64(in.A, L.OffS+8)
	e.loadI64(in.B, L.OffS+8)
	w.op(opI64Eq)
	w.op(opIf, opVoid)
	e.loadI64(in.A, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txa)
	e.loadI64(in.B, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txb)
	e.loadI64(in.A, L.OffS+8)
	w.Set(e.t2)
	w.I64(0)
	w.Set(e.t1)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.t2)
	w.op(opI64Eqz)
	w.BrIf(1)
	e.bytesDiffer()
	w.op(opIf, opVoid)
	w.I64(1)
	w.Set(e.t1)
	w.Br(2)
	w.op(opEnd)
	e.advanceText()
	w.Br(0)
	w.op(opEnd, opEnd) // the loop and its block
	w.op(opEnd)        // equal lengths
}

// orderedText sets t1 to r[B]'s TEXT against r[A]'s: their bytes unsigned up
// to the shorter's length, then their lengths.
func (e *vmEmitWasm) orderedText(in VInsn) {
	w, L := e.w, e.lay
	e.loadI64(in.B, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txa)
	e.loadI64(in.A, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txb)
	e.loadI64(in.B, L.OffS+8)
	e.loadI64(in.A, L.OffS+8)
	e.loadI64(in.B, L.OffS+8)
	e.loadI64(in.A, L.OffS+8)
	w.op(opI64LtS)
	w.op(opSelect)
	w.Set(e.t2) // the shorter length
	w.I64(0)
	w.Set(e.t1)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.t2)
	w.op(opI64Eqz)
	w.BrIf(1)
	e.bytesDiffer()
	w.op(opIf, opVoid)
	// 1, or -1 when r[B]'s byte is below r[A]'s.
	w.I64(1)
	w.I64(-1)
	e.textByte(e.txa)
	e.textByte(e.txb)
	w.op(opI32GeU)
	w.op(opSelect)
	w.Set(e.t1)
	w.Br(2)
	w.op(opEnd)
	e.advanceText()
	w.Br(0)
	w.op(opEnd, opEnd)
	w.Get(e.t1)
	w.op(opI64Eqz)
	w.op(opIf, opVoid)
	e.loadI64(in.B, L.OffS+8)
	e.loadI64(in.A, L.OffS+8)
	w.op(opI64GtS)
	e.loadI64(in.B, L.OffS+8)
	e.loadI64(in.A, L.OffS+8)
	w.op(opI64LtS)
	w.op(opI32Sub)
	w.op(opI64ExtendI32S)
	w.Set(e.t1)
	w.op(opEnd)
}

// textByte pushes the byte at the address in local p.
func (e *vmEmitWasm) textByte(p uint32) {
	e.w.Get(p)
	e.w.op(opI32Load8U)
	e.w.memarg(0, 0)
}

// bytesDiffer pushes whether the bytes at txa and txb differ.
func (e *vmEmitWasm) bytesDiffer() {
	e.textByte(e.txa)
	e.textByte(e.txb)
	e.w.op(opI32Ne)
}

// advanceText moves txa and txb on a byte, t2 counting down.
func (e *vmEmitWasm) advanceText() {
	w := e.w
	for _, p := range []uint32{e.txa, e.txb} {
		w.Get(p)
		w.I32(1)
		w.op(opI32Add)
		w.Set(p)
	}
	w.Get(e.t2)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.t2)
}

// textLocals declares the three more byte addresses or counts the LIKE, IN
// and numeric-text arms walk with, at first use.
func (e *vmEmitWasm) textLocals() (uint32, uint32, uint32) {
	if e.txc == 0 {
		e.txc, e.txd, e.txe = e.w.Local(wI32), e.w.Local(wI32), e.w.Local(wI32)
	}
	return e.txc, e.txd, e.txe
}

// numericTextExit exits at pc when r[r]'s TEXT is a whole number under
// numeric affinity, as vm_text_amd64.go's: blanks, a sign, digits, a
// fraction, an exponent with digits, blanks, and nothing else, with a digit
// before or after the point. txa walks the bytes, txb counts those left, c
// is the byte; t2 and t3 count the digits before and after the point.
func (e *vmEmitWasm) numericTextExit(pc, r int) {
	w, L := e.w, e.lay
	c, q, m := e.textLocals()
	p, n := e.txa, e.txb
	load := func() { // c = the byte at p
		e.textByte(p)
		w.Set(c)
	}
	next := func() {
		w.Get(p)
		w.I32(1)
		w.op(opI32Add)
		w.Set(p)
		w.Get(n)
		w.I32(1)
		w.op(opI32Sub)
		w.Set(n)
	}
	isSpace := func() { // ' ', or '\t' to '\r'
		w.Get(c)
		w.I32(' ')
		w.op(opI32Eq)
		w.Get(c)
		w.I32(9)
		w.op(opI32Sub)
		w.I32(4)
		w.op(opI32LeU)
		w.op(opI32Or)
	}
	isDigit := func() {
		w.Get(c)
		w.I32('0')
		w.op(opI32Sub)
		w.I32(9)
		w.op(opI32LeU)
	}
	isSign := func() {
		w.Get(c)
		w.I32('+')
		w.op(opI32Eq)
		w.Get(c)
		w.I32('-')
		w.op(opI32Eq)
		w.op(opI32Or)
	}
	// while n != 0 and test holds: on a byte (and count)
	skip := func(test func(), count uint32) {
		w.op(opBlock, opVoid, opLoop, opVoid)
		w.Get(n)
		w.op(opI32Eqz)
		w.BrIf(1)
		load()
		test()
		w.op(opI32Eqz)
		w.BrIf(1)
		next()
		if count != 0 {
			w.Get(count)
			w.I64(1)
			w.op(opI64Add)
			w.Set(count)
		}
		w.Br(0)
		w.op(opEnd, opEnd)
	}
	w.op(opBlock, opVoid) // TEXT: on past the test
	e.loadI64(r, L.OffS)
	w.op(opI32WrapI64)
	w.Set(p)
	e.loadI64(r, L.OffS+8)
	w.op(opI32WrapI64)
	w.Set(n)
	skip(isSpace, 0)
	w.Get(n)
	w.op(opIf, opVoid)
	load()
	isSign()
	w.op(opIf, opVoid)
	next()
	w.op(opEnd, opEnd)
	w.I64(0)
	w.Set(e.t2)
	skip(isDigit, e.t2)
	w.I64(0)
	w.Set(e.t3)
	w.Get(n)
	w.op(opIf, opVoid)
	load()
	w.Get(c)
	w.I32('.')
	w.op(opI32Eq)
	w.op(opIf, opVoid)
	next()
	skip(isDigit, e.t3)
	w.op(opEnd, opEnd)
	w.Get(e.t2)
	w.Get(e.t3)
	w.op(opI64Or)
	w.op(opI64Eqz)
	w.BrIf(0) // no digit at all: no number
	// An exponent counts only with a digit, so it is looked at ahead of
	// moving: q the byte after the 'e', m the bytes left from there.
	w.Get(n)
	w.op(opIf, opVoid)
	load()
	w.Get(c)
	w.I32(0x20)
	w.op(opI32Or)
	w.I32('e')
	w.op(opI32Eq)
	w.op(opIf, opVoid)
	w.Get(p)
	w.I32(1)
	w.op(opI32Add)
	w.Set(q)
	w.Get(n)
	w.I32(1)
	w.op(opI32Sub)
	w.Set(m)
	w.Get(m)
	w.op(opIf, opVoid)
	e.textByte(q)
	w.Set(c)
	isSign()
	w.op(opIf, opVoid)
	w.Get(q)
	w.I32(1)
	w.op(opI32Add)
	w.Set(q)
	w.Get(m)
	w.I32(1)
	w.op(opI32Sub)
	w.Set(m)
	w.op(opEnd)
	w.Get(m)
	w.op(opIf, opVoid)
	e.textByte(q)
	w.Set(c)
	isDigit()
	w.op(opIf, opVoid)
	w.Get(q)
	w.Set(p)
	w.Get(m)
	w.Set(n)
	skip(isDigit, 0)
	w.op(opEnd, opEnd, opEnd, opEnd, opEnd) // a digit, bytes after the sign, after the 'e', an 'e', a byte
	// Blanks to the end: the number, the VDBE's.
	w.op(opLoop, opVoid)
	w.Get(n)
	w.op(opI32Eqz)
	e.exitIf(pc)
	load()
	isSpace()
	w.op(opI32Eqz)
	w.BrIf(1) // something after the number: TEXT
	next()
	w.Br(0)
	w.op(opEnd)
	w.op(opEnd) // TEXT
}

// inText is VInText: each TEXT of the list whose length is r[A]'s is compared
// with it byte by byte. c walks the list's entries, t2 counts those left,
// txa and txb are the two byte addresses and t1 the answer.
func (e *vmEmitWasm) inText(pc int, in VInsn) {
	w, L := e.w, e.lay
	if in.Dead {
		e.forgetReg(in.C)
		return
	}
	ent, _, _ := e.textLocals()
	e.guards(pc, e.dstNoS(in, in.C))
	e.loadTyp(in.A)
	w.I32(int32(L.TypText))
	w.op(opI32Ne)
	w.Get(0)
	w.LoadI64(POffUTF16) // the engine compares UTF-16 TEXT
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	w.op(opI32Or)
	e.exitIf(pc)
	w.I64(0)
	w.Set(e.t1)
	w.I32(int32(in.Imm))
	w.Set(ent)
	w.I64(int64(in.B))
	w.Set(e.t3)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.t3)
	w.op(opI64Eqz)
	w.BrIf(1)
	w.Get(e.t3)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.t3)
	w.Get(ent)
	w.LoadI64(8)
	e.loadI64(in.A, L.OffS+8)
	w.op(opI64Eq)
	w.op(opIf, opVoid)
	w.Get(ent)
	w.LoadI64(0)
	w.op(opI32WrapI64)
	w.Set(e.txa)
	e.loadI64(in.A, L.OffS)
	w.op(opI32WrapI64)
	w.Set(e.txb)
	w.Get(ent)
	w.LoadI64(8)
	w.Set(e.t2)
	w.I64(1)
	w.Set(e.t1)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.t2)
	w.op(opI64Eqz)
	w.BrIf(1)
	e.bytesDiffer()
	w.op(opIf, opVoid)
	w.I64(0)
	w.Set(e.t1)
	w.Br(2)
	w.op(opEnd)
	e.advanceText()
	w.Br(0)
	w.op(opEnd, opEnd)
	w.Get(e.t1)
	w.op(opI32WrapI64)
	w.BrIf(2)   // one of them: the answer
	w.op(opEnd) // the same length
	w.Get(ent)
	w.I32(16)
	w.op(opI32Add)
	w.Set(ent)
	w.Br(0)
	w.op(opEnd, opEnd)
	e.storeInt(in, in.C, e.t1)
}

// text is VText: r[A] the constant TEXT, unless it already is.
func (e *vmEmitWasm) text(pc int, in VInsn) {
	w, L := e.w, e.lay
	if in.Dead {
		e.forgetReg(in.A)
		return
	}
	e.loadI64(in.A, L.OffS)
	w.I64(in.Imm)
	w.op(opI64Eq)
	e.loadI64(in.A, L.OffTyp)
	w.I64(int64(L.TypText)) // and no subtype
	w.op(opI64Eq)
	w.op(opI32And)
	e.loadI64(in.A, L.OffS+8)
	w.I64(int64(in.B))
	w.op(opI64Eq)
	w.op(opI32And)
	w.op(opI32Eqz)
	w.op(opIf, opVoid)
	w.Get(e.wb)
	e.exitIf(pc)
	for _, s := range []struct {
		off int
		v   int64
	}{{L.OffTyp, int64(L.TypText)}, {L.OffI, 0}, {L.OffF, 0}, {L.OffS, in.Imm}, {L.OffS + 8, int64(in.B)}, {L.OffS + 16, int64(in.B)}} {
		w.Get(e.regs)
		w.I64(s.v)
		w.StoreI64(e.addr(in.A, s.off))
	}
	w.op(opEnd)
	e.zeroS.del(in.A)
	e.forgetReg(in.A)
}

// inSub is VInSub: a binary search of the set's ascending keys. c is the
// set, q its keys; t2 and t3 bound the search, ovf its middle, t1 the answer.
func (e *vmEmitWasm) inSub(pc int, in VInsn) {
	w, L := e.w, e.lay
	if in.Dead {
		e.forgetReg(in.C)
		return
	}
	set, keys, _ := e.textLocals()
	e.guards(pc, e.dstNoS(in, in.C))
	in1, in0 := int64(1), int64(0)
	if in.Imm == 1 {
		in1, in0 = 0, 1
	}
	w.LoadPtr(POffInSets)
	w.I32(int32(in.B * VInSubSize))
	w.op(opI32Add)
	w.Set(set)
	w.Get(set)
	w.LoadI64(VInSubOffReady)
	w.op(opI64Eqz)
	e.exitIf(pc)
	w.I64(in0)
	w.Set(e.t1)
	w.op(opBlock, opVoid) // the answer
	w.Get(set)
	w.LoadI64(VInSubOffEmpty)
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	w.BrIf(0)
	e.loadTyp(in.A)
	w.I32(int32(L.TypInt))
	w.op(opI32Ne)
	e.exitIf(pc)
	w.Get(set)
	w.LoadI64(VInSubOffKeys)
	w.op(opI32WrapI64)
	w.Set(keys)
	key := func(i uint32) { // keys[i]
		w.Get(keys)
		w.Get(i)
		w.op(opI32WrapI64)
		w.I32(3)
		w.op(opI32Shl)
		w.op(opI32Add)
		w.LoadI64(0)
	}
	w.I64(0)
	w.Set(e.t2)
	w.Get(set)
	w.LoadI64(VInSubOffN)
	w.Set(e.t3)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.t2)
	w.Get(e.t3)
	w.op(opI64GeS)
	w.BrIf(1)
	w.Get(e.t2)
	w.Get(e.t3)
	w.op(opI64Add)
	w.I64(1)
	w.op(opI64ShrS)
	w.Set(e.ovf)
	// keys[mid] at or above the key: the upper bound, else past mid.
	w.Get(e.t2)
	w.Get(e.ovf)
	w.I64(1)
	w.op(opI64Add)
	w.Get(e.ovf)
	w.Get(e.t3)
	key(e.ovf)
	e.loadI64(in.A, L.OffI)
	w.op(opI64GeS)
	w.op(opSelect)
	w.Set(e.t3)
	key(e.ovf)
	e.loadI64(in.A, L.OffI)
	w.op(opI64GeS)
	w.op(opSelect)
	w.Set(e.t2)
	w.Br(0)
	w.op(opEnd, opEnd)
	w.Get(e.t2)
	w.Get(set)
	w.LoadI64(VInSubOffN)
	w.op(opI64LtS)
	w.op(opIf, opVoid)
	key(e.t2)
	e.loadI64(in.A, L.OffI)
	w.op(opI64Eq)
	w.op(opIf, opVoid)
	w.I64(in1)
	w.Set(e.t1)
	w.Br(2)
	w.op(opEnd, opEnd)
	w.Get(set)
	w.LoadI64(VInSubOffHasNull)
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	e.exitIf(pc) // NULL, which the VDBE answers
	w.op(opEnd)
	e.storeInt(in, in.C, e.t1)
}
