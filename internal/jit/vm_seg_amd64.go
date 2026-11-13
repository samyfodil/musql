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

// predictedRowRead is segColumn's predicted read of a cursor on its row list,
// reached at label prows with base the cursor's address and RDX its position:
// the run's first read of the cursor checks the row is as wide as every
// column the run predicts (mask) and keeps the row's address (vmRowKey), and
// every later one reads from that, as column does for a plain cursor. Without
// a register to keep it in, a read takes the row list's full read.
func (e *vmEmit) predictedRowRead(pc int, in VInsn, base Reg, first bool, mask uint64, l func(string) Lbl) {
	a, L := e.a, e.lay
	a.Bind(l("prows"))
	row, have := e.holds[vmRowKey(in.A)]
	if !have && first {
		if u := e.nextUse(vmRowKey(in.A), pc); u >= 0 {
			if m, ok := e.alloc(pc, u, true); ok {
				row, have = m, true
				a.MovRegImm64(R10, sliceHeaderSize)
				a.ImulRegReg(RDX, R10)
				a.AddRegMemD(RDX, base, VCursorOffRows)
				a.MovRegMemD(R10, RDX, 8) // the row's length
				a.CmpRegImm32(R10, int32(bits.Len64(mask)-1))
				e.jccStub(condBelowEq, pc) // a row narrower than a column: the VDBE's
				a.MovRegMemD(row, RDX, 0)
				e.holds[vmRowKey(in.A)] = row
			}
		}
	}
	if !have {
		a.MovRegMemD(RAX, RDI, POffCur)
		a.JmpL(l("rows"))
		return
	}
	e.pinned.add(int(row))
	dst := e.d(in.C, 0)
	vb := int32(in.B) * int32(L.Size)
	// As column copies: the string words only when the column or the
	// destination holds a string, with the write barrier off.
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
	a.JmpL(l("done"))
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
	e.destNoS(pc, in.C)
	e.zeroS.del(in.C) // every word of the destination is written below
	dst := e.d(in.C, 0)
	l := e.named()
	if name := segKindLabel(in.T); name != "" {
		base, have := e.holds[^in.A]
		first, mask := !have, uint64(1)<<uint(in.B)
		if have {
			e.pinned.add(int(base))
		} else {
			if e.cache {
				mask = vmRunPredicted(e.prog, e.bound, pc)
			}
			a.MovRegMemD(RAX, RDI, POffCur)
			a.LeaRegMemD(RAX, RAX, cb)
			a.CmpMemDImm8(RAX, VCursorOffOk, 0)
			e.jccStub(CondE, pc)
			a.MovRegMemD(RDX, RAX, VCursorOffMask)
			a.MovRegImm64(R10, int64(mask))
			a.AndRegReg(RDX, R10)
			a.CmpRegReg(RDX, R10)
			e.jccStub(CondNE, pc)
			a.MovRegMemD(RDX, RAX, VCursorOffPos)
			a.AluRegMemD(AluCmp, RDX, RAX, VCursorOffN)
			e.jccStub(condAboveEq, pc) // unsigned: a position before the first row is huge
			base = RAX
			if u := e.nextUse(^in.A, pc); u >= 0 {
				if m, ok := e.alloc(pc, u, true); ok {
					a.MovRegReg(m, RAX)
					base = m
					e.holds[^in.A] = m
					e.pinned.add(int(m))
				}
			}
		}
		a.MovRegMemD(RDX, base, VCursorOffPos)
		a.MovRegMemD(R9, base, VCursorOffSeg)
		a.TestRegReg(R9, R9)
		a.JccL(CondE, l("prows"))
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
		e.predictedRowRead(pc, in, base, first, mask, l)
		a.Bind(l("full"))
	} else {
		e.unmapCursor(in.A)
	}

	a.MovRegMemD(RAX, RDI, POffCur)
	a.CmpMemDImm8(RAX, cb+VCursorOffOk, 0)
	e.jccStub(CondE, pc)
	a.MovRegMemD(RDX, RAX, cb+VCursorOffMask)
	a.MovRegImm64(R10, int64(uint64(1)<<uint(in.B)))
	a.TestRegReg(RDX, R10)
	e.jccStub(CondE, pc)
	a.MovRegMemD(RDX, RAX, cb+VCursorOffPos)
	a.AluRegMemD(AluCmp, RDX, RAX, cb+VCursorOffN)
	e.jccStub(condAboveEq, pc) // unsigned: a position before the first row is huge
	a.MovRegMemD(R9, RAX, cb+VCursorOffSeg)
	a.TestRegReg(R9, R9)
	a.JccL(CondNE, l("seg"))

	// The row list's: the Value copied whole, its string's pointer shared.
	a.Bind(l("rows"))
	a.MovRegImm64(R10, sliceHeaderSize)
	a.ImulRegReg(RDX, R10)
	a.AddRegMemD(RDX, RAX, cb+VCursorOffRows)
	a.MovRegMemD(R10, RDX, 8) // the row's length
	a.CmpRegImm32(R10, int32(in.B))
	e.jccStub(condBelowEq, pc) // a row narrower than the column: the VDBE's
	a.MovRegMemD(R9, RDX, 0)
	vb := int32(in.B) * int32(L.Size)
	// The string words only when the column or the destination holds a
	// string, as column copies.
	a.MovRegMemD(RDX, R9, vb+int32(L.OffS))
	a.AluRegMemD(AluOr, RDX, RSI, dst+int32(L.OffS))
	e.coldPath(func() {
		e.jccWB(pc)
		for _, off := range []int{L.OffS, L.OffS + 8, L.OffS + 16} {
			a.MovRegMemD(RDX, R9, vb+int32(off))
			a.MovMemDReg(RSI, dst+int32(off), RDX)
		}
	})
	for _, off := range []int{0, L.OffI, L.OffF} {
		a.MovRegMemD(RDX, R9, vb+int32(off))
		a.MovMemDReg(RSI, dst+int32(off), RDX)
	}
	a.JmpL(l("done"))

	// The segment's: R9 the VSeg, RDX the row (Sel[Pos] when the cursor has
	// a selection), R10 the column's VSegCol.
	a.Bind(l("seg"))
	a.LeaRegMemD(RCX, RAX, cb)
	e.rowSeg(RCX, l("segs"))
	a.MovRegMemD(R10, RAX, cb+VCursorOffSel)
	a.TestRegReg(R10, R10)
	a.JccL(CondE, l("nosel"))
	a.MovRegMemIdx(RDX, R10, RDX)
	a.Bind(l("nosel"))
	a.MovRegMemD(R10, R9, VSegOffCols)
	a.AddRegImm32(R10, int32(in.B*VSegColSize))
	a.MovRegMemD(RAX, R10, VSegColOffNulls)
	a.TestRegReg(RAX, RAX)
	a.JccL(CondE, l("nonull"))
	a.MovRegReg(RCX, RDX)
	a.SarRegImm8(RCX, 3)
	a.AddRegReg(RAX, RCX)
	a.MovzxByte(RAX, RAX)
	a.MovRegReg(RCX, RDX)
	a.AndRegImm32(RCX, 7)
	a.BtRegReg(RAX, RCX)
	a.JccL(condCarry, l("null"))
	a.Bind(l("nonull"))
	a.MovRegMemD(RAX, R10, VSegColOffExc)
	a.TestRegReg(RAX, RAX)
	a.JccL(CondE, l("noexc"))
	a.MovRegReg(RCX, RDX)
	a.SarRegImm8(RCX, 6)
	a.ShlRegImm8(RCX, 3)
	a.AddRegReg(RAX, RCX)
	a.MovRegMemD(RAX, RAX, 0)
	a.BtRegReg(RAX, RDX)
	e.jccStub(condCarry, pc) // an exception: the VDBE's
	a.Bind(l("noexc"))
	a.MovRegMemD(RCX, R10, VSegColOffKind)
	for _, k := range []struct {
		kind int
		name string
	}{{VSegInt, "int"}, {VSegFloat, "float"}, {VSegNumeric, "num"}, {VSegIntReal, "intreal"}, {VSegRowid, "rowid"}, {VSegText, "text"}} {
		a.CmpRegImm32(RCX, int32(k.kind))
		a.JccL(CondE, l(k.name))
	}
	e.jmpStub(pc) // a kind this build does not read

	cell := func() { // RAX = the 8-byte cell at row RDX
		a.MovRegMemD(RAX, R10, VSegColOffBlock)
		a.MovRegMemIdx(RAX, RAX, RDX)
	}
	const none = Reg(255)
	writeNum := func(typ byte, ival, fval Reg) { // a number, no string
		a.MovMemDImm(RSI, dst, int32(typ))
		if ival != none {
			a.MovMemDReg(RSI, dst+int32(L.OffI), ival)
		} else {
			a.MovMemDImm(RSI, dst+int32(L.OffI), 0)
		}
		if fval != none {
			a.MovMemDReg(RSI, dst+int32(L.OffF), fval)
		} else {
			a.MovMemDImm(RSI, dst+int32(L.OffF), 0)
		}
		for k := range 3 {
			a.MovMemDImm(RSI, dst+int32(L.OffS+8*k), 0)
		}
		a.JmpL(l("done"))
	}

	a.Bind(l("int"))
	cell()
	writeNum(L.TypInt, RAX, none)

	// A NULL cell is a NULL: a column is in the cursor's view only where every
	// row is as wide as the table, so none reads a default, and the rowid
	// alias's stored NULL has no bitmap.
	a.Bind(l("null"))
	writeNum(L.TypNull, none, none)

	a.Bind(l("float"))
	cell()
	writeNum(L.TypFloat, none, RAX)

	a.Bind(l("intreal"))
	cell()
	a.Cvtsi2sdq(X0, RAX)
	a.MovqRX(RAX, X0)
	writeNum(L.TypFloat, none, RAX)

	// An integral double within 2^53 is the INTEGER, any other the REAL
	// (numericValue). A NaN or a value past int64 fails the round trip.
	a.Bind(l("num"))
	cell()
	a.MovqXR(X0, RAX)
	a.Cvttsd2siq(RCX, X0)
	a.Cvtsi2sdq(X1, RCX)
	a.Ucomisd(X0, X1)
	a.JccL(CondNE, l("numreal"))
	a.JccL(condParity, l("numreal"))
	a.MovRegImm64(R10, 1<<53)
	a.AddRegReg(R10, RCX)
	a.MovRegImm64(R9, 1<<54)
	a.CmpRegReg(R10, R9)
	a.JccL(condAbove, l("numreal"))
	writeNum(L.TypInt, RCX, none)
	a.Bind(l("numreal"))
	writeNum(L.TypFloat, none, RAX)

	a.Bind(l("rowid"))
	a.CmpMemDImm8(R9, VSegOffDense, 0)
	a.JccL(CondE, l("rowids"))
	a.MovRegMemD(RAX, R9, VSegOffFirst)
	a.AddRegReg(RAX, RDX)
	writeNum(L.TypInt, RAX, none)
	a.Bind(l("rowids"))
	a.MovRegMemD(RAX, R9, VSegOffRowids)
	a.MovRegMemIdx(RAX, RAX, RDX)
	writeNum(L.TypInt, RAX, none)

	// TEXT: the bytes from bound Pos to bound Pos+1 of the heap, shared as
	// segment.Value shares them, which is a pointer store.
	a.Bind(l("text"))
	e.jccWB(pc)
	a.MovRegMemD(RAX, R10, VSegColOffBlock)
	a.ShlRegImm8(RDX, 2)
	a.AddRegReg(RAX, RDX)
	a.MovRegMemD32(RCX, RAX, 0) // from
	a.MovRegMemD32(RDX, RAX, 4) // to
	a.SubRegReg(RDX, RCX)       // the length
	// An empty string points at the heap's start, never one past its end:
	// Go's own slicing of a zero capacity keeps the base pointer.
	a.JccL(CondNE, l("tlen"))
	a.XorRegReg(RCX, RCX)
	a.Bind(l("tlen"))
	a.AddRegMemD(RCX, R10, VSegColOffHeap)
	a.MovMemDImm(RSI, dst, int32(L.TypText))
	a.MovMemDImm(RSI, dst+int32(L.OffI), 0)
	a.MovMemDImm(RSI, dst+int32(L.OffF), 0)
	a.MovMemDReg(RSI, dst+int32(L.OffS), RCX)
	a.MovMemDReg(RSI, dst+int32(L.OffS+8), RDX)
	a.MovMemDReg(RSI, dst+int32(L.OffS+16), RDX)

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
