package jit

// The VM JIT's segment cursors in wasm, as vm_seg_amd64.go's: a column read
// from the segment's blocks in place, and the rowid and index seeks. curs is
// the cursor table; sk1, sk2 and skf (i32) and skm (i64) are these arms'
// scratch, with t1 to t3 and ovf.

// segLocals declares the seek and segment arms' scratch at first use.
func (e *vmEmitWasm) segLocals() {
	if e.skf == 0 {
		e.sk1, e.sk2, e.skf, e.skm = e.w.Local(wI32), e.w.Local(wI32), e.w.Local(wI32), e.w.Local(wI64)
	}
}

// cursorField pushes field off of cursor A.
func (e *vmEmitWasm) cursorField(a int, off uint32) {
	e.w.Get(e.curs)
	e.w.LoadI64(uint32(a*VCursorSize) + off)
}

// setCursorField stores what push leaves into field off of cursor A.
func (e *vmEmitWasm) setCursorField(a int, off uint32, push func()) {
	e.w.Get(e.curs)
	push()
	e.w.StoreI64(uint32(a*VCursorSize) + off)
}

func (e *vmEmitWasm) setCursorImm(a int, off uint32, v int64) {
	e.setCursorField(a, off, func() { e.w.I64(v) })
}

// ptrAt pushes the i32 address in the i64 field off of the struct at local p.
func (e *vmEmitWasm) ptrAt(p uint32, off uint32) {
	e.w.Get(p)
	e.w.LoadI64(off)
	e.w.op(opI32WrapI64)
}

// index pushes base + i*scale for the i32 address in local base and the i64
// index in local i.
func (e *vmEmitWasm) index(base, i uint32, scale int32) {
	w := e.w
	w.Get(base)
	w.Get(i)
	w.op(opI32WrapI64)
	w.I32(scale)
	w.op(opI32Mul)
	w.op(opI32Add)
}

// cursorChecks exits unless cursor A is usable, column B readable and the
// position a row, which it leaves in t1.
func (e *vmEmitWasm) cursorChecks(pc int, in VInsn) {
	w := e.w
	e.cursorField(in.A, VCursorOffOk)
	w.op(opI64Eqz)
	e.exitIf(pc)
	e.cursorField(in.A, VCursorOffMask) // the column must be one the engine made readable
	w.I64(int64(1) << uint(in.B))
	w.op(opI64And)
	w.op(opI64Eqz)
	e.exitIf(pc)
	e.cursorField(in.A, VCursorOffPos)
	w.Set(e.t1)
	w.Get(e.t1)
	e.cursorField(in.A, VCursorOffN)
	w.op(opI64GeU) // unsigned: a position before the first row is huge
	e.exitIf(pc)
}

// rowColumn is VColumn's read of row t1 of the cursor's row list.
func (e *vmEmitWasm) rowColumn(pc int, in VInsn) {
	w, L := e.w, e.lay
	vb := uint32(in.B * L.Size)
	e.cursorField(in.A, VCursorOffRows)
	w.op(opI32WrapI64)
	w.Get(e.t1)
	w.op(opI32WrapI64)
	w.I32(sliceHeaderSize)
	w.op(opI32Mul)
	w.op(opI32Add)
	w.Set(e.rowp)
	w.Get(e.rowp)
	w.LoadI64(8) // the row's length
	w.I64(int64(in.B))
	w.op(opI64LeU) // a row narrower than the column: the VDBE's
	e.exitIf(pc)
	w.Get(e.rowp)
	w.LoadI64(0)
	w.op(opI32WrapI64)
	w.Set(e.valp)
	// A string or blob is copied with its pointer, as on amd64.
	w.Get(e.valp)
	w.LoadI64(vb + uint32(L.OffS))
	if !e.know.noS.has(in.C) {
		e.loadI64(in.C, L.OffS)
		w.op(opI64Or)
	}
	e.pointerWords(pc, func(f int) {
		w.Get(e.regs)
		w.Get(e.valp)
		w.LoadI64(vb + uint32(f))
		w.StoreI64(e.addr(in.C, f))
	})
}

// segColumn is a VColumn marked segment-capable (Imm 1): column B of cursor
// A's row Pos, from its segment's column blocks when the engine handed it one
// (VCursor.Seg), else from its row list. An exception and a kind this build
// does not read exit.
func (e *vmEmitWasm) segColumn(pc int, in VInsn) {
	w, L := e.w, e.lay
	e.segLocals()
	e.cursorChecks(pc, in)
	e.cursorField(in.A, VCursorOffSeg)
	w.op(opI64Eqz)
	e.openIf()
	e.rowColumn(pc, in)
	w.op(opElse)
	// The segment's: rowp the VSeg, t1 its row (Sel[Pos] when the cursor has
	// a selection), valp the column's VSegCol.
	e.cursorField(in.A, VCursorOffSeg)
	w.op(opI32WrapI64)
	w.Set(e.rowp)
	// Several segments' run: the row's own VSeg (VCursor.Segs).
	e.cursorField(in.A, VCursorOffSegs)
	w.op(opI32WrapI64)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opIf, opVoid)
	e.index(e.sk1, e.t1, 8)
	w.LoadI64(0)
	w.op(opI32WrapI64)
	w.Set(e.rowp)
	w.op(opEnd)
	e.cursorField(in.A, VCursorOffSel)
	w.op(opI32WrapI64)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opIf, opVoid)
	e.index(e.sk1, e.t1, 8)
	w.LoadI64(0)
	w.Set(e.t1)
	w.op(opEnd)
	e.ptrAt(e.rowp, VSegOffCols)
	w.I32(int32(in.B * VSegColSize))
	w.op(opI32Add)
	w.Set(e.valp)
	// A NULL: its bit in the bitmap, kept in t3 for the read below.
	w.I64(0)
	w.Set(e.t3)
	e.ptrAt(e.valp, VSegColOffNulls)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opIf, opVoid)
	w.Get(e.sk1)
	w.Get(e.t1)
	w.I64(3)
	w.op(opI64ShrU)
	w.op(opI32WrapI64)
	w.op(opI32Add)
	w.op(opI32Load8U)
	w.memarg(0, 0)
	w.Get(e.t1)
	w.op(opI32WrapI64)
	w.I32(7)
	w.op(opI32And)
	w.op(opI32ShrU)
	w.I32(1)
	w.op(opI32And)
	w.op(opI64ExtendI32U)
	w.Set(e.t3)
	w.op(opEnd)
	// An exception: its bit in the 64-bit words.
	e.ptrAt(e.valp, VSegColOffExc)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opIf, opVoid)
	w.Get(e.sk1)
	w.Get(e.t1)
	w.I64(6)
	w.op(opI64ShrU)
	w.op(opI32WrapI64)
	w.I32(3)
	w.op(opI32Shl)
	w.op(opI32Add)
	w.LoadI64(0)
	w.Get(e.t1)
	w.I64(63)
	w.op(opI64And)
	w.op(opI64ShrU)
	w.I64(1)
	w.op(opI64And)
	w.op(opI32WrapI64)
	e.exitIf(pc)
	w.op(opEnd)
	// Every word of r[C] is written: one holding a pointer needs the barrier
	// off.
	if !e.know.noS.has(in.C) {
		e.loadI64(in.C, L.OffS)
		w.op(opI64Eqz)
		w.op(opI32Eqz)
		w.Get(e.wb)
		w.op(opI32And)
		e.exitIf(pc)
	}
	// The column's kind, or -1 for a NULL cell, which is a NULL as
	// vm_seg_amd64.go's segColumn reads it.
	w.Get(e.t3)
	w.op(opI64Eqz)
	w.op(opIf, opVoid)
	w.Get(e.valp)
	w.LoadI64(VSegColOffKind)
	w.Set(e.t3)
	w.op(opElse)
	w.I64(-1)
	w.Set(e.t3)
	w.op(opEnd)
	cell := func() { // t2 = the 8-byte cell at row t1
		e.ptrAt(e.valp, VSegColOffBlock)
		w.Set(e.sk1)
		e.index(e.sk1, e.t1, 8)
		w.LoadI64(0)
		w.Set(e.t2)
	}
	store := func(off int, push func()) {
		w.Get(e.regs)
		push()
		w.StoreI64(e.addr(in.C, off))
	}
	writeNum := func(typ byte, i, f uint32) { // a number, no string
		store(L.OffTyp, func() { w.I64(int64(typ)) })
		for _, s := range []struct {
			off int
			l   uint32
		}{{L.OffI, i}, {L.OffF, f}} {
			store(s.off, func() {
				if s.l != 0 {
					w.Get(s.l)
				} else {
					w.I64(0)
				}
			})
		}
		for k := range 3 {
			store(L.OffS+8*k, func() { w.I64(0) })
		}
	}
	kind := func(k int, body func()) {
		w.Get(e.t3)
		w.I64(int64(k))
		w.op(opI64Eq)
		w.op(opIf, opVoid)
		body()
		w.Br(1)
		w.op(opEnd)
	}
	w.op(opBlock, opVoid) // done
	kind(-1, func() { writeNum(L.TypNull, 0, 0) })
	kind(VSegInt, func() {
		cell()
		writeNum(L.TypInt, e.t2, 0)
	})
	kind(VSegFloat, func() {
		cell()
		writeNum(L.TypFloat, 0, e.t2)
	})
	kind(VSegIntReal, func() {
		cell()
		w.Get(e.t2)
		w.op(opF64ConvertI64S)
		w.op(opI64ReinterpretF64)
		w.Set(e.t2)
		writeNum(L.TypFloat, 0, e.t2)
	})
	// An integral double within 2^53 is the INTEGER, any other the REAL
	// (numericValue). A NaN or a value past int64 fails the round trip.
	kind(VSegNumeric, func() {
		cell()
		w.Get(e.t2)
		w.op(opF64ReinterpretI64)
		w.op(opMisc, miscI64TruncSatF64S)
		w.Set(e.skm)
		w.Get(e.skm)
		w.op(opF64ConvertI64S)
		w.Get(e.t2)
		w.op(opF64ReinterpretI64)
		w.op(opF64Eq)
		w.Get(e.skm)
		w.I64(1 << 53)
		w.op(opI64Add)
		w.I64(1 << 54)
		w.op(opI64LeU)
		w.op(opI32And)
		w.op(opIf, opVoid)
		writeNum(L.TypInt, e.skm, 0)
		w.op(opElse)
		writeNum(L.TypFloat, 0, e.t2)
		w.op(opEnd)
	})
	kind(VSegRowid, func() {
		w.Get(e.rowp)
		w.LoadI64(VSegOffDense)
		w.op(opI64Eqz)
		w.op(opIf, opVoid)
		e.ptrAt(e.rowp, VSegOffRowids)
		w.Set(e.sk1)
		e.index(e.sk1, e.t1, 8)
		w.LoadI64(0)
		w.Set(e.t2)
		w.op(opElse)
		w.Get(e.rowp)
		w.LoadI64(VSegOffFirst)
		w.Get(e.t1)
		w.op(opI64Add)
		w.Set(e.t2)
		w.op(opEnd)
		writeNum(L.TypInt, e.t2, 0)
	})
	// TEXT: the bytes from bound t1 to bound t1+1 of the heap, shared as
	// segment.Value shares them, which is a pointer store. An empty string
	// points at the heap's start.
	kind(VSegText, func() {
		w.Get(e.wb)
		e.exitIf(pc)
		e.ptrAt(e.valp, VSegColOffBlock)
		w.Set(e.sk1)
		e.index(e.sk1, e.t1, 4)
		w.Set(e.sk1)
		w.Get(e.sk1)
		w.op(opI64Load32U)
		w.memarg(2, 4)
		w.Get(e.sk1)
		w.op(opI64Load32U)
		w.memarg(2, 0)
		w.op(opI64Sub)
		w.Set(e.t2) // the length
		w.Get(e.valp)
		w.LoadI64(VSegColOffHeap)
		w.Get(e.sk1)
		w.op(opI64Load32U)
		w.memarg(2, 0)
		w.I64(0)
		w.Get(e.t2)
		w.op(opI64Eqz)
		w.op(opI32Eqz)
		w.op(opSelect)
		w.op(opI64Add)
		w.Set(e.t3)
		store(L.OffTyp, func() { w.I64(int64(L.TypText)) })
		store(L.OffI, func() { w.I64(0) })
		store(L.OffF, func() { w.I64(0) })
		store(L.OffS, func() { w.Get(e.t3) })
		store(L.OffS+8, func() { w.Get(e.t2) })
		store(L.OffS+16, func() { w.Get(e.t2) })
	})
	e.exitAt(pc) // a kind this build does not read
	w.op(opEnd)
	e.closeIf()
	e.zeroS.del(in.C)
	e.know.wroteAny(in.C, false)
}

// segRowid is VRowid: a selection's position names its segment row
// (VCursor.Sel), and with no rowid list a dense segment's rowid is its first
// plus the row.
func (e *vmEmitWasm) segRowid(pc int, in VInsn) {
	w := e.w
	if in.Dead {
		e.forgetReg(in.C)
		return
	}
	e.segLocals()
	e.guards(pc, e.dstNoS(in, in.C))
	e.cursorField(in.A, VCursorOffOk)
	w.op(opI64Eqz)
	e.exitIf(pc)
	e.cursorField(in.A, VCursorOffPos)
	w.Set(e.t1)
	w.Get(e.t1)
	e.cursorField(in.A, VCursorOffN)
	w.op(opI64GeU) // unsigned: a position before the first row is huge
	e.exitIf(pc)
	e.cursorField(in.A, VCursorOffSegs) // a row of several segments' run
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	e.exitIf(pc)
	e.cursorField(in.A, VCursorOffSel)
	w.op(opI32WrapI64)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opIf, opVoid)
	e.index(e.sk1, e.t1, 8)
	w.LoadI64(0)
	w.Set(e.t1)
	w.op(opEnd)
	e.cursorField(in.A, VCursorOffRids)
	w.op(opI32WrapI64)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opIf, opVoid)
	e.index(e.sk1, e.t1, 8)
	w.LoadI64(0)
	w.Set(e.t1)
	w.op(opElse)
	e.cursorField(in.A, VCursorOffSeg)
	w.op(opI32WrapI64)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opI32Eqz)
	e.exitIf(pc)
	w.Get(e.sk1)
	w.LoadI64(VSegOffDense)
	w.op(opI64Eqz)
	e.exitIf(pc)
	w.Get(e.sk1)
	w.LoadI64(VSegOffFirst)
	w.Get(e.t1)
	w.op(opI64Add)
	w.Set(e.t1)
	w.op(opEnd)
	e.storeInt(in, in.C, e.t1)
}

// search leaves in t3 the first of the n (local hi, i64) ascending int64s at
// the address in local base that is not below the key in t1. ovf is the
// middle; hi is consumed.
func (e *vmEmitWasm) search(base, hi uint32) {
	w := e.w
	w.I64(0)
	w.Set(e.t3)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.t3)
	w.Get(hi)
	w.op(opI64GeS)
	w.BrIf(1)
	w.Get(e.t3)
	w.Get(hi)
	w.op(opI64Add)
	w.I64(1)
	w.op(opI64ShrS)
	w.Set(e.ovf)
	at := func() {
		e.index(base, e.ovf, 8)
		w.LoadI64(0)
		w.Get(e.t1)
		w.op(opI64GeS)
	}
	// at or above the key: the upper bound, else past the middle.
	w.Get(e.t3)
	w.Get(e.ovf)
	w.I64(1)
	w.op(opI64Add)
	at()
	w.op(opSelect)
	w.Get(e.ovf)
	w.Get(hi)
	at()
	w.op(opSelect)
	w.Set(hi)
	w.Set(e.t3)
	w.Br(0)
	w.op(opEnd, opEnd)
}

// seekKey exits unless r[B] is an INTEGER and the barrier is off (the
// cursor's segment pointers are stored), and leaves it in t1.
func (e *vmEmitWasm) seekKey(pc int, in VInsn) {
	w, L := e.w, e.lay
	e.loadTyp(in.B)
	w.I32(int32(L.TypInt))
	w.op(opI32Ne)
	w.Get(e.wb)
	w.op(opI32Or)
	e.exitIf(pc)
	e.loadI64(in.B, L.OffI)
	w.Set(e.t1)
}

// outside pushes whether the key in t1 is outside the range at lo and hi of
// the struct at local p.
func (e *vmEmitWasm) outside(p uint32, lo, hi uint32) {
	w := e.w
	w.Get(e.t1)
	w.Get(p)
	w.LoadI64(lo)
	w.op(opI64LtS)
	w.Get(e.t1)
	w.Get(p)
	w.LoadI64(hi)
	w.op(opI64GtS)
	w.op(opI32Or)
}

// seekMiss puts cursor A on no row, keyed t1, as a seek that finds none.
func (e *vmEmitWasm) seekMiss(a int) {
	e.setCursorField(a, VCursorOffKey, func() { e.w.Get(e.t1) })
	e.setCursorImm(a, VCursorOffN, 0)
	e.setCursorImm(a, VCursorOffPos, 0)
	e.setCursorImm(a, VCursorOffSegs, 0)
	e.setCursorImm(a, VCursorOffOk, 1)
	e.setCursorImm(a, VCursorOffDirty, 1)
	e.setCursorImm(a, VCursorOffState, 3)
}

// seekRowid is VSeekRowid. The segments are tried in turn, each by its rowid
// range: a dense one holds the key at key-First, any other is searched. sk1
// walks the VTabSegs, t2 counts those left, sk2 is the VSeg, t3 the row and
// skf whether none has it.
func (e *vmEmitWasm) seekRowid(pc int, in VInsn) {
	w := e.w
	e.segLocals()
	e.seekKey(pc, in)
	e.cursorField(in.A, VCursorOffTab)
	w.op(opI32WrapI64)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opI32Eqz)
	e.exitIf(pc)
	w.Get(e.sk1)
	w.LoadI64(VTabOffN)
	w.Set(e.t2)
	e.ptrAt(e.sk1, VTabOffSegs)
	w.Set(e.sk1)
	w.I32(1)
	w.Set(e.skf)
	w.op(opBlock, opVoid, opLoop, opVoid) // out, seg
	w.Get(e.t2)
	w.op(opI64Eqz)
	w.BrIf(1)
	w.op(opBlock, opVoid) // next
	e.outside(e.sk1, VTabSegOffLo, VTabSegOffHi)
	w.BrIf(0)
	e.ptrAt(e.sk1, VTabSegOffSeg)
	w.Set(e.sk2)
	w.Get(e.sk2)
	w.LoadI64(VSegOffDense)
	w.op(opI64Eqz)
	w.op(opI32Eqz)
	w.op(opIf, opVoid)
	w.Get(e.t1)
	w.Get(e.sk2)
	w.LoadI64(VSegOffFirst)
	w.op(opI64Sub)
	w.Set(e.t3)
	w.I32(0)
	w.Set(e.skf)
	w.Br(3)
	w.op(opEnd)
	// Its rowid block ascends, compared signed, as segment.findRowid compares.
	w.Get(e.sk1)
	w.LoadI64(VTabSegOffRows)
	w.Set(e.skm)
	e.ptrAt(e.sk2, VSegOffRowids)
	w.Set(e.sk2)
	e.search(e.sk2, e.skm)
	w.Get(e.t3)
	w.Get(e.sk1)
	w.LoadI64(VTabSegOffRows)
	w.op(opI64GeS)
	w.BrIf(0)
	e.index(e.sk2, e.t3, 8)
	w.LoadI64(0)
	w.Get(e.t1)
	w.op(opI64Ne)
	w.BrIf(0)
	e.ptrAt(e.sk1, VTabSegOffSeg)
	w.Set(e.sk2)
	w.I32(0)
	w.Set(e.skf)
	w.Br(2)
	w.op(opEnd) // next
	w.Get(e.sk1)
	w.I32(VTabSegSize)
	w.op(opI32Add)
	w.Set(e.sk1)
	w.Get(e.t2)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.t2)
	w.Br(0)
	w.op(opEnd, opEnd)
	// Found at row t3 of VSeg sk2, sk1 its VTabSeg: the cursor on that one
	// row, as a point seek leaves it.
	w.Get(e.skf)
	w.op(opI32Eqz)
	w.op(opIf, opVoid)
	ptr := func(p uint32) func() {
		return func() {
			w.Get(p)
			w.op(opI64ExtendI32U)
		}
	}
	e.setCursorField(in.A, VCursorOffSeg, ptr(e.sk2))
	e.setCursorField(in.A, VCursorOffKey, func() { w.Get(e.t1) })
	e.setCursorField(in.A, VCursorOffRids, func() { w.Get(e.sk2); w.LoadI64(VSegOffRowids) })
	e.setCursorField(in.A, VCursorOffMask, func() { w.Get(e.sk1); w.LoadI64(VTabSegOffMask) })
	e.setCursorImm(in.A, VCursorOffSel, 0)
	e.setCursorImm(in.A, VCursorOffSegs, 0)
	e.setCursorField(in.A, VCursorOffPos, func() { w.Get(e.t3) })
	e.setCursorField(in.A, VCursorOffLast, func() { w.Get(e.t3) })
	e.setCursorField(in.A, VCursorOffN, func() { w.Get(e.t3); w.I64(1); w.op(opI64Add) })
	e.setCursorImm(in.A, VCursorOffOk, 1)
	e.setCursorImm(in.A, VCursorOffDirty, 1)
	e.setCursorImm(in.A, VCursorOffAdv, 1)
	e.setCursorImm(in.A, VCursorOffState, 2)
	e.setCursorField(in.A, VCursorOffSrvd, func() { e.cursorField(in.A, VCursorOffSrvd); w.I64(1); w.op(opI64Add) })
	w.op(opElse)
	e.seekMiss(in.A)
	w.op(opEnd)
	w.Get(e.skf)
	e.jumpIf(in.T)
}

// seekIdx is VSeekIdx: the segments' indexes in turn, each skipped when the
// key is outside its range and otherwise searched; a find is the cursor on
// the rows its run names (Sel), unless a later segment's range holds the key
// too. sk1 walks the VIdxs, t2 counts those left, t3 is the value found and
// skf whether none has it.
func (e *vmEmitWasm) seekIdx(pc int, in VInsn) {
	w := e.w
	e.segLocals()
	e.seekKey(pc, in)
	e.cursorField(in.A, VCursorOffIdx)
	w.op(opI32WrapI64)
	w.Set(e.sk1)
	w.Get(e.sk1)
	w.op(opI32Eqz)
	e.exitIf(pc)
	w.Get(e.sk1)
	w.LoadI64(VIdxOffCount)
	w.Set(e.t2)
	w.I32(1)
	w.Set(e.skf)
	w.op(opBlock, opVoid, opLoop, opVoid) // out, seg
	w.Get(e.t2)
	w.op(opI64Eqz)
	w.BrIf(1)
	w.op(opBlock, opVoid) // next
	e.outside(e.sk1, VIdxOffLo, VIdxOffHi)
	w.BrIf(0)
	w.Get(e.sk1)
	w.LoadI64(VIdxOffNVals)
	w.Set(e.skm)
	e.ptrAt(e.sk1, VIdxOffVals)
	w.Set(e.sk2)
	e.search(e.sk2, e.skm)
	w.Get(e.t3)
	w.Get(e.sk1)
	w.LoadI64(VIdxOffNVals)
	w.op(opI64GeS)
	w.BrIf(0)
	e.index(e.sk2, e.t3, 8)
	w.LoadI64(0)
	w.Get(e.t1)
	w.op(opI64Ne)
	w.BrIf(0)
	// Found value t3 of segment sk1: a later segment whose range holds the
	// key may hold some of its rows, which the VDBE gathers.
	w.Get(e.t2)
	w.Set(e.ovf)
	w.Get(e.sk1)
	w.Set(e.sk2)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(e.ovf)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.ovf)
	w.Get(e.ovf)
	w.op(opI64Eqz)
	w.BrIf(1)
	w.Get(e.sk2)
	w.I32(VIdxSize)
	w.op(opI32Add)
	w.Set(e.sk2)
	e.outside(e.sk2, VIdxOffLo, VIdxOffHi)
	w.op(opI32Eqz)
	e.exitIf(pc)
	w.Br(0)
	w.op(opEnd, opEnd)
	w.I32(0)
	w.Set(e.skf)
	w.Br(2)
	w.op(opEnd) // next
	w.Get(e.sk1)
	w.I32(VIdxSize)
	w.op(opI32Add)
	w.Set(e.sk1)
	w.Get(e.t2)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.t2)
	w.Br(0)
	w.op(opEnd, opEnd)
	e.seekIdxServed(in.A)
	w.Get(e.skf)
	w.op(opI32Eqz)
	w.op(opIf, opVoid)
	// Its rows are Rows[Starts[t3]:Starts[t3+1]].
	start := func(k uint32) {
		e.ptrAt(e.sk1, VIdxOffStarts)
		w.Set(e.sk2)
		e.index(e.sk2, e.t3, 4)
		w.op(opI64Load32S)
		w.memarg(2, k)
	}
	e.setCursorField(in.A, VCursorOffKey, func() { w.Get(e.t1) })
	start(0)
	w.Set(e.t2)
	e.setCursorField(in.A, VCursorOffN, func() { start(4); w.Get(e.t2); w.op(opI64Sub) })
	e.setCursorField(in.A, VCursorOffSel, func() {
		w.Get(e.sk1)
		w.LoadI64(VIdxOffRows)
		w.Get(e.t2)
		w.I64(3)
		w.op(opI64Shl)
		w.op(opI64Add)
	})
	e.setCursorField(in.A, VCursorOffSegs, func() {
		w.Get(e.sk1)
		w.LoadI64(VIdxOffSegs)
		w.Get(e.t2)
		w.I64(3)
		w.op(opI64Shl)
		w.op(opI64Add)
		w.I64(0)
		w.Get(e.sk1)
		w.LoadI64(VIdxOffSegs)
		w.op(opI64Eqz)
		w.op(opI32Eqz)
		w.op(opSelect)
	})
	e.setCursorField(in.A, VCursorOffSeg, func() { w.Get(e.sk1); w.LoadI64(VIdxOffSeg) })
	e.setCursorField(in.A, VCursorOffRids, func() {
		e.ptrAt(e.sk1, VIdxOffSeg)
		w.LoadI64(VSegOffRowids)
	})
	e.setCursorField(in.A, VCursorOffMask, func() { w.Get(e.sk1); w.LoadI64(VIdxOffMask) })
	e.setCursorImm(in.A, VCursorOffPos, 0)
	e.setCursorImm(in.A, VCursorOffLast, 0)
	e.setCursorImm(in.A, VCursorOffOk, 1)
	e.setCursorImm(in.A, VCursorOffDirty, 1)
	e.setCursorImm(in.A, VCursorOffAdv, 1)
	e.setCursorImm(in.A, VCursorOffState, 2)
	w.op(opElse)
	e.seekMiss(in.A)
	w.op(opEnd)
	w.Get(e.skf)
	e.jumpIf(in.T)
}

// seekIdxServed adds to cursor A's Served the rows its last seek's run
// served, up to the row it is on, when it has one (State 2, 4 or 5).
func (e *vmEmitWasm) seekIdxServed(a int) {
	w := e.w
	for i, s := range []int64{2, 4, 5} {
		e.cursorField(a, VCursorOffState)
		w.I64(s)
		w.op(opI64Eq)
		if i > 0 {
			w.op(opI32Or)
		}
	}
	w.op(opIf, opVoid)
	e.setCursorField(a, VCursorOffSrvd, func() {
		e.cursorField(a, VCursorOffLast)
		w.I64(1)
		w.op(opI64Add)
		e.cursorField(a, VCursorOffSrvd)
		w.op(opI64Add)
	})
	w.op(opEnd)
}

// nullRow is vm_seg_amd64.go's VNullRow.
func (e *vmEmitWasm) nullRow(pc int, in VInsn) {
	e.w.Get(e.wb)
	e.exitIf(pc)
	e.setCursorImm(in.A, VCursorOffSeg, nullSegAddr())
	for _, f := range []struct {
		off uint32
		v   int64
	}{{VCursorOffSel, 0}, {VCursorOffSegs, 0}, {VCursorOffRids, 0}, {VCursorOffN, 1}, {VCursorOffPos, 0}, {VCursorOffLast, 0}, {VCursorOffOk, 1},
		{VCursorOffDirty, 1}, {VCursorOffAdv, 0}, {VCursorOffRew, 0}, {VCursorOffMask, -1}, {VCursorOffState, 6}} {
		e.setCursorImm(in.A, f.off, f.v)
	}
}

// seekRewind is vm_seg_amd64.go's VSeekRewind.
func (e *vmEmitWasm) seekRewind(pc int, in VInsn) {
	w := e.w
	e.cursorField(in.A, VCursorOffState)
	w.I64(3)
	w.op(opI64Eq)
	e.jumpIf(in.T)
	e.cursorField(in.A, VCursorOffState)
	w.I64(2)
	w.op(opI64Ne)
	e.exitIf(pc)
	e.setCursorImm(in.A, VCursorOffState, 4)
}
