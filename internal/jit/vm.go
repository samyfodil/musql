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
	// VTextCmpJump, VTextCmpSet: VCmpJump and VCmpSet over two TEXTs, ordered
	// as bytes.Compare orders their bytes, or two INTEGERs. Any other pair
	// exits, as do two TEXTs when ProgArgs.UTF16 is set: the engine compares
	// those converted. Under Imm 1 (a numeric affinity) a TEXT that is a whole
	// number exits too. Without VMTextOrder only Cond E and NE, and no Imm.
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

	// ---- Segment seeks (VMSegCursor).

	// VSeekRowid: find the row whose rowid is the INTEGER in r[B] among the
	// segments of cursor A's table (VCursor.Tab) and put the cursor on it in
	// segment mode, as the VDBE's OP_SeekRowidHint and OP_Rewind would, then
	// fall through to the VSeekRewind; jump to T when no row has it. A key
	// that is no INTEGER, a cursor with no Tab, and the write barrier on exit.
	VSeekRowid
	// VSeekRewind: the OP_Rewind after a VSeekRowid, which has done its work:
	// go on (State 2 becomes 4). Reached any other way, it exits.
	VSeekRewind
	// VSeekIdx: put cursor A in segment mode on the rows of its table whose
	// indexed column (VCursor.Idx) holds the INTEGER in r[B], in the index's
	// row order, as the VDBE's OP_SeekIndexHint, OP_AutoIndexOrder and
	// OP_Rewind of that key would, then fall through to the VSeekRewind; jump
	// to T when no row does. A key that is no INTEGER, a cursor with no Idx, a
	// key a later segment's range also holds (its rows may span segments) and
	// the write barrier exit. Key and State are as VSeekRowid leaves them;
	// Served gains, at each VSeekIdx, the rows the last seek's run served up
	// to the cursor's row -- State 5 being a list the engine seeked and handed
	// over on its row Last, with Served set so the gain is the rows past it.
	VSeekIdx

	// VText: r[A] = the TEXT of the B bytes at Imm, which the engine keeps
	// alive and never writes, shared as the VDBE's OP_String8 shares its
	// constant. The write barrier on exits. (VMTextOrder)
	VText
	// VLike: r[C] = r[A] LIKE r[B] (NOT LIKE when Pat.Not), 1 or 0, for the
	// pattern Pat, which r[B] must be: the TEXT sharing Pat's constant bytes.
	// Matched as the engine's likePlan matches, ASCII letters in either case.
	// A non-TEXT operand, any other pattern, a NUL in r[A]'s TEXT and
	// ProgArgs.LikeCS (case_sensitive_like) exit. (VMTextOrder)
	VLike
	// VInSub: r[C] = 1 when the INTEGER in r[A] is in the set
	// ProgArgs.InSets[B] holds, else 0; the other way round under Imm 1 (NOT
	// IN). An empty set answers without reading r[A]. A set not Ready, any
	// other r[A], and a miss in a set holding a NULL exit. (VMTextOrder)
	VInSub
)

// VInSubSet is an uncorrelated IN subquery's rows as VInSub probes them, which
// the engine publishes at every entry: Keys the INTEGERs ascending, N of
// them; HasNull, Empty and Ready flags.
type VInSubSet struct {
	Keys    unsafe.Pointer
	N       int64
	HasNull int64
	Empty   int64
	Ready   int64
}

const (
	VInSubSize       = 40
	VInSubOffKeys    = 0
	VInSubOffN       = 8
	VInSubOffHasNull = 16
	VInSubOffEmpty   = 24
	VInSubOffReady   = 32
)

// VLikePat is the LIKE pattern a VLike matches: a constant with no '_', no
// NUL and nothing past ASCII, split at its '%'s as the engine's likePlan
// splits it.
type VLikePat struct {
	Src    uintptr  // the constant's bytes, which the pattern register shares
	SrcLen int      // and their length
	Pieces [][]byte // the text between the '%'s, none empty (but an exact "")
	Head   bool     // the pattern does not start with '%'
	Tail   bool     // nor end with one
	Exact  bool     // it has no '%': the text must be Pieces[0]
	Not    bool     // NOT LIKE
}

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
	// Pat is VLike's pattern.
	Pat *VLikePat
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
	// native code never moves it. Adv 2 says the cursor has rows past the N
	// handed over: a VNext off the end exits rather than falling through.
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
	// Seg, when not nil, is the cursor's current segment (a *VSeg): a
	// VColumn marked segment-capable (Imm 1) reads row Pos's cell from the
	// column blocks there instead of from Rows, and N is where the segment's
	// rows the cursor walks end. Only where VMSegCursor.
	Seg unsafe.Pointer
	// Tab, when not nil, is the segments of the cursor's table (a *VTab),
	// every row of which they hold: what VSeekRowid searches. Key is the last
	// key it searched for, State 2 when it found the row (4 once its
	// VSeekRewind ran) and 3 when not; Served counts its finds.
	Tab    unsafe.Pointer
	Key    int64
	Served int64
	// Sel, when not nil, is in segment mode the segment row of each position:
	// a list of N ints, the row at Pos being Sel[Pos] (an index seek's rows,
	// a scan in a key's order). Nil, the row is Pos.
	Sel unsafe.Pointer
	// Idx, when not nil, is the equality indexes VSeekIdx searches: the first
	// of the VIdxs of the table's segments, in order.
	Idx unsafe.Pointer
}

// VIdx is one segment's equality index over a column, as VSeekIdx searches
// it: Vals the NVals distinct INTEGERs ascending, from Lo to Hi, Starts the
// NVals+1 int32s where each value's rows begin in Rows, Rows the segment rows
// (ints) in the order a seek visits them, Seg the segment's VSeg and Mask the
// columns native code reads from it. Count is how many VIdxs this one and
// those after it in the table's array make.
type VIdx struct {
	Vals   unsafe.Pointer
	NVals  int64
	Starts unsafe.Pointer
	Rows   unsafe.Pointer
	Seg    unsafe.Pointer
	Mask   uint64
	Count  int64
	Lo, Hi int64
}

// VTab is the segments of a table, in the order the engine searches them.
type VTab struct {
	Segs unsafe.Pointer // the first of N VTabSegs
	N    int64
}

// VTabSeg is one segment of a VTab: the rowids from Lo to Hi it holds, its
// Rows, its VSeg, and the columns native code reads from it.
type VTabSeg struct {
	Lo, Hi int64
	Rows   int64
	Seg    unsafe.Pointer
	Mask   uint64
}

// VSeg is native code's view of one segment of a table: per column, where its
// cells are and how they read. The engine builds it once per segment and keeps
// it with the segment, which it outlives no entry.
type VSeg struct {
	Cols   unsafe.Pointer // the first of the segment's VSegCols, column c at c*VSegColSize
	First  int64          // row 0's rowid, when Dense
	Dense  int64          // non-zero: row i's rowid is First+i
	Rowids unsafe.Pointer // the rowid block (uint64s) when not Dense
}

// VSegCol is one column of a VSeg. A column whose Kind is VSegNone is never in
// a segment cursor's Mask.
type VSegCol struct {
	Kind  int64
	Block unsafe.Pointer // the cells: 8 bytes a row, or for VSegText nRows+1 uint32 heap bounds
	Nulls unsafe.Pointer // the NULL bitmap (bit i%8 of byte i/8), or nil: a NULL exits
	Exc   unsafe.Pointer // the exceptions' bitmap (bit i%64 of uint64 i/64), or nil: one exits
	Heap  unsafe.Pointer // VSegText's bytes
	// Plain is Kind for a column with no NULL bitmap and no exceptions, else
	// 0: a VColumn that predicted the kind (T) reads such a cell with one test.
	Plain int64
}

// VSegCol kinds: how a cell reads as a Value.
const (
	VSegNone    = iota
	VSegInt     // an int64 cell, an INTEGER
	VSegFloat   // a double cell, a REAL
	VSegNumeric // a double cell, an INTEGER when integral and within 2^53, else a REAL
	VSegIntReal // an int64 cell read as a REAL (a column of REAL affinity)
	VSegRowid   // the rowid (an INTEGER PRIMARY KEY, stored NULL)
	VSegText    // TEXT between two heap bounds
)

const (
	VCursorSize     = 144
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
	VCursorOffSeg   = 96
	VCursorOffTab   = 104
	VCursorOffKey   = 112
	VCursorOffSrvd  = 120
	VCursorOffSel   = 128
	VCursorOffIdx   = 136
	VIdxOffVals     = 0
	VIdxOffNVals    = 8
	VIdxOffStarts   = 16
	VIdxOffRows     = 24
	VIdxOffSeg      = 32
	VIdxOffMask     = 40
	VIdxOffCount    = 48
	VIdxOffLo       = 56
	VIdxOffHi       = 64
	VIdxSize        = 72
	VTabOffSegs     = 0
	VTabOffN        = 8
	VTabSegSize     = 40
	VTabSegOffLo    = 0
	VTabSegOffHi    = 8
	VTabSegOffRows  = 16
	VTabSegOffSeg   = 24
	VTabSegOffMask  = 32
	VSegOffCols     = 0
	VSegOffFirst    = 8
	VSegOffDense    = 16
	VSegOffRowids   = 24
	VSegColSize     = 48
	VSegColOffKind  = 0
	VSegColOffBlock = 8
	VSegColOffNulls = 16
	VSegColOffExc   = 24
	VSegColOffHeap  = 32
	VSegColOffPlain = 40
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
