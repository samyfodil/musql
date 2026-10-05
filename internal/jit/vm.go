package jit

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
	// VIf: jump to T when r[A] is an integer whose truth is Imm != 0; a NULL
	// jumps when C != 0.
	VIf
	// VGosub: r[A] = this pc, jump to T.
	VGosub
	// VReturn: jump to r[A] + 1, an integer inside the program.
	VReturn
)

// VInsn is one instruction of a VM program: the one at its index's pc.
type VInsn struct {
	Op      VOp
	A, B, C int
	T       int
	Imm     int64
	Cond    Cond
}

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
