package jit

import "unsafe"

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
//   - Native code writes only the non-pointer words of a Value, and only into
//     a Value whose S is nil. Writing S would need the GC's write barrier,
//     which generated code cannot call; a destination holding a string or blob
//     exits instead, and the VDBE's ordinary assignment clears it.
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
	// VCopy: r[B] = r[A], a shallow copy of a Value with no string or blob.
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
	// emits nothing; native code never enters or jumps to it.
	VFused
)

// VInsn is one instruction of a VM program: the one at its index's pc.
type VInsn struct {
	Op      VOp
	A, B, C int
	T       int
	Imm     int64
	Cond    Cond
	// Dead: no instruction reads the register this one writes before it is
	// written again (the fusion pass's liveness), so its store may be skipped.
	Dead bool
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
}

const (
	VCursorSize     = 72
	VCursorOffRows  = 0
	VCursorOffN     = 8
	VCursorOffPos   = 16
	VCursorOffLast  = 24
	VCursorOffOk    = 32
	VCursorOffDirty = 40
	VCursorOffRew   = 48
	VCursorOffAdv   = 56
	VCursorOffMask  = 64
	// sliceHeaderSize is the size of a []Value header in the rows array.
	sliceHeaderSize = 24
)

// ValueLayout is where a register's fields live, which only the engine knows.
type ValueLayout struct {
	Size    int // bytes per register
	OffTyp  int // the type byte
	OffI    int // int64
	OffF    int // float64
	OffS    int // the string/blob data pointer; nil means no string or blob
	TypNull byte
	TypInt  byte
}

// VMFuel is how many backward jumps run between two returns to Go.
const VMFuel = 1 << 20
