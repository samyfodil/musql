//go:build arm64

package jit

import "slices"

// The segment cursor, the native rowid seek and the constant TEXT on AArch64,
// as vm_seg_amd64.go's. X4-X10 are scratch, and the seek's search X11-X13 as
// well: it calls no helper that keeps them.

// LdrWImm emits LDR Wt, [Xn, #off]: the uint32 there, zero-extended.
func (a *Arm) LdrWImm(rt, rn Reg64, off int) {
	a.emit(0xB9400000 | uint32(off/4)<<10 | uint32(rn)<<5 | uint32(rt))
}

// curBase leaves cursor c's VCursor in X9.
func (e *vmEmitArm) curBase(c int) {
	e.a.LdrImm(X9, X0, POffCur)
	if cb := c * VCursorSize; cb != 0 {
		e.a.MovImm64(X10, int64(cb))
		e.a.AddReg(X9, X9, X10)
	}
}

// segColumn is vm_seg_amd64.go's: column B of cursor A's row Pos, from the
// segment's blocks when the engine handed one over (VCursor.Seg), else from
// its row list. X9 the cursor, X5 the row, X6 the VSeg, X7 the VSegCol.
//
// A predicted kind (T) shares a run's checks of the cursor and reads a plain
// column of that kind directly, as on amd64.
// vmSegKinds is every kind a segment's column reads natively as, in the order
// segColumn tests them.
var vmSegKinds = [...]int{VSegInt, VSegFloat, VSegNumeric, VSegIntReal, VSegRowid, VSegText}

func (e *vmEmitArm) segColumn(pc int, in VInsn) {
	a := e.a
	e.destNoS(pc, in.C)
	e.zeroS.del(in.C) // every word of the destination is written below
	done, full := e.vmLabel("sd"), e.vmLabel("sf")
	kind := ""
	if slices.Contains(vmSegKinds[:], in.T) {
		kind = e.vmLabel("sk")
		fnosel, prows := e.vmLabel("so"), e.vmLabel("sr")
		base, have := e.holds[^in.A]
		if have {
			e.pinned.add(int(base))
		} else {
			mask := uint64(1) << uint(in.B)
			if e.cache {
				mask = vmRunPredicted(e.prog, e.bound, pc)
			}
			e.curBase(in.A)
			a.LdrImm(X10, X9, VCursorOffOk)
			a.Cbz(X10, e.stub(pc))
			a.LdrImm(X10, X9, VCursorOffMask)
			a.MovImm64(X4, int64(mask))
			a.AndReg(X10, X10, X4)
			a.Cmp(X10, X4)
			a.Bcond(CondNE, e.stub(pc))
			a.LdrImm(X5, X9, VCursorOffPos)
			a.LdrImm(X10, X9, VCursorOffN)
			a.Cmp(X5, X10)
			a.BcondRaw(armHS, e.stub(pc)) // unsigned: a position before the first row is huge
			base = X9
			if u := e.nextUse(^in.A, pc); u >= 0 {
				if m, ok := e.alloc(pc, u); ok {
					a.MovReg(m, X9)
					base = m
					e.holds[^in.A] = m
					e.pinned.add(int(m))
				}
			}
		}
		a.LdrImm(X5, base, VCursorOffPos)
		a.LdrImm(X6, base, VCursorOffSeg)
		a.Cbz(X6, prows)
		e.rowSeg(base)
		a.LdrImm(X4, base, VCursorOffSel)
		a.Cbz(X4, fnosel)
		a.LdrRegIdx(X5, X4, X5)
		a.Label(fnosel)
		a.LdrImm(X7, X6, VSegOffCols)
		a.MovImm64(X4, int64(in.B*VSegColSize))
		a.AddReg(X7, X7, X4)
		a.LdrImm(X8, X7, VSegColOffPlain)
		a.CmpImm(X8, uint32(in.T))
		a.Bcond(CondE, kind)
		// A cursor reading its row list takes the full read.
		a.Label(prows)
	} else {
		e.unmap(^in.A)
	}

	// The full read, shared by every column of the program (emitSegRead):
	// the cursor in X9, the column in X11, the destination's address in X12
	// and the return address in X13. X10 comes back non-zero where the VDBE
	// must read the cell.
	a.Label(full)
	e.curBase(in.A)
	a.MovImm64(X11, int64(in.B))
	if o := e.off(in.C, 0); o < 4096 {
		a.AddImm(X12, X1, uint32(o))
	} else {
		a.MovImm64(X12, int64(o))
		a.AddReg(X12, X1, X12)
	}
	back := e.vmLabel("sb")
	a.Adr(X13, back)
	e.segRead = "segread"
	a.B(e.segRead)
	a.Label(back)
	a.Cbnz(X10, e.stub(pc))
	if kind != "" {
		// The predicted kind's read, which a plain column of it jumps to.
		a.B(done)
		a.Label(kind)
		e.segArm(in.T, func(rt Reg64, f int) { e.str(rt, in.C, f) }, done, func() { e.branchWB(pc) })
	}

	a.Label(done)
	e.unmap(in.C)
	e.know.wroteAny(in.C, false)
}

// segArmNull is segArm's NULL, a cell its column's bitmap marks.
const segArmNull = -1

// segArm reads a segment cell of kind k, X5 its row, X6 its VSeg and X7 its
// VSegCol, storing each word of the Value with put, then branches to done.
// wb fails the read when the write barrier is on, which a string's pointer
// store needs off. X4, X8, X9 and X10 are scratch.
func (e *vmEmitArm) segArm(k int, put func(rt Reg64, f int), done string, wb func()) {
	a, L := e.a, e.lay
	cell := func() { // X4 = the 8-byte cell at row X5
		a.LdrImm(X4, X7, VSegColOffBlock)
		a.LdrRegIdx(X4, X4, X5)
	}
	writeNum := func(typ byte, ival, fval Reg64) { // a number, no string
		a.MovImm16(X10, uint16(typ))
		put(X10, 0)
		put(ival, L.OffI)
		put(fval, L.OffF)
		for k := range 3 {
			put(xzr, L.OffS+8*k)
		}
		a.B(done)
	}
	switch k {
	case VSegInt:
		cell()
		writeNum(L.TypInt, X4, xzr)
	// A NULL cell is a NULL, as vm_seg_amd64.go's segArm reads it.
	case segArmNull:
		writeNum(L.TypNull, xzr, xzr)
	case VSegFloat:
		cell()
		writeNum(L.TypFloat, xzr, X4)
	case VSegIntReal:
		cell()
		a.Scvtf(0, X4)
		a.FmovToX(X4, 0)
		writeNum(L.TypFloat, xzr, X4)
	// An integral double within 2^53 is the INTEGER, any other the REAL
	// (numericValue). A NaN compares unequal, and a value past int64
	// saturates and fails the round trip or the range.
	case VSegNumeric:
		numReal := e.vmLabel("sr")
		cell()
		a.FmovToD(0, X4)
		a.Fcvtzs(X8, 0)
		a.Scvtf(1, X8)
		a.Fcmp(0, 1)
		a.Bcond(CondNE, numReal)
		a.MovImm64(X10, 1<<53)
		a.AddReg(X10, X8, X10)
		a.MovImm64(X9, 1<<54)
		a.Cmp(X10, X9)
		a.BcondRaw(armHI, numReal)
		writeNum(L.TypInt, X8, xzr)
		a.Label(numReal)
		writeNum(L.TypFloat, xzr, X4)
	case VSegRowid:
		rowids := e.vmLabel("sw")
		a.LdrImm(X10, X6, VSegOffDense)
		a.Cbz(X10, rowids)
		a.LdrImm(X4, X6, VSegOffFirst)
		a.AddReg(X4, X4, X5)
		writeNum(L.TypInt, X4, xzr)
		a.Label(rowids)
		a.LdrImm(X4, X6, VSegOffRowids)
		a.LdrRegIdx(X4, X4, X5)
		writeNum(L.TypInt, X4, xzr)
	// TEXT: the bytes from bound Pos to bound Pos+1 of the heap, shared,
	// which is a pointer store. An empty string points at the heap's start.
	case VSegText:
		wb()
		a.LdrImm(X4, X7, VSegColOffBlock)
		a.LslImm(X10, X5, 2)
		a.AddReg(X4, X4, X10)
		a.LdrWImm(X8, X4, 0)  // from
		a.LdrWImm(X10, X4, 4) // to
		a.SubsReg(X10, X10, X8)
		a.Csel(X8, X8, xzr, CondNE)
		a.LdrImm(X4, X7, VSegColOffHeap)
		a.AddReg(X8, X4, X8)
		a.MovImm16(X4, uint16(L.TypText))
		put(X4, 0)
		put(xzr, L.OffI)
		put(xzr, L.OffF)
		put(X8, L.OffS)
		put(X10, L.OffS+8)
		put(X10, L.OffS+16)
		a.B(done)
	}
}

// emitSegRead emits segColumn's full read, which every column a program reads
// shares, as vm_seg_amd64.go's: column X11 of the cursor at X9, its row Pos,
// into the Value at X12, from the segment's blocks when the engine handed one
// over, else from its row list. It returns through X13 with X10 zero when it
// read the cell, and non-zero, having written nothing, where the VDBE must.
// The destination holds no string (destNoS before the call). X4-X10 are
// scratch; X11-X13 are left as they came.
func (e *vmEmitArm) emitSegRead() {
	a, L := e.a, e.lay
	fail, ok, seg, nonull, noexc, null, nosel, nostr := e.vmLabel("rf"), e.vmLabel("ro"), e.vmLabel("rg"),
		e.vmLabel("rn"), e.vmLabel("rx"), e.vmLabel("ru"), e.vmLabel("rl"), e.vmLabel("rs")
	var at [VSegText + 1]string
	for _, k := range vmSegKinds {
		at[k] = e.vmLabel("rk")
	}
	a.Label(e.segRead)
	a.LdrImm(X10, X9, VCursorOffOk)
	a.Cbz(X10, fail)
	a.CmpImm(X11, 63)
	a.BcondRaw(armHI, fail) // past the mask's 64 columns
	a.LdrImm(X10, X9, VCursorOffMask)
	a.Asrv(X10, X10, X11)
	a.AndImmLow(X10, X10, 1)
	a.Cbz(X10, fail) // the column is not readable
	a.LdrImm(X5, X9, VCursorOffPos)
	a.LdrImm(X10, X9, VCursorOffN)
	a.Cmp(X5, X10)
	a.BcondRaw(armHS, fail) // unsigned: a position before the first row is huge
	a.LdrImm(X6, X9, VCursorOffSeg)
	a.Cbnz(X6, seg)

	// The row list's: the Value copied whole, its string's pointer shared.
	a.MovImm64(X10, sliceHeaderSize)
	a.Mul(X10, X5, X10)
	a.LdrImm(X4, X9, VCursorOffRows)
	a.AddReg(X10, X4, X10)
	a.LdrImm(X4, X10, 8) // the row's length
	a.Cmp(X4, X11)
	a.BcondRaw(armLS, fail) // a row narrower than the column: the VDBE's
	a.LdrImm(X7, X10, 0)
	a.MovImm64(X10, int64(L.Size))
	a.Mul(X10, X11, X10)
	a.AddReg(X7, X7, X10)
	a.LdrImm(X4, X7, L.OffS)
	a.Cbz(X4, nostr)
	a.LdrImm(X10, X0, POffWB)
	a.Cbnz(X10, fail)
	a.Label(nostr)
	for _, off := range []int{0, L.OffI, L.OffF, L.OffS, L.OffS + 8, L.OffS + 16} {
		a.LdrImm(X4, X7, off)
		a.StrImm(X4, X12, off)
	}
	a.B(ok)

	a.Label(seg)
	e.rowSeg(X9)
	a.LdrImm(X4, X9, VCursorOffSel) // a selection's row for the position
	a.Cbz(X4, nosel)
	a.LdrRegIdx(X5, X4, X5)
	a.Label(nosel)
	a.LdrImm(X7, X6, VSegOffCols)
	a.MovImm64(X4, VSegColSize)
	a.Mul(X4, X11, X4)
	a.AddReg(X7, X7, X4)
	a.LdrImm(X4, X7, VSegColOffNulls)
	a.Cbz(X4, nonull)
	a.LsrImm(X8, X5, 3)
	a.LdrbReg(X8, X4, X8)
	a.AndImmLow(X10, X5, 3)
	a.Asrv(X8, X8, X10)
	a.AndImmLow(X8, X8, 1)
	a.Cbnz(X8, null)
	a.Label(nonull)
	a.LdrImm(X4, X7, VSegColOffExc)
	a.Cbz(X4, noexc)
	a.LsrImm(X8, X5, 6)
	a.LdrRegIdx(X8, X4, X8)
	a.Asrv(X8, X8, X5) // the amount is taken modulo 64: the row's bit
	a.AndImmLow(X8, X8, 1)
	a.Cbnz(X8, fail) // an exception: the VDBE's
	a.Label(noexc)
	a.LdrImm(X8, X7, VSegColOffKind)
	for _, k := range vmSegKinds {
		a.CmpImm(X8, uint32(k))
		a.Bcond(CondE, at[k])
	}
	a.B(fail) // a kind this build does not read
	put := func(rt Reg64, f int) { a.StrImm(rt, X12, f) }
	wb := func() {
		a.LdrImm(X10, X0, POffWB)
		a.Cbnz(X10, fail)
	}
	for _, k := range vmSegKinds {
		a.Label(at[k])
		e.segArm(k, put, ok, wb)
	}
	a.Label(null)
	e.segArm(segArmNull, put, ok, wb)

	a.Label(ok)
	a.MovImm16(X10, 0)
	a.Br(X13)
	a.Label(fail)
	a.MovImm16(X10, 1)
	a.Br(X13)
}

// seekRowid is vm_seg_amd64.go's VSeekRowid. X9 the cursor, X4 the key, X5
// the VTabSeg and X6 the segments left; X7 the VSeg and X8 the row found.
func (e *vmEmitArm) seekRowid(pc int, in VInsn) {
	a, L := e.a, e.lay
	e.unmap(^in.A)
	loop, search, bloop, bhi, bdone, next, found, miss, done :=
		e.vmLabel("kl"), e.vmLabel("ks"), e.vmLabel("kb"), e.vmLabel("kh"), e.vmLabel("kd"),
		e.vmLabel("kn"), e.vmLabel("kf"), e.vmLabel("km"), e.vmLabel("ke")
	e.ldrb(X10, in.B, L.OffTyp)
	a.CmpImm(X10, uint32(L.TypInt))
	a.Bcond(CondNE, e.stub(pc)) // seekRowidKey's conversions are the VDBE's
	e.branchWB(pc)              // the cursor's segment pointers are stored
	e.ldr(X4, in.B, L.OffI)
	e.curBase(in.A)
	a.LdrImm(X10, X9, VCursorOffTab)
	a.Cbz(X10, e.stub(pc))
	a.LdrImm(X5, X10, VTabOffSegs)
	a.LdrImm(X6, X10, VTabOffN)

	a.Label(loop)
	a.Cbz(X6, miss)
	a.LdrImm(X10, X5, VTabSegOffLo)
	a.Cmp(X4, X10)
	a.Bcond(CondL, next)
	a.LdrImm(X10, X5, VTabSegOffHi)
	a.Cmp(X4, X10)
	a.Bcond(CondG, next)
	a.LdrImm(X7, X5, VTabSegOffSeg)
	a.LdrImm(X10, X7, VSegOffDense)
	a.Cbz(X10, search)
	a.LdrImm(X10, X7, VSegOffFirst)
	a.SubReg(X8, X4, X10)
	a.B(found)

	// The rowid block ascends, compared signed: X8 lo, X13 hi, X12 mid.
	a.Label(search)
	a.LdrImm(X10, X7, VSegOffRowids)
	a.MovImm16(X8, 0)
	a.LdrImm(X13, X5, VTabSegOffRows)
	a.Label(bloop)
	a.Cmp(X8, X13)
	a.Bcond(CondGE, bdone)
	a.AddReg(X12, X8, X13)
	a.AsrImm(X12, X12, 1)
	a.LdrRegIdx(X11, X10, X12)
	a.Cmp(X11, X4)
	a.Bcond(CondGE, bhi)
	a.AddImm(X8, X12, 1)
	a.B(bloop)
	a.Label(bhi)
	a.MovReg(X13, X12)
	a.B(bloop)
	a.Label(bdone)
	a.LdrImm(X11, X5, VTabSegOffRows)
	a.Cmp(X8, X11)
	a.Bcond(CondGE, next)
	a.LdrRegIdx(X11, X10, X8)
	a.Cmp(X11, X4)
	a.Bcond(CondNE, next)
	a.B(found)

	a.Label(next)
	a.AddImm(X5, X5, VTabSegSize)
	a.SubsImm(X6, X6, 1)
	a.B(loop)

	// The cursor on that one row, as a point seek leaves it.
	a.Label(found)
	a.StrImm(X7, X9, VCursorOffSeg)
	a.StrImm(X4, X9, VCursorOffKey)
	a.LdrImm(X10, X7, VSegOffRowids)
	a.StrImm(X10, X9, VCursorOffRids)
	a.LdrImm(X10, X5, VTabSegOffMask)
	a.StrImm(X10, X9, VCursorOffMask)
	a.StrImm(xzr, X9, VCursorOffSel)
	a.StrImm(xzr, X9, VCursorOffSegs)
	a.StrImm(X8, X9, VCursorOffPos)
	a.StrImm(X8, X9, VCursorOffLast)
	a.AddImm(X10, X8, 1)
	a.StrImm(X10, X9, VCursorOffN)
	a.MovImm16(X10, 1)
	a.StrImm(X10, X9, VCursorOffOk)
	a.StrImm(X10, X9, VCursorOffDirty)
	a.StrImm(X10, X9, VCursorOffAdv)
	a.MovImm16(X10, 2)
	a.StrImm(X10, X9, VCursorOffState)
	a.LdrImm(X10, X9, VCursorOffSrvd)
	a.AddImm(X10, X10, 1)
	a.StrImm(X10, X9, VCursorOffSrvd)
	a.LdrImm(X10, X9, VCursorOffSteps)
	a.AddImm(X10, X10, 1)
	a.StrImm(X10, X9, VCursorOffSteps)
	a.B(done)

	// No row: the cursor on none, and on to T.
	a.Label(miss)
	a.StrImm(X4, X9, VCursorOffKey)
	a.StrImm(xzr, X9, VCursorOffN)
	a.StrImm(xzr, X9, VCursorOffPos)
	a.StrImm(xzr, X9, VCursorOffSegs)
	a.MovImm16(X10, 1)
	a.StrImm(X10, X9, VCursorOffOk)
	a.StrImm(X10, X9, VCursorOffDirty)
	a.MovImm16(X10, 3)
	a.StrImm(X10, X9, VCursorOffState)
	e.jmpBlock(pc, in.T)
	a.Label(done)
}

// seekIdx is vm_seg_amd64.go's VSeekIdx. X9 the cursor, X4 the key, X5 the
// segment's index and X6 the indexes left, X7 its values; X8 lo, X13 hi and
// X12 mid of the search.
func (e *vmEmitArm) seekIdx(pc int, in VInsn) {
	a, L := e.a, e.lay
	e.unmap(^in.A)
	seg, bloop, bhi, bdone, rest, next, found, miss, done := e.vmLabel("is"), e.vmLabel("ib"), e.vmLabel("ih"), e.vmLabel("id"),
		e.vmLabel("ir"), e.vmLabel("in"), e.vmLabel("if"), e.vmLabel("im"), e.vmLabel("ie")
	fserved, fskip, mserved, mskip := e.vmLabel("iv"), e.vmLabel("iw"), e.vmLabel("ix"), e.vmLabel("iy")
	e.ldrb(X10, in.B, L.OffTyp)
	a.CmpImm(X10, uint32(L.TypInt))
	a.Bcond(CondNE, e.stub(pc)) // the key's conversions are the VDBE's
	e.branchWB(pc)              // the cursor's segment pointers are stored
	e.ldr(X4, in.B, L.OffI)
	e.curBase(in.A)
	a.LdrImm(X5, X9, VCursorOffIdx)
	a.Cbz(X5, e.stub(pc))
	a.LdrImm(X6, X5, VIdxOffCount)

	a.Label(seg)
	a.Cbz(X6, miss)
	a.LdrImm(X10, X5, VIdxOffLo)
	a.Cmp(X4, X10)
	a.Bcond(CondL, next)
	a.LdrImm(X10, X5, VIdxOffHi)
	a.Cmp(X4, X10)
	a.Bcond(CondG, next)
	a.LdrImm(X7, X5, VIdxOffVals)
	a.MovImm16(X8, 0)
	a.LdrImm(X13, X5, VIdxOffNVals)
	a.Label(bloop)
	a.Cmp(X8, X13)
	a.Bcond(CondGE, bdone)
	a.AddReg(X12, X8, X13)
	a.AsrImm(X12, X12, 1)
	a.LdrRegIdx(X11, X7, X12)
	a.Cmp(X11, X4)
	a.Bcond(CondGE, bhi)
	a.AddImm(X8, X12, 1)
	a.B(bloop)
	a.Label(bhi)
	a.MovReg(X13, X12)
	a.B(bloop)
	a.Label(bdone)
	a.LdrImm(X11, X5, VIdxOffNVals)
	a.Cmp(X8, X11)
	a.Bcond(CondGE, next)
	a.LdrRegIdx(X11, X7, X8)
	a.Cmp(X11, X4)
	a.Bcond(CondNE, next)

	// Found value X8 of segment X5: a later segment whose range holds the key
	// may hold some of its rows, which the VDBE gathers.
	a.MovReg(X13, X6)
	a.MovReg(X12, X5)
	a.Label(rest)
	a.SubsImm(X13, X13, 1)
	a.Bcond(CondE, found)
	a.AddImm(X12, X12, VIdxSize)
	a.LdrImm(X10, X12, VIdxOffLo)
	a.Cmp(X4, X10)
	a.Bcond(CondL, rest)
	a.LdrImm(X10, X12, VIdxOffHi)
	a.Cmp(X4, X10)
	a.Bcond(CondLE, e.stub(pc))
	a.B(rest)

	a.Label(next)
	a.AddImm(X5, X5, VIdxSize)
	a.SubsImm(X6, X6, 1)
	a.B(seg)

	// Its rows are Rows[Starts[X8]:Starts[X8+1]].
	a.Label(found)
	e.seekIdxServed(fserved, fskip)
	a.StrImm(X4, X9, VCursorOffKey)
	a.LdrImm(X10, X5, VIdxOffStarts)
	a.LdrswRegLSL2(X11, X10, X8)
	a.AddImm(X8, X8, 1)
	a.LdrswRegLSL2(X12, X10, X8)
	a.SubReg(X12, X12, X11)
	a.StrImm(X12, X9, VCursorOffN)
	a.LdrImm(X10, X5, VIdxOffRows)
	a.AddRegLSL3(X10, X10, X11)
	a.StrImm(X10, X9, VCursorOffSel)
	fsegs := e.vmLabel("iz")
	a.LdrImm(X10, X5, VIdxOffSegs)
	a.Cbz(X10, fsegs)
	a.AddRegLSL3(X10, X10, X11)
	a.Label(fsegs)
	a.StrImm(X10, X9, VCursorOffSegs)
	a.LdrImm(X10, X5, VIdxOffSeg)
	a.StrImm(X10, X9, VCursorOffSeg)
	a.LdrImm(X10, X10, VSegOffRowids)
	a.StrImm(X10, X9, VCursorOffRids)
	a.LdrImm(X10, X5, VIdxOffMask)
	a.StrImm(X10, X9, VCursorOffMask)
	a.StrImm(xzr, X9, VCursorOffPos)
	a.StrImm(xzr, X9, VCursorOffLast)
	a.MovImm16(X10, 1)
	a.StrImm(X10, X9, VCursorOffOk)
	a.StrImm(X10, X9, VCursorOffDirty)
	a.StrImm(X10, X9, VCursorOffAdv)
	a.MovImm16(X10, 2)
	a.StrImm(X10, X9, VCursorOffState)
	a.LdrImm(X10, X9, VCursorOffSteps)
	a.AddImm(X10, X10, 1)
	a.StrImm(X10, X9, VCursorOffSteps)
	a.B(done)

	// No row: the cursor on none, and on to T.
	a.Label(miss)
	e.seekIdxServed(mserved, mskip)
	a.StrImm(X4, X9, VCursorOffKey)
	a.StrImm(xzr, X9, VCursorOffN)
	a.StrImm(xzr, X9, VCursorOffPos)
	a.StrImm(xzr, X9, VCursorOffSegs)
	a.MovImm16(X10, 1)
	a.StrImm(X10, X9, VCursorOffOk)
	a.StrImm(X10, X9, VCursorOffDirty)
	a.MovImm16(X10, 3)
	a.StrImm(X10, X9, VCursorOffState)
	e.jmpBlock(pc, in.T)
	a.Label(done)
}

// seekIdxServed adds to the cursor in X9's Served the rows its last seek's run
// served, up to the row it is on, when it has one (State 2, 4 or 5). X10 and
// X11 are scratch.
func (e *vmEmitArm) seekIdxServed(served, skip string) {
	a := e.a
	a.LdrImm(X10, X9, VCursorOffState)
	a.CmpImm(X10, 2)
	a.Bcond(CondE, served)
	a.CmpImm(X10, 4)
	a.Bcond(CondE, served)
	a.CmpImm(X10, 5)
	a.Bcond(CondNE, skip)
	a.Label(served)
	a.LdrImm(X10, X9, VCursorOffLast)
	a.AddImm(X10, X10, 1)
	a.LdrImm(X11, X9, VCursorOffSrvd)
	a.AddReg(X10, X10, X11)
	a.StrImm(X10, X9, VCursorOffSrvd)
	a.Label(skip)
}

// seekRewind is vm_seg_amd64.go's VSeekRewind.
func (e *vmEmitArm) seekRewind(pc int, in VInsn) {
	a := e.a
	e.unmap(^in.A)
	e.curBase(in.A)
	found := e.vmLabel("rf")
	a.LdrImm(X10, X9, VCursorOffState)
	a.CmpImm(X10, 2)
	a.Bcond(CondE, found)
	a.CmpImm(X10, 3)
	a.Bcond(CondNE, e.stub(pc))
	e.jmpBlock(pc, in.T)
	a.Label(found)
	a.MovImm16(X10, 4)
	a.StrImm(X10, X9, VCursorOffState)
}

// rowSeg sets X6 to the VSeg of the row at position X5 of the cursor at cur
// when its Segs lists one per position, leaving X6 its Seg otherwise. X4 is
// scratch.
func (e *vmEmitArm) rowSeg(cur Reg64) {
	a := e.a
	skip := e.vmLabel("sy")
	a.LdrImm(X4, cur, VCursorOffSegs)
	a.Cbz(X4, skip)
	a.LdrRegIdx(X6, X4, X5)
	a.Label(skip)
}

// nullRow is vm_seg_amd64.go's VNullRow.
func (e *vmEmitArm) nullRow(pc int, in VInsn) {
	a := e.a
	e.unmap(^in.A)
	e.branchWB(pc)
	e.curBase(in.A)
	a.MovImm64(X10, nullSegAddr())
	a.StrImm(X10, X9, VCursorOffSeg)
	for _, f := range []struct {
		off int
		v   int64
	}{{VCursorOffSel, 0}, {VCursorOffSegs, 0}, {VCursorOffRids, 0}, {VCursorOffN, 1}, {VCursorOffPos, 0}, {VCursorOffLast, 0}, {VCursorOffOk, 1},
		{VCursorOffDirty, 1}, {VCursorOffAdv, 0}, {VCursorOffRew, 0}, {VCursorOffMask, -1}, {VCursorOffState, 6}} {
		a.MovImm64(X10, f.v)
		a.StrImm(X10, X9, f.off)
	}
}

// text is VText: a pointer store, so the write barrier exits.
func (e *vmEmitArm) text(pc int, in VInsn) {
	if in.Dead {
		e.clobber(in.A)
		return
	}
	a, L := e.a, e.lay
	// Left as it is when r[A] already holds this TEXT, as from the last row on.
	same, store := e.vmLabel("ts"), e.vmLabel("tw")
	a.MovImm64(X9, in.Imm)
	e.ldr(X10, in.A, L.OffS)
	a.Cmp(X9, X10)
	a.Bcond(CondNE, store)
	e.ldrb(X10, in.A, L.OffTyp)
	a.CmpImm(X10, uint32(L.TypText))
	a.Bcond(CondNE, store)
	e.ldrb(X10, in.A, L.OffTyp+1) // no subtype
	a.Cbnz(X10, store)
	a.MovImm64(X9, int64(in.B))
	e.ldr(X10, in.A, L.OffS+8)
	a.Cmp(X9, X10)
	a.Bcond(CondE, same)
	a.Label(store)
	e.branchWB(pc)
	e.zeroS.del(in.A)
	a.MovImm16(X9, uint16(L.TypText))
	e.str(X9, in.A, 0)
	e.str(xzr, in.A, L.OffI)
	e.str(xzr, in.A, L.OffF)
	a.MovImm64(X9, in.Imm)
	e.str(X9, in.A, L.OffS)
	a.MovImm64(X9, int64(in.B))
	e.str(X9, in.A, L.OffS+8)
	e.str(X9, in.A, L.OffS+16)
	a.Label(same)
	e.unmap(in.A)
	e.know.wroteAny(in.A, false)
}

// orderedText leaves the flags as a signed compare of r[B]'s TEXT with
// r[A]'s would: their bytes up to the shorter's length (zero-extended, so a
// signed compare orders them unsigned), then their lengths, as bytes.Compare
// orders them. X4 and X5 hold r[B]'s and r[A]'s lengths, X7 and X8 their
// bytes. It goes on to tail.
func (e *vmEmitArm) orderedText(tail string) {
	a := e.a
	loop, lens := e.vmLabel("ol"), e.vmLabel("on")
	a.Cmp(X4, X5)
	a.Csel(X6, X5, X4, CondG) // the shorter length
	a.Label(loop)
	a.Cbz(X6, lens)
	a.LdrbImm(X9, X7, 0)
	a.LdrbImm(X10, X8, 0)
	a.Cmp(X9, X10)
	a.Bcond(CondNE, tail)
	a.AddImm(X7, X7, 1)
	a.AddImm(X8, X8, 1)
	a.SubsImm(X6, X6, 1)
	a.B(loop)
	a.Label(lens)
	a.Cmp(X4, X5)
	a.B(tail)
}

// numericTextExit is vm_text_amd64.go's: it exits at pc when r[r]'s TEXT is a
// whole number under numeric affinity (parseFullNumeric). X7 the next byte,
// X4 the bytes left, X8 that byte, X5 and X6 the digits before and after the
// point; X10 is scratch.
func (e *vmEmitArm) numericTextExit(pc, r int) {
	a, L := e.a, e.lay
	var lb [16]string
	for k := range lb {
		lb[k] = e.vmLabel("nt")
	}
	ws, wsAdv, sign, signAdv, d1, d1Loop, frac, d2, expo, eSign, eDigit, eLoop, trail, trailAdv, num, text :=
		lb[0], lb[1], lb[2], lb[3], lb[4], lb[5], lb[6], lb[7], lb[8], lb[9], lb[10], lb[11], lb[12], lb[13], lb[14], lb[15]
	next := func() { a.AddImm(X7, X7, 1); a.SubsImm(X4, X4, 1) }
	load := func(end string) { // X8 = the next byte, or on to end at the end
		a.Cbz(X4, end)
		a.LdrbImm(X8, X7, 0)
	}
	isSpace := func(yes string) { // ' ', or '\t' to '\r'
		a.CmpImm(X8, ' ')
		a.Bcond(CondE, yes)
		a.SubsImm(X10, X8, 9)
		a.CmpImm(X10, 4)
		a.BcondRaw(armLS, yes)
	}
	notDigit := func(no string) {
		a.SubsImm(X10, X8, '0')
		a.CmpImm(X10, 9)
		a.BcondRaw(armHI, no)
	}
	e.ldr(X7, r, L.OffS)
	e.ldr(X4, r, L.OffS+8)
	a.Label(ws)
	load(text)
	isSpace(wsAdv)
	a.B(sign)
	a.Label(wsAdv)
	next()
	a.B(ws)
	a.Label(sign)
	a.CmpImm(X8, '+')
	a.Bcond(CondE, signAdv)
	a.CmpImm(X8, '-')
	a.Bcond(CondNE, d1)
	a.Label(signAdv)
	next()
	a.Label(d1)
	a.MovImm16(X5, 0)
	a.Label(d1Loop)
	load(frac)
	notDigit(frac)
	next()
	a.AddImm(X5, X5, 1)
	a.B(d1Loop)
	a.Label(frac)
	a.MovImm16(X6, 0)
	load(expo)
	a.CmpImm(X8, '.')
	a.Bcond(CondNE, expo)
	next()
	a.Label(d2)
	load(expo)
	notDigit(expo)
	next()
	a.AddImm(X6, X6, 1)
	a.B(d2)
	a.Label(expo)
	a.OrrReg(X10, X5, X6)
	a.Cbz(X10, text) // no digit at all: no number
	// An exponent counts only with a digit, so it is looked at ahead of
	// moving: X5 the byte after the 'e', X6 the bytes left from there.
	load(trail)
	a.MovImm16(X10, 0x20)
	a.OrrReg(X8, X8, X10)
	a.CmpImm(X8, 'e')
	a.Bcond(CondNE, trail)
	a.AddImm(X5, X7, 1)
	a.SubsImm(X6, X4, 1)
	a.Bcond(CondE, trail)
	a.LdrbImm(X8, X5, 0)
	a.CmpImm(X8, '+')
	a.Bcond(CondE, eSign)
	a.CmpImm(X8, '-')
	a.Bcond(CondNE, eDigit)
	a.Label(eSign)
	a.AddImm(X5, X5, 1)
	a.SubsImm(X6, X6, 1)
	a.Bcond(CondE, trail)
	a.LdrbImm(X8, X5, 0)
	a.Label(eDigit)
	a.SubsImm(X10, X8, '0')
	a.CmpImm(X10, 9)
	a.BcondRaw(armHI, trail) // no digit: the 'e' is not the number's
	a.MovReg(X7, X5)
	a.MovReg(X4, X6)
	a.Label(eLoop)
	load(trail)
	notDigit(trail)
	next()
	a.B(eLoop)
	a.Label(trail)
	load(num)
	isSpace(trailAdv)
	a.B(text) // something after the number: TEXT
	a.Label(trailAdv)
	next()
	a.B(trail)
	a.Label(num)
	a.B(e.stub(pc))
	a.Label(text)
}

// inSub is vm_seg_amd64.go's VInSub: X9 the set, X4 its keys, X5 the key,
// X6 and X7 the bounds, X8 the middle.
func (e *vmEmitArm) inSub(pc int, in VInsn) {
	if in.Dead {
		e.clobber(in.C)
		return
	}
	a, L := e.a, e.lay
	e.destNoS(pc, in.C)
	loop, hi, miss, absent, yes, no, done := e.vmLabel("ql"), e.vmLabel("qh"), e.vmLabel("qm"), e.vmLabel("qa"),
		e.vmLabel("qy"), e.vmLabel("qn"), e.vmLabel("qd")
	a.LdrImm(X9, X0, POffInSets)
	if in.B > 0 {
		a.MovImm64(X10, int64(in.B*VInSubSize))
		a.AddReg(X9, X9, X10)
	}
	a.LdrImm(X10, X9, VInSubOffReady)
	a.Cbz(X10, e.stub(pc))
	a.LdrImm(X10, X9, VInSubOffEmpty)
	a.Cbnz(X10, no)
	e.ldrb(X10, in.A, L.OffTyp)
	a.CmpImm(X10, uint32(L.TypInt))
	a.Bcond(CondNE, e.stub(pc))
	e.ldr(X5, in.A, L.OffI)
	a.LdrImm(X4, X9, VInSubOffKeys)
	a.MovImm16(X6, 0)
	a.LdrImm(X7, X9, VInSubOffN)
	a.Label(loop)
	a.Cmp(X6, X7)
	a.Bcond(CondGE, miss)
	a.AddReg(X8, X6, X7)
	a.AsrImm(X8, X8, 1)
	a.LdrRegIdx(X10, X4, X8)
	a.Cmp(X10, X5)
	a.Bcond(CondGE, hi)
	a.AddImm(X6, X8, 1)
	a.B(loop)
	a.Label(hi)
	a.MovReg(X7, X8)
	a.B(loop)
	a.Label(miss)
	a.LdrImm(X10, X9, VInSubOffN)
	a.Cmp(X6, X10)
	a.Bcond(CondGE, absent)
	a.LdrRegIdx(X10, X4, X6)
	a.Cmp(X10, X5)
	a.Bcond(CondE, yes)
	a.Label(absent)
	a.LdrImm(X10, X9, VInSubOffHasNull)
	a.Cbnz(X10, e.stub(pc)) // NULL, which the VDBE answers
	a.B(no)
	in1, in0 := uint16(1), uint16(0)
	if in.Imm == 1 {
		in1, in0 = 0, 1
	}
	a.Label(yes)
	a.MovImm16(X8, in1)
	a.B(done)
	a.Label(no)
	a.MovImm16(X8, in0)
	a.Label(done)
	m := e.dest(pc, in.C, X8)
	if m != X8 {
		a.MovReg(m, X8)
	}
	e.storeInt(in, in.C, m)
}

// inText is vm_text_amd64.go's VInText. X4 r[A]'s length and X5 its bytes, X6
// the list entry and X7 the entries left, X8 and X11 the byte pointers, X12 the
// bytes left; X9 and X10 the two bytes.
func (e *vmEmitArm) inText(pc int, in VInsn) {
	if in.Dead {
		e.clobber(in.C)
		return
	}
	a, L := e.a, e.lay
	e.destNoS(pc, in.C)
	ent, cmpb, next, yes, no, done := e.vmLabel("it"), e.vmLabel("ib"), e.vmLabel("in"), e.vmLabel("iy"), e.vmLabel("io"), e.vmLabel("ie")
	e.ldrb(X10, in.A, L.OffTyp)
	a.CmpImm(X10, uint32(L.TypText))
	a.Bcond(CondNE, e.stub(pc))
	a.LdrImm(X10, X0, POffUTF16) // the engine compares UTF-16 TEXT
	a.Cbnz(X10, e.stub(pc))
	e.ldr(X4, in.A, L.OffS+8)
	e.ldr(X5, in.A, L.OffS)
	a.MovImm64(X6, in.Imm)
	a.MovImm64(X7, int64(in.B))
	a.Label(ent)
	a.Cbz(X7, no)
	a.SubsImm(X7, X7, 1)
	a.LdrImm(X12, X6, 8)
	a.Cmp(X12, X4)
	a.Bcond(CondNE, next)
	a.LdrImm(X8, X6, 0)
	a.MovReg(X11, X5)
	a.Label(cmpb)
	a.Cbz(X12, yes)
	a.LdrbImm(X9, X8, 0)
	a.LdrbImm(X10, X11, 0)
	a.Cmp(X9, X10)
	a.Bcond(CondNE, next)
	a.AddImm(X8, X8, 1)
	a.AddImm(X11, X11, 1)
	a.SubsImm(X12, X12, 1)
	a.B(cmpb)
	a.Label(next)
	a.AddImm(X6, X6, 16)
	a.B(ent)
	a.Label(yes)
	a.MovImm16(X8, 1)
	a.B(done)
	a.Label(no)
	a.MovImm16(X8, 0)
	a.Label(done)
	m := e.dest(pc, in.C, X8)
	if m != X8 {
		a.MovReg(m, X8)
	}
	e.storeInt(in, in.C, m)
}

// sortSeek is vm_seg_amd64.go's VSortSeek. X9 the cursor, X4 the key, X5 its
// Sort, X7 the keys; X8 lo, X13 hi and X12 mid of the search.
func (e *vmEmitArm) sortSeek(pc int, in VInsn) {
	a, L := e.a, e.lay
	e.unmap(^in.A)
	bloop, bhi, bdone, run, end := e.vmLabel("ss"), e.vmLabel("sh"), e.vmLabel("sd"), e.vmLabel("sr"), e.vmLabel("se")
	e.ldrb(X10, in.B, L.OffTyp)
	a.CmpImm(X10, uint32(L.TypInt))
	a.Bcond(CondNE, e.stub(pc)) // the key's conversions are the VDBE's
	e.branchWB(pc)              // the cursor's row pointers are stored
	e.ldr(X4, in.B, L.OffI)
	e.curBase(in.A)
	a.LdrImm(X5, X9, VCursorOffSort)
	a.Cbz(X5, e.stub(pc))
	a.LdrImm(X7, X5, VSortOffInts)
	a.MovImm16(X8, 0)
	a.LdrImm(X13, X5, VSortOffN)
	a.Label(bloop)
	a.Cmp(X8, X13)
	a.Bcond(CondGE, bdone)
	a.AddReg(X12, X8, X13)
	a.AsrImm(X12, X12, 1)
	a.LdrRegIdx(X11, X7, X12)
	a.Cmp(X11, X4)
	a.Bcond(CondGE, bhi)
	a.AddImm(X8, X12, 1)
	a.B(bloop)
	a.Label(bhi)
	a.MovReg(X13, X12)
	a.B(bloop)
	a.Label(bdone)

	// The run: X8 to X13.
	a.MovReg(X13, X8)
	a.LdrImm(X6, X5, VSortOffN)
	a.Label(run)
	a.Cmp(X13, X6)
	a.Bcond(CondGE, end)
	a.LdrRegIdx(X11, X7, X13)
	a.Cmp(X11, X4)
	a.Bcond(CondNE, end)
	a.AddImm(X13, X13, 1)
	a.B(run)
	a.Label(end)

	a.SubReg(X13, X13, X8)
	a.StrImm(X13, X9, VCursorOffN)
	a.StrImm(X8, X9, VCursorOffSLo)
	a.CmpImm(X13, 0)
	a.Csel(X8, X8, xzr, CondNE) // an empty run's pointers are the lists' starts (amd64's)
	a.LdrImm(X10, X5, VSortOffIds)
	a.AddRegLSL3(X10, X10, X8)
	a.StrImm(X10, X9, VCursorOffRids)
	a.AddReg(X12, X8, X8)
	a.AddReg(X12, X12, X8) // 3*lo words: 24-byte headers
	a.LdrImm(X10, X5, VSortOffRows)
	a.AddRegLSL3(X10, X10, X12)
	a.StrImm(X10, X9, VCursorOffRows)
	a.MovImm64(X10, -1)
	a.StrImm(X10, X9, VCursorOffLast)
	a.MovImm16(X10, 1)
	a.StrImm(X10, X9, VCursorOffDirty)
}
