package jit

import (
	"math/bits"
)

// condCarry and condParity are x86's JB (a BT that found the bit set) and JP
// (an unordered UCOMISD: a NaN).
const (
	condCarry  Cond = 0x2
	condParity Cond = 0xA
)

// BtRegReg is  CF = bit (bit mod 64) of dst.
func (a *Asm) BtRegReg(dst, bit Reg) {
	a.rex(bit, dst)
	a.emit(0x0F, 0xA3, modrm(0b11, bit, dst))
}

// MovRegMemD32 is  dst = the uint32 at [base+disp], zero-extended.
func (a *Asm) MovRegMemD32(dst, base Reg, disp int32) {
	if r := byte(0x40) | (byte(dst)>>3)<<2 | byte(base)>>3; r != 0x40 {
		a.emit(r)
	}
	a.emit(0x8B)
	a.memD(dst, base, disp)
}

// segColumn is a VColumn marked segment-capable (Imm 1): column B of cursor
// A's row Pos, read from the cursor's segment's column blocks when the engine
// handed it one (VCursor.Seg), else from its row list. No row is kept in a
// register across columns: the two kinds of cursor share this code. RAX, RCX,
// RDX, R9, R10, X0 and X1 are scratch.
//
// A segment cell reads exactly as segColumnValue reads it, or exits: an
// exception exits, and so does a kind the engine left out of the Mask.
//
// With a predicted kind (T) a run's reads of one cursor share their checks:
// the first checks the cursor, every column the run reads of it (the Mask)
// and its position, and keeps the cursor's address in a register
// (holds[^A]); each read then tests only that its column is plain (no NULL
// bitmap, no exceptions) and of the kind predicted, and goes straight to that
// kind's read. Anything else takes the full read below.
// rowSeg sets R9 to the VSeg of the row at position RDX of the cursor at cur
// when its Segs lists one per position, leaving R9 its Seg otherwise. RCX is
// scratch.
func (e *vmEmit) rowSeg(cur Reg, skip Lbl) {
	a := e.a
	a.MovRegMemD(RCX, cur, VCursorOffSegs)
	a.TestRegReg(RCX, RCX)
	a.JccL(CondE, skip)
	a.MovRegMemIdx(R9, RCX, RDX)
	a.Bind(skip)
}

// rowRead is segColumn's predicted read of a cursor on its row list,
// reached at label prows with base the cursor's address and RDX its
// position. With a register for the row (row, newRow when this read is the
// run's first of the cursor), the first read checks the row as rowAddr does
// and keeps its address there, and every later one reads from it (label
// prow), going on at label done, which follows unless the caller binds it.
// Without one, a read takes the row list's full read.
func (e *vmEmit) rowRead(pc int, in VInsn, base, row Reg, haveRow, newRow bool, checked uint64, l func(string) Lbl) {
	a := e.a
	a.Bind(l("prows"))
	if !haveRow {
		a.JmpL(l("full"))
		return
	}
	if newRow {
		e.rowAddr(pc, in, base, 0, row, checked)
	}
	a.Bind(l("prow"))
	e.rowCopy(pc, in, row)
}

// unpredictedRead is segColumn's read of a column no segment kind is
// predicted for (T 0), which reads a cursor on its row list as column reads
// a plain cursor's: the run's first read checks the cursor and keeps the
// row's address (vmRowKey), and every later one tests only that it has one.
// On a segment the row's register holds 0, and a later read goes the full
// way (label full, which the caller binds next).
func (e *vmEmit) unpredictedRead(pc int, in VInsn, l func(string) Lbl) {
	a := e.a
	cb := int32(in.A * VCursorSize)
	if row, ok := e.holds[vmRowKey(in.A)]; ok {
		e.pinned.add(int(row))
		a.TestRegReg(row, row)
		a.JccL(CondE, l("full"))
		e.rowCopy(pc, in, row)
		a.JmpL(l("done"))
		return
	}
	mask := uint64(1) << uint(in.B)
	if e.cache {
		mask = vmRunPredicted(e.prog, e.bound, pc)
	}
	a.MovRegMemD(RAX, RDI, POffCur)
	a.CmpMemDImm8(RAX, cb+VCursorOffOk, 0)
	e.jccStub(CondE, pc)
	a.MovRegMemD(RDX, RAX, cb+VCursorOffMask)
	a.MovRegImm64(R10, int64(mask))
	a.AndRegReg(RDX, R10)
	a.CmpRegReg(RDX, R10)
	e.jccStub(CondNE, pc)
	a.MovRegMemD(RDX, RAX, cb+VCursorOffPos)
	a.AluRegMemD(AluCmp, RDX, RAX, cb+VCursorOffN)
	e.jccStub(condAboveEq, pc) // unsigned: a position before the first row is huge
	row, held := R9, false
	if u := e.nextUse(vmRowKey(in.A), pc); u >= 0 {
		if m, ok := e.alloc(pc, u, true); ok {
			row, held = m, true
			e.pinned.add(int(m))
		}
	}
	a.CmpMemDImm8(RAX, cb+VCursorOffSeg, 0)
	a.JccL(CondNE, l("useg"))
	e.rowAddr(pc, in, RAX, cb, row, mask)
	e.rowCopy(pc, in, row)
	a.JmpL(l("done"))
	// On a segment: the full read (label full, which the caller binds next).
	a.Bind(l("useg"))
	if held {
		a.XorRegReg(row, row)
		e.holds[vmRowKey(in.A)] = row
	}
	a.JmpL(l("full"))
}

// rowAddr sets row to the address of the row at position RDX of the cursor at
// base+cb, first checking, as column does for a plain cursor, that every
// column the run reads of the cursor is readable (those of checked already
// were) and within the row.
func (e *vmEmit) rowAddr(pc int, in VInsn, base Reg, cb int32, row Reg, checked uint64) {
	a := e.a
	all := uint64(1) << uint(in.B)
	if e.cache {
		all = vmRunSegReads(e.prog, e.bound, pc, false)
	}
	if extra := all &^ checked; extra != 0 {
		a.MovRegMemD(R10, base, cb+VCursorOffMask)
		a.MovRegImm64(R9, int64(extra))
		a.AndRegReg(R10, R9)
		a.CmpRegReg(R10, R9)
		e.jccStub(CondNE, pc)
	}
	a.MovRegImm64(R10, sliceHeaderSize)
	a.ImulRegReg(RDX, R10)
	a.AddRegMemD(RDX, base, cb+VCursorOffRows)
	a.MovRegMemD(R10, RDX, 8) // the row's length
	a.CmpRegImm32(R10, int32(bits.Len64(all)-1))
	e.jccStub(condBelowEq, pc) // a row narrower than a column: the VDBE's
	a.MovRegMemD(row, RDX, 0)
}

// rowCopy copies column B of the row at row into r[C] as column does: the
// string words only when the column or the destination holds a string, with
// the write barrier off.
func (e *vmEmit) rowCopy(pc int, in VInsn, row Reg) {
	a, L := e.a, e.lay
	dst := e.d(in.C, 0)
	vb := int32(in.B) * int32(L.Size)
	a.MovRegMemD(RDX, row, vb+int32(L.OffS))
	if !e.know.noS.has(in.C) {
		a.AluRegMemD(AluOr, RDX, RSI, dst+int32(L.OffS))
	} else {
		a.TestRegReg(RDX, RDX)
	}
	e.coldPath(func() {
		e.jccWB(pc)
		for _, off := range []int{L.OffS, L.OffS + 8, L.OffS + 16} {
			a.MovRegMemD(RDX, row, vb+int32(off))
			a.MovMemDReg(RSI, dst+int32(off), RDX)
		}
	})
	for _, off := range []int{0, L.OffI, L.OffF} {
		a.MovRegMemD(RDX, row, vb+int32(off))
		a.MovMemDReg(RSI, dst+int32(off), RDX)
	}
}

// segKindLabel is the label of the predicted read of a cell of kind k, ""
// for a kind with none.
func segKindLabel(k int) string {
	switch k {
	case VSegInt:
		return "int"
	case VSegFloat:
		return "float"
	case VSegNumeric:
		return "num"
	case VSegIntReal:
		return "intreal"
	case VSegRowid:
		return "rowid"
	case VSegText:
		return "text"
	}
	return ""
}

func (e *vmEmit) segColumn(pc int, in VInsn) {
	a, L := e.a, e.lay
	cb := int32(in.A * VCursorSize)
	if in.T != 0 && segKindLabel(in.T) == "" {
		e.destNoS(pc, in.C)
	}
	e.zeroS.del(in.C) // every word of the destination is written below
	dst := e.d(in.C, 0)
	l := e.named()
	var rowsLast func() // the row list's read, emitted to run on into done
	if in.T == 0 {
		e.unpredictedRead(pc, in, l)
		a.Bind(l("full"))
		e.destNoS(pc, in.C)
		e.zeroS.del(in.C)
	} else if name := segKindLabel(in.T); name != "" {
		base, have := e.holds[^in.A]
		row, haveRow := e.holds[vmRowKey(in.A)]
		first, fast, newRow, mask := !have, have && haveRow, false, uint64(1)<<uint(in.B)
		if fast {
			// A later read of the run: row is the row's address on the row
			// list, 0 on a segment.
			e.pinned.add(int(base))
			e.pinned.add(int(row))
			a.TestRegReg(row, row)
			a.JccL(CondNE, l("prow"))
		} else if have {
			e.pinned.add(int(base))
		} else {
			if e.cache {
				mask = vmRunPredicted(e.prog, e.bound, pc)
			}
			a.MovRegMemD(RAX, RDI, POffCur)
			a.CmpMemDImm8(RAX, cb+VCursorOffOk, 0)
			e.jccStub(CondE, pc)
			a.MovRegMemD(RDX, RAX, cb+VCursorOffMask)
			a.MovRegImm64(R10, int64(mask))
			a.AndRegReg(RDX, R10)
			a.CmpRegReg(RDX, R10)
			e.jccStub(CondNE, pc)
			a.MovRegMemD(RDX, RAX, cb+VCursorOffPos)
			a.AluRegMemD(AluCmp, RDX, RAX, cb+VCursorOffN)
			e.jccStub(condAboveEq, pc) // unsigned: a position before the first row is huge
			base = RAX
			if u := e.nextUse(^in.A, pc); u >= 0 {
				if m, ok := e.alloc(pc, u, true); ok {
					base = m
					e.holds[^in.A] = m
					e.pinned.add(int(m))
				}
			}
			a.LeaRegMemD(base, RAX, cb)
		}
		// A cursor native code rewinds is usually on its row list: its
		// segment pointer is only compared, and its segment path loads it.
		rows := e.rewinds.has(in.A)
		if !fast {
			if have {
				a.MovRegMemD(RDX, base, VCursorOffPos) // the check above left it in RDX
			}
			if !rows {
				a.MovRegMemD(R9, base, VCursorOffSeg)
			}
		}
		if !haveRow && first {
			if u := e.nextUse(vmRowKey(in.A), pc); u >= 0 {
				if m, ok := e.alloc(pc, u, true); ok {
					row, haveRow, newRow = m, true, true
					e.pinned.add(int(m))
				}
			}
		}
		if !fast {
			if rows {
				a.CmpMemDImm8(base, VCursorOffSeg, 0)
			} else {
				a.TestRegReg(R9, R9)
			}
			a.JccL(CondE, l("prows"))
		}
		if newRow {
			a.XorRegReg(row, row)
			e.holds[vmRowKey(in.A)] = row
		}
		// On a segment: the stores below clear the destination's string
		// without the write barrier, so it must hold none.
		e.destNoS(pc, in.C)
		e.zeroS.del(in.C)
		if fast {
			a.MovRegMemD(RDX, base, VCursorOffPos)
		}
		if fast || rows {
			a.MovRegMemD(R9, base, VCursorOffSeg)
		}
		e.rowSeg(base, l("fsegs"))
		a.MovRegMemD(R10, base, VCursorOffSel)
		a.TestRegReg(R10, R10)
		a.JccL(CondE, l("fnosel"))
		a.MovRegMemIdx(RDX, R10, RDX)
		a.Bind(l("fnosel"))
		a.MovRegMemD(R10, R9, VSegOffCols)
		a.AddRegImm32(R10, int32(in.B*VSegColSize))
		a.CmpMemDImm8(R10, VSegColOffPlain, int8(in.T))
		a.JccL(CondE, l(name))
		a.JmpL(l("full"))
		read := func() { e.rowRead(pc, in, base, row, haveRow, newRow, mask, l) }
		if rows {
			rowsLast = read
		} else {
			read()
			a.JmpL(l("done"))
		}
		a.Bind(l("full"))
	} else {
		e.unmapCursor(in.A)
	}

	// The full read, shared by every column of the program (segReadAny):
	// the cursor's address in RAX, the column in X3 and the destination's
	// address in X4. It returns ZF clear where the VDBE must read the cell.
	a.MovRegMemD(RAX, RDI, POffCur)
	if cb != 0 {
		a.LeaRegMemD(RAX, RAX, cb)
	}
	a.MovRegImm64(R10, int64(in.B))
	a.MovqXR(X3, R10)
	a.LeaRegMemD(R10, RSI, dst)
	a.MovqXR(X4, R10)
	if e.segRead == 0 {
		e.segRead = a.NewLabel()
	}
	a.CallL(e.segRead)
	e.jccStub(CondNE, pc)
	if name := segKindLabel(in.T); name != "" {
		// The predicted kind's read, which a plain column of it jumps to.
		a.JmpL(l("done"))
		a.Bind(l(name))
		e.segArm(in.T, func(Reg) (Reg, int32) { return RSI, dst }, l("done"), func() { e.jccWB(pc) })
	}
	if rowsLast != nil {
		a.JmpL(l("done"))
		rowsLast()
	}

	a.Bind(l("done"))
	e.unmap(in.C)
	e.know.wroteAny(in.C, false)
	// The I word kept for the run's next read of r[C], as column keeps it:
	// loaded once here, where every way of reading the column has stored it.
	if u := e.nextUse(in.C, pc); u >= 0 {
		if m, ok := e.alloc(pc, u, false); ok {
			a.MovRegMemD(m, RSI, e.d(in.C, L.OffI))
			e.holds[in.C] = m
		}
	}
}

// emitSegRead emits segColumn's full read, which every column a program reads
// shares: column X3 of the cursor at RAX, its row Pos, into the Value at X4,
// read from the segment's blocks when the engine handed one over (VCursor.Seg),
// else from its row list. It returns with ZF set when it read the cell, and
// clear, having written nothing, where the VDBE must: a check that fails, an
// exception, a kind this build does not read, or a string with the write
// barrier on. RAX, RCX, RDX, R9, R10, X0, X1, X3 and X4 are scratch.
//
// A segment cell reads exactly as segColumnValue reads it. The destination
// holds no string when it is read from a segment (destNoS before the call).
func (e *vmEmit) emitSegRead() {
	a, L := e.a, e.lay
	fail, ok := a.NewLabel(), a.NewLabel()
	seg, segs, nosel, nonull, noexc, null := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	nostr := a.NewLabel()
	var at [VSegText + 1]Lbl
	for _, k := range segArmKinds {
		at[k] = a.NewLabel()
	}
	a.Bind(e.segRead)
	a.CmpMemDImm8(RAX, VCursorOffOk, 0)
	a.JccL(CondE, fail)
	a.MovqRX(RCX, X3)
	a.CmpRegImm32(RCX, 63)
	a.JccL(condAbove, fail) // past the mask's 64 columns
	a.MovRegMemD(RDX, RAX, VCursorOffMask)
	a.BtRegReg(RDX, RCX)
	a.JccL(condAboveEq, fail) // the column is not readable
	a.MovRegMemD(RDX, RAX, VCursorOffPos)
	a.AluRegMemD(AluCmp, RDX, RAX, VCursorOffN)
	a.JccL(condAboveEq, fail) // unsigned: a position before the first row is huge
	a.MovRegMemD(R9, RAX, VCursorOffSeg)
	a.TestRegReg(R9, R9)
	a.JccL(CondNE, seg)

	// The row list's: the Value copied whole, its string's pointer shared.
	a.ImulRegRegImm32(RDX, RDX, sliceHeaderSize)
	a.AddRegMemD(RDX, RAX, VCursorOffRows)
	a.MovRegMemD(R10, RDX, 8) // the row's length
	a.CmpRegReg(R10, RCX)
	a.JccL(condBelowEq, fail) // a row narrower than the column: the VDBE's
	a.MovRegMemD(R9, RDX, 0)
	a.ImulRegRegImm32(RCX, RCX, int32(L.Size))
	a.AddRegReg(R9, RCX)
	a.MovqRX(R10, X4)
	// The string words only when the column or the destination holds a
	// string, as column copies.
	a.MovRegMemD(RDX, R9, int32(L.OffS))
	a.AluRegMemD(AluOr, RDX, R10, int32(L.OffS))
	a.JccL(CondE, nostr)
	a.CmpMemDByte(RDI, POffWB, 0)
	a.JccL(CondNE, fail)
	for _, off := range []int{L.OffS, L.OffS + 8, L.OffS + 16} {
		a.MovRegMemD(RDX, R9, int32(off))
		a.MovMemDReg(R10, int32(off), RDX)
	}
	a.Bind(nostr)
	for _, off := range []int{0, L.OffI, L.OffF} {
		a.MovRegMemD(RDX, R9, int32(off))
		a.MovMemDReg(R10, int32(off), RDX)
	}
	a.JmpL(ok)

	// The segment's: R9 the VSeg, RDX the row (Sel[Pos] when the cursor has
	// a selection), R10 the column's VSegCol.
	a.Bind(seg)
	e.rowSeg(RAX, segs)
	a.MovRegMemD(R10, RAX, VCursorOffSel)
	a.TestRegReg(R10, R10)
	a.JccL(CondE, nosel)
	a.MovRegMemIdx(RDX, R10, RDX)
	a.Bind(nosel)
	a.MovRegMemD(R10, R9, VSegOffCols)
	a.MovqRX(RCX, X3)
	a.ImulRegRegImm32(RCX, RCX, VSegColSize)
	a.AddRegReg(R10, RCX)
	a.MovRegMemD(RAX, R10, VSegColOffNulls)
	a.TestRegReg(RAX, RAX)
	a.JccL(CondE, nonull)
	a.MovRegReg(RCX, RDX)
	a.SarRegImm8(RCX, 3)
	a.AddRegReg(RAX, RCX)
	a.MovzxByte(RAX, RAX)
	a.MovRegReg(RCX, RDX)
	a.AndRegImm32(RCX, 7)
	a.BtRegReg(RAX, RCX)
	a.JccL(condCarry, null)
	a.Bind(nonull)
	a.MovRegMemD(RAX, R10, VSegColOffExc)
	a.TestRegReg(RAX, RAX)
	a.JccL(CondE, noexc)
	a.MovRegReg(RCX, RDX)
	a.SarRegImm8(RCX, 6)
	a.ShlRegImm8(RCX, 3)
	a.AddRegReg(RAX, RCX)
	a.MovRegMemD(RAX, RAX, 0)
	a.BtRegReg(RAX, RDX)
	a.JccL(condCarry, fail) // an exception: the VDBE's
	a.Bind(noexc)
	a.MovRegMemD(RCX, R10, VSegColOffKind)
	for _, k := range segArmKinds {
		a.CmpRegImm32(RCX, int32(k))
		a.JccL(CondE, at[k])
	}
	a.JmpL(fail) // a kind this build does not read
	dest := func(m Reg) (Reg, int32) {
		a.MovqRX(m, X4)
		return m, 0
	}
	wb := func() {
		a.CmpMemDByte(RDI, POffWB, 0)
		a.JccL(CondNE, fail)
	}
	for _, k := range segArmKinds {
		a.Bind(at[k])
		e.segArm(k, dest, ok, wb)
	}
	a.Bind(null)
	e.segArm(segArmNull, dest, ok, wb)

	a.Bind(ok)
	a.XorRegReg(RCX, RCX) // ZF set: read
	a.Ret()
	a.Bind(fail)
	a.aluImm32(1, RCX, 1) // ZF clear: the VDBE's
	a.Ret()
}

// segArmKinds is the kinds of segment cell native code reads.
var segArmKinds = []int{VSegInt, VSegFloat, VSegNumeric, VSegIntReal, VSegRowid, VSegText}

// segArmNull is segArm's NULL, a cell its column's bitmap marks.
const segArmNull = -1

// segArm reads a segment cell of kind k, RDX its row, R9 its VSeg and R10
// its VSegCol, into the Value dest gives (base and displacement, the base
// loaded into the scratch register it is handed where it must be), then
// jumps to done. wb fails the read when the write barrier is on, which a
// string's pointer store needs off.
func (e *vmEmit) segArm(k int, dest func(scratch Reg) (Reg, int32), done Lbl, wb func()) {
	a, L := e.a, e.lay
	cell := func() { // RAX = the 8-byte cell at row RDX
		a.MovRegMemD(RAX, R10, VSegColOffBlock)
		a.MovRegMemIdx(RAX, RAX, RDX)
	}
	const none = Reg(255)
	writeNum := func(typ byte, ival, fval Reg) { // a number, no string
		b, d := dest(R10)
		a.MovMemDImm(b, d, int32(typ))
		if ival != none {
			a.MovMemDReg(b, d+int32(L.OffI), ival)
		} else {
			a.MovMemDImm(b, d+int32(L.OffI), 0)
		}
		if fval != none {
			a.MovMemDReg(b, d+int32(L.OffF), fval)
		} else {
			a.MovMemDImm(b, d+int32(L.OffF), 0)
		}
		for k := range 3 {
			a.MovMemDImm(b, d+int32(L.OffS+8*k), 0)
		}
		a.JmpL(done)
	}
	switch k {
	case VSegInt:
		cell()
		writeNum(L.TypInt, RAX, none)
	// A NULL cell is a NULL: a column is in the cursor's view only where
	// every row is as wide as the table, so none reads a default, and the
	// rowid alias's stored NULL has no bitmap.
	case segArmNull:
		writeNum(L.TypNull, none, none)
	case VSegFloat:
		cell()
		writeNum(L.TypFloat, none, RAX)
	case VSegIntReal:
		cell()
		a.Cvtsi2sdq(X0, RAX)
		a.MovqRX(RAX, X0)
		writeNum(L.TypFloat, none, RAX)
	// An integral double within 2^53 is the INTEGER, any other the REAL
	// (numericValue). A NaN or a value past int64 fails the round trip.
	case VSegNumeric:
		numreal := a.NewLabel()
		cell()
		a.MovqXR(X0, RAX)
		a.Cvttsd2siq(RCX, X0)
		a.Cvtsi2sdq(X1, RCX)
		a.Ucomisd(X0, X1)
		a.JccL(CondNE, numreal)
		a.JccL(condParity, numreal)
		a.MovRegImm64(R10, 1<<53)
		a.AddRegReg(R10, RCX)
		a.MovRegImm64(R9, 1<<54)
		a.CmpRegReg(R10, R9)
		a.JccL(condAbove, numreal)
		writeNum(L.TypInt, RCX, none)
		a.Bind(numreal)
		writeNum(L.TypFloat, none, RAX)
	case VSegRowid:
		rowids := a.NewLabel()
		a.CmpMemDImm8(R9, VSegOffDense, 0)
		a.JccL(CondE, rowids)
		a.MovRegMemD(RAX, R9, VSegOffFirst)
		a.AddRegReg(RAX, RDX)
		writeNum(L.TypInt, RAX, none)
		a.Bind(rowids)
		a.MovRegMemD(RAX, R9, VSegOffRowids)
		a.MovRegMemIdx(RAX, RAX, RDX)
		writeNum(L.TypInt, RAX, none)
	// TEXT: the bytes from bound Pos to bound Pos+1 of the heap, shared as
	// segment.Value shares them, which is a pointer store.
	case VSegText:
		tlen := a.NewLabel()
		wb()
		a.MovRegMemD(RAX, R10, VSegColOffBlock)
		a.ShlRegImm8(RDX, 2)
		a.AddRegReg(RAX, RDX)
		a.MovRegMemD32(RCX, RAX, 0) // from
		a.MovRegMemD32(RDX, RAX, 4) // to
		a.SubRegReg(RDX, RCX)       // the length
		// An empty string points at the heap's start, never one past its
		// end: Go's own slicing of a zero capacity keeps the base pointer.
		a.JccL(CondNE, tlen)
		a.XorRegReg(RCX, RCX)
		a.Bind(tlen)
		a.AddRegMemD(RCX, R10, VSegColOffHeap)
		b, d := dest(RAX)
		a.MovMemDImm(b, d, int32(L.TypText))
		a.MovMemDImm(b, d+int32(L.OffI), 0)
		a.MovMemDImm(b, d+int32(L.OffF), 0)
		a.MovMemDReg(b, d+int32(L.OffS), RCX)
		a.MovMemDReg(b, d+int32(L.OffS+8), RDX)
		a.MovMemDReg(b, d+int32(L.OffS+16), RDX)
		a.JmpL(done)
	}
}

// AddRegMemD is  dst += [base+disp].
func (a *Asm) AddRegMemD(dst, base Reg, disp int32) { a.AluRegMemD(AluAdd, dst, base, disp) }

// seekRowid is VSeekRowid. The segments are tried in turn, each by its rowid
// range: a dense one holds key at key-First, any other is searched (its rowid
// block ascends, compared signed, as segment.findRowid compares). RAX, RCX,
// RDX, R9, R10 and X0-X2 are scratch.
func (e *vmEmit) seekRowid(pc int, in VInsn) {
	a, L := e.a, e.lay
	cb := int32(in.A * VCursorSize)
	e.unmapCursor(in.A)
	l := e.named()
	a.CmpMemDByte(RSI, e.d(in.B, L.OffTyp), L.TypInt)
	e.jccStub(CondNE, pc) // seekRowidKey's conversions are the VDBE's
	e.jccWB(pc)           // the cursor's segment pointers are stored
	a.MovRegMemD(RAX, RDI, POffCur)
	a.LeaRegMemD(RAX, RAX, cb)
	a.MovRegMemD(R9, RAX, VCursorOffTab)
	a.TestRegReg(R9, R9)
	e.jccStub(CondE, pc)
	a.MovqXR(X0, RAX) // the cursor
	a.MovRegMemD(RCX, RSI, e.d(in.B, L.OffI))
	a.MovRegMemD(R10, R9, VTabOffSegs)
	a.MovRegMemD(R9, R9, VTabOffN)

	a.Bind(l("seg"))
	a.TestRegReg(R9, R9)
	a.JccL(CondE, l("miss"))
	a.AluRegMemD(AluCmp, RCX, R10, VTabSegOffLo)
	a.JccL(CondL, l("next"))
	a.AluRegMemD(AluCmp, RCX, R10, VTabSegOffHi)
	a.JccL(CondG, l("next"))
	a.MovRegMemD(RDX, R10, VTabSegOffSeg)
	a.CmpMemDImm8(RDX, VSegOffDense, 0)
	a.JccL(CondE, l("search"))
	a.MovRegReg(RAX, RCX)
	a.AluRegMemD(AluSub, RAX, RDX, VSegOffFirst)
	a.JmpL(l("found"))

	a.Bind(l("search"))
	a.MovqXR(X1, R9)
	a.MovqXR(X2, R10)
	a.MovRegMemD(RDX, RDX, VSegOffRowids)
	a.XorRegReg(RAX, RAX)                 // lo
	a.MovRegMemD(R9, R10, VTabSegOffRows) // hi
	a.Bind(l("bloop"))
	a.CmpRegReg(RAX, R9)
	a.JccL(CondGE, l("bdone"))
	a.MovRegReg(R10, RAX)
	a.AddRegReg(R10, R9)
	a.SarRegImm8(R10, 1)
	a.CmpMemIdxReg(RDX, R10, RCX) // rowids[mid] against key
	a.JccL(CondGE, l("bhi"))
	a.LeaRegMemD(RAX, R10, 1)
	a.JmpL(l("bloop"))
	a.Bind(l("bhi"))
	a.MovRegReg(R9, R10)
	a.JmpL(l("bloop"))
	a.Bind(l("bdone"))
	a.MovqRX(R10, X2)
	a.MovqRX(R9, X1)
	a.AluRegMemD(AluCmp, RAX, R10, VTabSegOffRows)
	a.JccL(CondGE, l("next"))
	a.CmpMemIdxReg(RDX, RAX, RCX)
	a.JccL(CondNE, l("next"))
	a.MovRegMemD(RDX, R10, VTabSegOffSeg)
	a.JmpL(l("found"))

	a.Bind(l("next"))
	a.AddRegImm32(R10, VTabSegSize)
	a.DecReg(R9)
	a.JmpL(l("seg"))

	// Found at row RAX of VSeg RDX, R10 its VTabSeg: the cursor on that one
	// row, as a point seek leaves it.
	a.Bind(l("found"))
	a.MovqRX(R9, X0)
	a.MovMemDReg(R9, VCursorOffSeg, RDX)
	a.MovMemDReg(R9, VCursorOffKey, RCX)
	a.MovRegMemD(RCX, RDX, VSegOffRowids)
	a.MovMemDReg(R9, VCursorOffRids, RCX)
	a.MovRegMemD(RCX, R10, VTabSegOffMask)
	a.MovMemDReg(R9, VCursorOffMask, RCX)
	a.MovMemDImm(R9, VCursorOffSel, 0)
	a.MovMemDImm(R9, VCursorOffSegs, 0)
	a.MovMemDReg(R9, VCursorOffPos, RAX)
	a.MovMemDReg(R9, VCursorOffLast, RAX)
	a.IncReg(RAX)
	a.MovMemDReg(R9, VCursorOffN, RAX)
	a.MovMemDImm(R9, VCursorOffOk, 1)
	a.MovMemDImm(R9, VCursorOffDirty, 1)
	a.MovMemDImm(R9, VCursorOffAdv, 1)
	a.MovMemDImm(R9, VCursorOffState, 2)
	a.MovRegMemD(RCX, R9, VCursorOffSrvd)
	a.IncReg(RCX)
	a.MovMemDReg(R9, VCursorOffSrvd, RCX)
	a.IncMemD(R9, VCursorOffSteps)
	a.JmpL(l("done"))

	// No row: the cursor on none, and on to T.
	a.Bind(l("miss"))
	a.MovqRX(R9, X0)
	a.MovMemDReg(R9, VCursorOffKey, RCX)
	a.MovMemDImm(R9, VCursorOffN, 0)
	a.MovMemDImm(R9, VCursorOffPos, 0)
	a.MovMemDImm(R9, VCursorOffOk, 1)
	a.MovMemDImm(R9, VCursorOffDirty, 1)
	a.MovMemDImm(R9, VCursorOffState, 3)
	e.jmpBlock(pc, in.T)
	a.Bind(l("done"))
}

// seekIdx is VSeekIdx: the segments' indexes in turn, each skipped when the
// key is outside its range and otherwise binary searched; a find is the cursor
// on the rows its run names (Sel), unless a later segment's range holds the key
// too. RAX, RCX, RDX, R9, R10 and X0-X2 are scratch.
func (e *vmEmit) seekIdx(pc int, in VInsn) {
	a, L := e.a, e.lay
	cb := int32(in.A * VCursorSize)
	e.unmapCursor(in.A)
	l := e.named()
	a.CmpMemDByte(RSI, e.d(in.B, L.OffTyp), L.TypInt)
	e.jccStub(CondNE, pc) // the key's conversions are the VDBE's
	e.jccWB(pc)           // the cursor's segment pointers are stored
	a.MovRegMemD(RAX, RDI, POffCur)
	a.LeaRegMemD(RAX, RAX, cb)
	a.MovRegMemD(R9, RAX, VCursorOffIdx)
	a.TestRegReg(R9, R9)
	e.jccStub(CondE, pc)
	a.MovqXR(X0, RAX) // the cursor
	a.MovRegMemD(RCX, RSI, e.d(in.B, L.OffI))
	a.MovRegMemD(RDX, R9, VIdxOffCount)

	a.Bind(l("seg"))
	a.TestRegReg(RDX, RDX)
	a.JccL(CondE, l("miss"))
	a.MovqXR(X1, R9) // this segment's index
	a.MovqXR(X2, RDX)
	a.AluRegMemD(AluCmp, RCX, R9, VIdxOffLo)
	a.JccL(CondL, l("next"))
	a.AluRegMemD(AluCmp, RCX, R9, VIdxOffHi)
	a.JccL(CondG, l("next"))
	a.MovRegMemD(R10, R9, VIdxOffVals)
	a.MovRegMemD(RDX, R9, VIdxOffNVals) // hi
	a.XorRegReg(RAX, RAX)               // lo
	a.Bind(l("bloop"))
	a.CmpRegReg(RAX, RDX)
	a.JccL(CondGE, l("bdone"))
	a.MovRegReg(R9, RAX)
	a.AddRegReg(R9, RDX)
	a.SarRegImm8(R9, 1)
	a.CmpMemIdxReg(R10, R9, RCX) // vals[mid] against key
	a.JccL(CondGE, l("bhi"))
	a.LeaRegMemD(RAX, R9, 1)
	a.JmpL(l("bloop"))
	a.Bind(l("bhi"))
	a.MovRegReg(RDX, R9)
	a.JmpL(l("bloop"))
	a.Bind(l("bdone"))
	a.MovqRX(R9, X1)
	a.AluRegMemD(AluCmp, RAX, R9, VIdxOffNVals)
	a.JccL(CondGE, l("next"))
	a.CmpMemIdxReg(R10, RAX, RCX)
	a.JccL(CondNE, l("next"))

	// Found value RAX of segment R9: a later segment whose range holds the
	// key may hold some of its rows, which the VDBE gathers.
	a.MovqRX(RDX, X2)
	a.MovRegReg(R10, R9)
	a.Bind(l("rest"))
	a.DecReg(RDX)
	a.JccL(CondE, l("found"))
	a.AddRegImm32(R10, VIdxSize)
	a.AluRegMemD(AluCmp, RCX, R10, VIdxOffLo)
	a.JccL(CondL, l("rest"))
	a.AluRegMemD(AluCmp, RCX, R10, VIdxOffHi)
	e.jccStub(CondLE, pc)
	a.JmpL(l("rest"))

	a.Bind(l("next"))
	a.MovqRX(R9, X1)
	a.MovqRX(RDX, X2)
	a.AddRegImm32(R9, VIdxSize)
	a.DecReg(RDX)
	a.JmpL(l("seg"))

	// Its rows are Rows[Starts[RAX]:Starts[RAX+1]].
	a.Bind(l("found"))
	a.MovqRX(R10, X0)
	e.seekIdxServed(R10, RDX, l("fserved"), l("fsearch"))
	a.MovMemDReg(R10, VCursorOffKey, RCX)
	a.MovRegMemD(RDX, R9, VIdxOffStarts)
	a.MovsxdRegMemIdx4(RCX, RDX, RAX)
	a.IncReg(RAX)
	a.MovsxdRegMemIdx4(RAX, RDX, RAX)
	a.SubRegReg(RAX, RCX)
	a.MovMemDReg(R10, VCursorOffN, RAX)
	a.MovRegMemD(RDX, R9, VIdxOffRows)
	a.LeaIdx(RDX, RDX, RCX)
	a.MovMemDReg(R10, VCursorOffSel, RDX)
	a.MovRegMemD(RAX, R9, VIdxOffSegs)
	a.TestRegReg(RAX, RAX)
	a.JccL(CondE, l("fsegs"))
	a.LeaIdx(RAX, RAX, RCX)
	a.Bind(l("fsegs"))
	a.MovMemDReg(R10, VCursorOffSegs, RAX)
	a.MovRegMemD(RCX, R9, VIdxOffSeg)
	a.MovMemDReg(R10, VCursorOffSeg, RCX)
	a.MovRegMemD(RCX, RCX, VSegOffRowids)
	a.MovMemDReg(R10, VCursorOffRids, RCX)
	a.MovRegMemD(RCX, R9, VIdxOffMask)
	a.MovMemDReg(R10, VCursorOffMask, RCX)
	a.MovMemDImm(R10, VCursorOffPos, 0)
	a.MovMemDImm(R10, VCursorOffLast, 0)
	a.MovMemDImm(R10, VCursorOffOk, 1)
	a.MovMemDImm(R10, VCursorOffDirty, 1)
	a.MovMemDImm(R10, VCursorOffAdv, 1)
	a.MovMemDImm(R10, VCursorOffState, 2)
	a.IncMemD(R10, VCursorOffSteps)
	a.JmpL(l("done"))

	// No row: the cursor on none, and on to T.
	a.Bind(l("miss"))
	a.MovqRX(R10, X0)
	e.seekIdxServed(R10, RDX, l("mserved"), l("msearch"))
	a.MovMemDReg(R10, VCursorOffKey, RCX)
	a.MovMemDImm(R10, VCursorOffN, 0)
	a.MovMemDImm(R10, VCursorOffPos, 0)
	a.MovMemDImm(R10, VCursorOffSegs, 0)
	a.MovMemDImm(R10, VCursorOffOk, 1)
	a.MovMemDImm(R10, VCursorOffDirty, 1)
	a.MovMemDImm(R10, VCursorOffState, 3)
	e.jmpBlock(pc, in.T)
	a.Bind(l("done"))
}

// seekIdxServed adds to cursor cur's Served the rows its last seek's run
// served, up to the row it is on, when it has one (State 2, 4 or 5). tmp is
// scratch.
func (e *vmEmit) seekIdxServed(cur, tmp Reg, served, skip Lbl) {
	a := e.a
	a.CmpMemDImm8(cur, VCursorOffState, 2)
	a.JccL(CondE, served)
	a.CmpMemDImm8(cur, VCursorOffState, 4)
	a.JccL(CondE, served)
	a.CmpMemDImm8(cur, VCursorOffState, 5)
	a.JccL(CondNE, skip)
	a.Bind(served)
	a.MovRegMemD(tmp, cur, VCursorOffLast)
	a.IncReg(tmp)
	a.AddRegMemD(tmp, cur, VCursorOffSrvd)
	a.MovMemDReg(cur, VCursorOffSrvd, tmp)
	a.Bind(skip)
}

// seekRewind is VSeekRewind: a seek that found none and fell through to it
// (State 3) takes the rewind's jump to T.
func (e *vmEmit) seekRewind(pc int, in VInsn) {
	a := e.a
	cb := int32(in.A * VCursorSize)
	e.unmapCursor(in.A)
	found := e.vmLabel()
	a.MovRegMemD(RAX, RDI, POffCur)
	a.CmpMemDImm8(RAX, cb+VCursorOffState, 2)
	a.JccL(CondE, found)
	a.CmpMemDImm8(RAX, cb+VCursorOffState, 3)
	e.jccStub(CondNE, pc)
	e.jmpBlock(pc, in.T)
	a.Bind(found)
	a.MovMemDImm(RAX, cb+VCursorOffState, 4)
}

// nullRow is VNullRow.
func (e *vmEmit) nullRow(pc int, in VInsn) {
	a := e.a
	cb := int32(in.A * VCursorSize)
	e.unmapCursor(in.A)
	e.jccWB(pc)
	a.MovRegMemD(RAX, RDI, POffCur)
	a.MovRegImm64(RCX, nullSegAddr())
	a.MovMemDReg(RAX, cb+VCursorOffSeg, RCX)
	for _, f := range []struct{ off, v int32 }{{VCursorOffSel, 0}, {VCursorOffSegs, 0}, {VCursorOffRids, 0}, {VCursorOffN, 1}, {VCursorOffPos, 0},
		{VCursorOffLast, 0}, {VCursorOffOk, 1}, {VCursorOffDirty, 1}, {VCursorOffAdv, 0}, {VCursorOffRew, 0}, {VCursorOffMask, -1},
		{VCursorOffState, 6}} {
		a.MovMemDImm(RAX, cb+f.off, f.v)
	}
}

// text is VText: a pointer store, so the write barrier exits -- unless r[A]
// already holds this TEXT, as it does from the last row on, which is left as
// it is.
func (e *vmEmit) text(pc int, in VInsn) {
	if in.Dead {
		e.clobber(in.A)
		return
	}
	a, L := e.a, e.lay
	dst := e.d(in.A, 0)
	same := e.vmLabel()
	a.MovRegImm64(RAX, in.Imm)
	a.AluRegMemD(AluCmp, RAX, RSI, dst+int32(L.OffS))
	store := e.vmLabel()
	a.JccL(CondNE, store)
	a.CmpMemDByte(RSI, dst, L.TypText)
	a.JccL(CondNE, store)
	a.CmpMemDByte(RSI, dst+1, 0) // no subtype
	a.JccL(CondNE, store)
	a.MovRegImm64(RAX, int64(in.B))
	a.AluRegMemD(AluCmp, RAX, RSI, dst+int32(L.OffS+8))
	a.JccL(CondE, same)
	a.Bind(store)
	e.jccWB(pc)
	e.zeroS.del(in.A)
	a.MovMemDImm(RSI, dst, int32(L.TypText))
	a.MovMemDImm(RSI, dst+int32(L.OffI), 0)
	a.MovMemDImm(RSI, dst+int32(L.OffF), 0)
	a.MovRegImm64(RAX, in.Imm)
	a.MovMemDReg(RSI, dst+int32(L.OffS), RAX)
	a.MovMemDImm(RSI, dst+int32(L.OffS+8), int32(in.B))
	a.MovMemDImm(RSI, dst+int32(L.OffS+16), int32(in.B))
	a.Bind(same)
	e.unmap(in.A)
	e.know.wroteAny(in.A, false)
}

// inSub is VInSub: a binary search of the set's ascending keys. R9 the set
// (kept in X0 while R9 is the probe's middle), RDX its keys, RCX the key, RAX
// and R10 the bounds.
func (e *vmEmit) inSub(pc int, in VInsn) {
	if in.Dead {
		e.clobber(in.C)
		return
	}
	a, L := e.a, e.lay
	e.destNoS(pc, in.C)
	loop, hi, miss, absent, yes, no, done := e.vmLabel(), e.vmLabel(), e.vmLabel(), e.vmLabel(),
		e.vmLabel(), e.vmLabel(), e.vmLabel()
	a.MovRegMemD(R9, RDI, POffInSets)
	if in.B > 0 {
		a.AddRegImm32(R9, int32(in.B*VInSubSize))
	}
	a.CmpMemDImm8(R9, VInSubOffReady, 0)
	e.jccStub(CondE, pc)
	a.CmpMemDImm8(R9, VInSubOffEmpty, 0)
	a.JccL(CondNE, no)
	a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypInt)
	e.jccStub(CondNE, pc)
	a.MovRegMemD(RCX, RSI, e.d(in.A, L.OffI))
	a.MovRegMemD(RDX, R9, VInSubOffKeys)
	a.MovqXR(X0, R9)
	a.XorRegReg(RAX, RAX)
	a.MovRegMemD(R10, R9, VInSubOffN)
	a.Bind(loop)
	a.CmpRegReg(RAX, R10)
	a.JccL(CondGE, miss)
	a.MovRegReg(R9, RAX)
	a.AddRegReg(R9, R10)
	a.SarRegImm8(R9, 1)
	a.CmpMemIdxReg(RDX, R9, RCX) // keys[mid] against the key
	a.JccL(CondGE, hi)
	a.LeaRegMemD(RAX, R9, 1)
	a.JmpL(loop)
	a.Bind(hi)
	a.MovRegReg(R10, R9)
	a.JmpL(loop)
	a.Bind(miss)
	a.MovqRX(R9, X0)
	a.AluRegMemD(AluCmp, RAX, R9, VInSubOffN)
	a.JccL(CondGE, absent)
	a.CmpMemIdxReg(RDX, RAX, RCX)
	a.JccL(CondE, yes)
	a.Bind(absent)
	a.CmpMemDImm8(R9, VInSubOffHasNull, 0)
	e.jccStub(CondNE, pc) // NULL, which the VDBE answers
	a.JmpL(no)
	in1, in0 := int64(1), int64(0)
	if in.Imm == 1 {
		in1, in0 = 0, 1
	}
	a.Bind(yes)
	a.MovRegImm64(R10, in1)
	a.JmpL(done)
	a.Bind(no)
	a.MovRegImm64(R10, in0)
	a.Bind(done)
	m := e.dest(pc, in.C, R10)
	if m != R10 {
		a.MovRegReg(m, R10)
	}
	e.storeInt(pc, in, in.C, m)
}

// sortSeek is VSortSeek: the first of the Sort's keys at or above the key,
// binary searched, and the run of those equal to it after. RAX, RCX, RDX, R9,
// R10, X0 and X1 are scratch.
func (e *vmEmit) sortSeek(pc int, in VInsn) {
	a, L := e.a, e.lay
	cb := int32(in.A * VCursorSize)
	e.unmapCursor(in.A)
	l := e.named()
	a.CmpMemDByte(RSI, e.d(in.B, L.OffTyp), L.TypInt)
	e.jccStub(CondNE, pc) // the key's conversions are the VDBE's
	e.jccWB(pc)           // the cursor's row pointers are stored
	a.MovRegMemD(RAX, RDI, POffCur)
	a.LeaRegMemD(RAX, RAX, cb)
	a.MovRegMemD(R9, RAX, VCursorOffSort)
	a.TestRegReg(R9, R9)
	e.jccStub(CondE, pc)
	a.MovqXR(X0, RAX) // the cursor
	a.MovqXR(X1, R9)  // its Sort
	a.MovRegMemD(RCX, RSI, e.d(in.B, L.OffI))
	a.MovRegMemD(R10, R9, VSortOffInts)
	a.MovRegMemD(RDX, R9, VSortOffN) // hi
	a.XorRegReg(RAX, RAX)            // lo
	a.Bind(l("bloop"))
	a.CmpRegReg(RAX, RDX)
	a.JccL(CondGE, l("bdone"))
	a.MovRegReg(R9, RAX)
	a.AddRegReg(R9, RDX)
	a.SarRegImm8(R9, 1)
	a.CmpMemIdxReg(R10, R9, RCX) // ints[mid] against key
	a.JccL(CondGE, l("bhi"))
	a.LeaRegMemD(RAX, R9, 1)
	a.JmpL(l("bloop"))
	a.Bind(l("bhi"))
	a.MovRegReg(RDX, R9)
	a.JmpL(l("bloop"))
	a.Bind(l("bdone"))

	// The run: RAX to RDX.
	a.MovqRX(R9, X1)
	a.MovRegReg(RDX, RAX)
	a.Bind(l("run"))
	a.AluRegMemD(AluCmp, RDX, R9, VSortOffN)
	a.JccL(CondGE, l("end"))
	a.CmpMemIdxReg(R10, RDX, RCX)
	a.JccL(CondNE, l("end"))
	a.IncReg(RDX)
	a.JmpL(l("run"))
	a.Bind(l("end"))

	a.MovqRX(R10, X0)
	a.SubRegReg(RDX, RAX)
	a.MovMemDReg(R10, VCursorOffN, RDX)
	a.MovMemDReg(R10, VCursorOffSLo, RAX)
	a.MovRegMemD(RCX, R9, VSortOffIds)
	a.LeaIdx(RCX, RCX, RAX)
	a.MovMemDReg(R10, VCursorOffRids, RCX)
	a.MovRegReg(RDX, RAX)
	a.AddRegReg(RDX, RAX)
	a.AddRegReg(RDX, RAX) // 3*lo words: 24-byte headers
	a.MovRegMemD(RCX, R9, VSortOffRows)
	a.LeaIdx(RCX, RCX, RDX)
	a.MovMemDReg(R10, VCursorOffRows, RCX)
	a.MovMemDImm(R10, VCursorOffLast, -1)
	a.MovMemDImm(R10, VCursorOffDirty, 1)
}
