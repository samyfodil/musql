package jit

import (
	"unsafe"

	"github.com/samyfodil/musql/internal/fir"
)

// VM JIT: compiling whole VDBE programs to machine code.
// EmitProgram compiles every recognized instruction to native blocks.
// next. Nothing is lost by an opcode the emitter does not know except the speed
// of that one instruction.
//
// THE CONTRACT, which is what makes the two interchangeable mid-program:
//
//   - The register file is the VDBE's own []Value, read and written in place.
//     There is no second copy of any register, so an exit needs no spill and an
//     entry needs no reload.
//   - Every native instruction checks its operands' types BEFORE it writes
//     anything. When a guard fails -- a NULL, a REAL, a TEXT, an integer
//     overflow -- the code exits at that instruction's pc with every register
//     exactly as it was, and the VDBE runs the instruction with its full
//     semantics. Native code therefore implements only the INTEGER arm of each
//     opcode, the one the VDBE itself tests first.
//   - A store that writes or overwrites a string or blob pointer (a copy of a
//     TEXT value, an integer stored over one) is made natively only while the
//     GC's write barrier is off, which the prologue reads once per entry
//     (wb.go). While it is on, the instruction exits before it writes
//     anything, and the VDBE's ordinary assignment makes the store, barrier
//     and all. A store of an integer, a NULL or a REAL over a register known
//     to hold no pointer needs no test.
//   - Every backward jump, and every OP_Return, spends one unit of fuel. When
//     it runs out the code exits at the jump's target. Generated code cannot be
//     preempted, so this is what bounds the time between two returns to Go.

// VOp is a VM-JIT opcode.
type VOp uint8

const (
	// VExit: no native form; return pc for the VDBE to run.
	VExit VOp = iota
	// VGoto: jump to T.
	VGoto
	// VInt: r[A] = integer Imm.
	VInt
	// VCopy: r[B] = r[A]. With Imm == 0 (OP_Copy, which copies a string's
	// bytes) only a Value with no string or blob; with Imm != 0 (OP_SCopy, a
	// shallow copy) any Value, its pointer shared.
	VCopy
	// VAdd, VSub, VMul: r[C] = r[B] op r[A], integers, exiting on overflow.
	VAdd
	VSub
	VMul
	// VAnd, VOr: r[C] = r[B] op r[A], integers.
	VAnd
	VOr
	// VShl, VShr: r[C] = r[B] shifted by r[A], integers, for a shift amount in
	// 0..63 (VShr is arithmetic). Any other amount exits.
	VShl
	VShr
	// VCmpJump: jump to T when r[B] Cond r[A], both integers.
	VCmpJump
	// VCmpSet: r[C] = 1 if r[B] Cond r[A] else 0, both integers: the store form
	// of a comparison.
	VCmpSet
	// VNullJump: jump to T when r[A] is NULL (Imm != 0) or is not NULL (Imm == 0).
	// Any type answers; nothing is guarded.
	VNullJump
	// VNull: r[C] = NULL.
	VNull
	// VNeg: r[C] = -r[A], an integer other than the minimum (which exits).
	VNeg
	// VIf: jump to T when r[A] is an integer whose truth is Imm != 0; a NULL
	// jumps when C != 0.
	VIf
	// VGosub: r[A] = this pc, jump to T.
	VGosub
	// VReturn: jump to r[A] + 1, an integer inside the program.
	VReturn
	// VColumn: r[C] = column B of cursor A's current row, when the cursor is
	// usable (VCursor.Ok), positioned on a row, and the value holds no string
	// or blob. Anything else exits.
	VColumn
	// VRewind: put cursor A on its first row and fall through, or jump to T when
	// it has none. A cursor that is not usable exits, as does the first rewind
	// of one the engine has not yet materialized (it is not usable).
	VRewind
	// VNext: advance cursor A one row and jump to T if it is on one; fall
	// through when it ran off the end. A cursor that is not usable exits.
	VNext
	// VDiv, VRem: r[C] = r[B] / r[A] or r[B] % r[A], integers (truncating), for
	// a divisor other than 0 and -1 (which exit: NULL, and the one overflow).
	VDiv
	VRem
	// VNot: r[C] = 1 if r[A] is the integer 0, 0 for any other integer.
	VNot
	// VCmpBr is a VCmpSet at its pc fused with the VIf at the next that tests
	// its result: compare r[B] Cond r[A], store the 0 or 1 in r[C] unless Dead,
	// and jump to T when the result is Imm. The next pc is VFused.
	VCmpBr
	// VNotBr is a VNot fused the same way: r[C] = !r[A] unless Dead, jump to T
	// when that is Imm.
	VNotBr
	// VFused: the second half of the fused instruction at the pc before. It
	// emits nothing in the run, and nothing jumps to it; an entry at it runs
	// the test it was (VFusedTest) in a block of its own.
	VFused
	// VResultRow: OP_ResultRow of the B registers from A. Native code copies
	// them (a shallow copy, pointers shared) into the engine's row buffer
	// (ProgArgs.RowBuf) when it has room and the write barrier is off. A
	// machine that hands each row over returns to Go with Yield set and PC the
	// next pc, and resumes native code there; one that gathers its rows goes
	// on natively, the row's header written into its list (RowsBuf). Anything
	// else exits at the pc.
	VResultRow
	// VIntGuard: go on when r[A] is an INTEGER, exit at the pc otherwise. It
	// changes nothing: the engine lowers an OP_Affinity that is the identity on
	// an INTEGER to it, so only any other value needs the VDBE.
	VIntGuard
	// VRowid: r[C] = the rowid of cursor A's current row, from its rowid list
	// (VCursor.Rowids); a cursor with none, or past its rows, exits.
	VRowid
	// VInSet: r[C] = 1 when the INTEGER in r[A] is among the ascending
	// INTEGERs from r[B] on, counted by the INTEGER in r[Imm]; else 0. A
	// non-INTEGER operand, or a count that is not a non-negative INTEGER,
	// exits.
	VInSet

	// ---- REAL arms, which the engine chooses where an operand is a REAL.

	// VReal: r[A] = the REAL whose bits are Imm.
	VReal
	// VFAdd, VFSub, VFMul, VFDiv: r[C] = r[B] op r[A] in floating point, each
	// operand an INTEGER (read as a double) or a REAL, at least one a REAL.
	// Two INTEGERs, any other operand, a zero divisor and a NaN result (each
	// NULL or the INTEGER arm) exit.
	VFAdd
	VFSub
	VFMul
	VFDiv
	// VFCmpJump, VFCmpSet, VFCmpBr: VCmpJump, VCmpSet and VCmpBr over REALs,
	// and over INTEGERs a double holds exactly; any other operand exits.
	VFCmpJump
	VFCmpSet
	VFCmpBr
	// VMath: r[C] = Kernel(r[A]), or Kernel(r[A], r[A+1]) when B is 2, each
	// argument a REAL or (Imm zero) an INTEGER read as a double. Any other
	// argument, a NaN answer and the kernel's own Exit exit.
	VMath

	// ---- TEXT arms.

	// VSubstr: r[C] = substr(r[A], r[A+1], r[A+2]) where that is one ASCII
	// character: r[A] a TEXT whose first y bytes are ASCII and none NUL, r[A+1]
	// the INTEGER y, from 1 to the TEXT's length, and r[A+2] the INTEGER 1.
	// The answer is the byte at y-1, as one byte of the table at Imm (the
	// engine's own, as its substr answers it). Anything else exits, as does a
	// write barrier that is on. The bytes already checked in this entry are
	// not checked again (ProgArgs.TextPtr).
	VSubstr
	// VTextCmpJump, VTextCmpSet: VCmpJump and VCmpSet for Cond E or NE over
	// two TEXTs, equal when their bytes are, or two INTEGERs. Any other pair
	// exits, as do two TEXTs when ProgArgs.UTF16 is set: the engine compares
	// those converted.
	VTextCmpJump
	VTextCmpSet

	// ---- Cursor lifetime.

	// VOpen: reopen cursor A over Rows, on no row until a VRewind (Pos is
	// N, so a VNext falls through as the VDBE's advance of an unfilled cursor
	// does). Only a cursor whose VCursor.Reopen the engine set; any other
	// exits.
	VOpen
	// VClose: close cursor A (Ok zero), when VCursor.Reopen is set; any other
	// exits.
	VClose
)

// sdivMagic is the multiplier m and shift s with which n/d, truncated, is
// (mulhs(m, n) [+ n when m < 0]) >> s, plus 1 when n < 0 -- for a divisor
// from 2 that is not a power of two (Hacker's Delight, 10-1).
func sdivMagic(d int64) (m int64, s uint) {
	const two63 = uint64(1) << 63
	ad := uint64(d)
	anc := two63 - 1 - two63%ad
	p := 63
	q1, r1 := two63/anc, two63-two63/anc*anc
	q2, r2 := two63/ad, two63-two63/ad*ad
	for {
		p++
		q1, r1 = 2*q1, 2*r1
		if r1 >= anc {
			q1, r1 = q1+1, r1-anc
		}
		q2, r2 = 2*q2, 2*r2
		if r2 >= ad {
			q2, r2 = q2+1, r2-ad
		}
		if delta := ad - r2; q1 > delta || q1 == delta && r1 != 0 {
			break
		}
	}
	return int64(q2 + 1), uint(p - 64)
}

// vmIntCmp is a VTextCmpJump or VTextCmpSet as the VCmpJump or VCmpSet it is
// when an operand is known to be an INTEGER: the other must be one too.
func vmIntCmp(in VInsn) VInsn {
	if in.Op == VTextCmpJump {
		in.Op = VCmpJump
	} else {
		in.Op = VCmpSet
	}
	return in
}

// VInsn is one instruction of a VM program: the one at its index's pc.
type VInsn struct {
	Op VOp
	// A VFused keeps the operands of the test it was (A, C, T and Imm of a
	// VIf), so an entry at its pc can run that test alone.
	A, B, C int
	T       int
	Imm     int64
	Cond    Cond
	// Dead: no instruction reads the register this one writes before it is
	// written again (the fusion pass's liveness), so its store may be skipped.
	Dead bool
	// Skip, on a VResultRow that gathers (an UPDATE's records), is the
	// registers A+i (bit i) it does not copy: their slots in the row are left
	// as they were, and nothing here reads them, so the instructions that set
	// them may be dead. The engine sets it only where it does not read those
	// slots and does not need those registers.
	Skip uint64
	// Kernel is VMath's function.
	Kernel *fir.Func
}

// VCursor is native code's view of one materialized cursor: the rows the
// cursor holds, and its position. The engine fills it before every entry
// (from the cursor, if the cursor is a kind native code may read -- Ok) and
// reads Pos/Last back after every exit, if Dirty.
//
// Native code writes only Pos, Last and Dirty, all plain integers, so no
// write barrier is needed; it never changes the cursor's rows, its current
// row slice, its rowid or any pointer. The engine does that when it syncs
// (Dirty), which is why an exit may leave a cursor "ahead" of the Go object.
//
// The fields are at the offsets VCursorOff* name, asserted by TestVCursorLayout.
type VCursor struct {
	Rows  unsafe.Pointer // first element of the cursor's [][]Value (24-byte headers)
	N     int64          // how many rows
	Pos   int64          // index of the current row; -1 before the first
	Last  int64          // the last index native code advanced onto a row, or -1
	Ok    int64          // non-zero: native code may read and advance this cursor
	Dirty int64          // non-zero: native code changed Pos
	// Rewind is non-zero when a rewind of the cursor is a reset of Pos: it needs
	// no new rows built (a cursor that re-seeks on every rewind does).
	Rewind int64
	// Adv is non-zero when native code may advance the cursor (VNext). A cursor
	// the engine can only read -- a streamed scan's current row, copied into a
	// one-row list for the length of the call -- has Adv and Rewind zero, so
	// native code never moves it.
	Adv int64
	// Mask has bit c set for each column c < 64 native code may read; a
	// column outside it exits. A materialized cursor has them all. A copied
	// row has only the columns the engine decoded for this entry.
	Mask uint64
	// Rowids is the first element of the cursor's []uint64 of rowids, one per
	// row, or nil: VRowid exits.
	Rowids unsafe.Pointer
	// Reopen is non-zero when an open of the cursor (VOpen) would give it
	// Rows again, so native code may close and reopen it: Rows, N, Rowids and
	// Mask are then the reopened cursor's even while it is closed (Ok zero).
	// The engine's value says what state it handed the cursor over in.
	Reopen int64
	// State is 1 once native code has reopened the cursor (VOpen).
	State int64
}

const (
	VCursorSize     = 96
	VCursorOffRows  = 0
	VCursorOffN     = 8
	VCursorOffPos   = 16
	VCursorOffLast  = 24
	VCursorOffOk    = 32
	VCursorOffDirty = 40
	VCursorOffRew   = 48
	VCursorOffAdv   = 56
	VCursorOffMask  = 64
	VCursorOffRids  = 72
	VCursorOffReop  = 80
	VCursorOffState = 88
	// sliceHeaderSize is the size of a []Value header in the rows array.
	sliceHeaderSize = 24
)

// ValueLayout is where a register's fields live, which only the engine knows.
type ValueLayout struct {
	Size     int // bytes per register
	OffTyp   int // the type byte
	OffI     int // int64
	OffF     int // float64
	OffS     int // the string/blob data pointer; nil means no string or blob
	TypNull  byte
	TypInt   byte
	TypFloat byte
	TypText  byte
}

// VMFuel is how many backward jumps run between two returns to Go.
const VMFuel = 1 << 20
