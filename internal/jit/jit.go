// Package jit emits and executes machine code at runtime: x86-64 or AArch64.
// The JIT is the second sanctioned execution path (after the VDBE).
//
// Architecture rules (load-bearing, not stylistic):
//
//   - one pointer in, at a fixed offset layout (Args). Generated code reaches
//     everything through it, so the ABI is a struct rather than a calling
//     convention;
//   - every address in Args is a real Go pointer, never a uintptr, because the
//     GC scans this struct through callKernel's signature. A uintptr is
//     invisible to it and the slice behind it could be collected mid-kernel;
//   - Args is APPEND-ONLY. Generated code addresses fields by byte offset, so
//     inserting one silently repoints every load in every kernel;
//   - generated code makes no Go call and grows no stack, which is what lets
//     the trampoline be NOSPLIT -- and is also why a kernel cannot be
//     async-preempted, so each one must be bounded work;
//   - the goroutine's register is never touched: R14 on amd64, X28 on arm64.
//     Go pins g there, and a kernel that clobbered it corrupts g, which does
//     not fail near its cause. arm64 additionally leaves X27 (the assembler's
//     temporary) and X18 (darwin's platform register) alone.
package jit

import "strconv"

// Args is the single struct a generated kernel reads, at fixed byte offsets.
// APPEND ONLY -- see the package comment.
type Args struct {
	A   *int64 // first column block                 (offset 0)
	C   *int64 // second column block, or nil        (offset 8)
	N   int64  // rows                                (offset 16)
	XA  int64  // first comparison constant           (offset 24)
	XC  int64  // second comparison constant          (offset 32)
	Out *int64 // where the kernel writes its answer  (offset 40)
	// EmitFilterSumSIMD only: the column summed over matching rows, and where
	// the number of matching rows goes (Out receives the sum).
	V    *int64 // (offset 48)
	Out2 *int64 // (offset 56)
}

// Byte offsets of Args' fields, asserted by TestArgsLayout. Generated code uses
// these numbers directly.
const (
	OffA   = 0
	OffC   = 8
	OffN   = 16
	OffXA  = 24
	OffXC  = 32
	OffOut = 40
	OffV    = 48
	OffOut2 = 56
)

// Cond is a condition code. Its numeric values are x86 Jcc nibbles, which is an
// accident of which architecture was written first -- nothing outside an
// encoder may depend on them, and asm_arm64.go's armCond is the translation
// that keeps that true.
type Cond uint8

const (
	CondE  Cond = 0x4 // ==
	CondNE Cond = 0x5 // !=
	CondL  Cond = 0xC // <   signed
	CondGE Cond = 0xD // >=  signed
	CondLE Cond = 0xE // <=  signed
	CondG  Cond = 0xF // >   signed
)

// condOverflow is x86's OF condition (the 0 nibble), used only by SETO after an
// ADD. It is not part of the portable Cond set -- no comparison produces it and
// arm64's armCond has no case for it -- so it lives here with that stated
// rather than sitting in the enum looking like an ordering.
const condOverflow Cond = 0x0

// condSign is x86's SF condition, used only by CMOVS in POpAbs.
const condSign Cond = 0x8

// Negate returns the condition that is true exactly when c is false, which is
// what a filter loop jumps on: "skip this row unless it matches".
func (c Cond) Negate() Cond {
	switch c {
	case CondE:
		return CondNE
	case CondNE:
		return CondE
	case CondL:
		return CondGE
	case CondGE:
		return CondL
	case CondLE:
		return CondG
	default:
		return CondLE
	}
}

// ---- The PROGRAM JIT: machine code for a compiled loop body, not a fixed
// kernel.
//
// EmitFilterCount and EmitFilterCountSIMD each answer ONE shape, chosen when
// the emitter was written: a count with one or two integer comparisons. Every
// further shape -- a third predicate, an OR, arithmetic in the comparison, a
// sum instead of a count -- needed a new matcher and a new emitter, which is a
// combinatorial way to build a JIT.
//
// This compiles a small INSTRUCTION LIST instead. The engine lowers a
// recognised VDBE loop body into it, and what runs is machine code for that
// particular body. Adding a shape becomes a matter of lowering one more opcode
// rather than writing another kernel.
//
// The IR is deliberately tiny and int64-only: loads from a column block,
// constants, comparisons, boolean combination, and an accumulate. That is the
// subset a columnar scan actually spends its time in, and keeping it small is
// what keeps the emitter something one person can check.

// POp is a program-JIT opcode.
type POp uint8

const (
	// POpLoadCol: r[A] = Col[B][i] -- the row's value from column block B.
	POpLoadCol POp = iota
	// POpLoadReg: r[A] = r[B]. Used to seed a constant the caller parked in
	// the register file before the call.
	POpLoadReg
	// POpCmp: r[A] = 1 when r[B] <cond> r[C], else 0.
	POpCmp
	// POpAnd: r[A] = r[B] & r[C], over the 0/1 values POpCmp produces.
	POpAnd
	// POpOr: r[A] = r[B] | r[C].
	POpOr
	// POpSkipIfZero: when r[A] == 0, abandon this row and start the next.
	POpSkipIfZero
	// POpAccCount: acc += 1.
	POpAccCount
	// POpAccSum: acc += r[A], and r[B]++ to count the rows that reached it.
	//
	// The row count is not bookkeeping: sum() of NO rows is NULL and not 0, and
	// the accumulator alone cannot tell "no rows" from "rows that summed to
	// zero". Counting in the same pass is what makes the distinction without a
	// second walk.
	//
	// Signed overflow is detected and recorded -- the caller declines, because
	// SQLite switches sum() to floating point there and this cannot.
	POpAccSum
	// POpSetConst: r[A] = B. The VDBE's OpInteger, and the constant halves of
	// the three-valued-logic epilogue OR and BETWEEN compile into.
	POpSetConst
	// POpJump: go to IR index A, or to the next row when A is ProgNextRow.
	POpJump
	// POpJumpIfZero / POpJumpIfNotZero: the same, conditional on r[B].
	// These are the VDBE's OpIfNot and OpIf, and having them is what lets a
	// whole compiled expression be lowered instead of a fixed predicate list --
	// an OR is a branch, not a shape.
	POpJumpIfZero
	POpJumpIfNotZero
	// POpNot: r[A] = 1 when r[B] is zero, else 0 -- the VDBE's OpNot over
	// values that cannot be NULL.
	POpNot
	// POpAdd / POpSub / POpMul: r[A] = r[B] <op> r[C], with signed overflow
	// recorded the way POpAcSum records it.
	//
	// Overflow has to be detected, not ignored: SQLite promotes an integer
	// expression that overflows to floating point, and a wrapped int64 here
	// would be a wrong answer rather than a slow one. The caller declines and
	// the interpreter, which does promote, answers instead.
	POpAdd
	POpSub
	POpMul

	// POpAccMin / POpAccMax: acc = min(acc, r[A]) / max(acc, r[A]), and r[B]++
	// to count the rows that reached it.
	//
	// The accumulator is SEEDED at the top of the program -- MaxInt64 for a
	// min, MinInt64 for a max -- rather than tested for "first row". That turns
	// the whole accumulate into one compare and one conditional move, and it
	// costs nothing in correctness because the seed can never win: a program
	// that matched no rows is detected by r[B] being zero, and the caller
	// answers NULL without looking at the accumulator at all.
	POpAccMin
	POpAccMax

	// POpRem: r[A] = r[B] % r[C], truncating like C's %. A zero divisor is
	// SQL's NULL, which the IR cannot hold, so it is recorded the way an
	// overflow is and the caller declines; a divisor of -1 gives 0 (vdbe.c's
	// "if( iA==-1 ) iA = 1"), which also keeps MinInt64 % -1 from trapping.
	POpRem

	// POpAbs: r[A] = |r[B]|. abs(MinInt64) is SQLite's "integer overflow"
	// error, which the program cannot raise, so it is flagged like an
	// overflow and the caller declines; the VDBE then reports the error.
	POpAbs

	// POpService: hand the current row to the caller's service A (0-based) and
	// stop. The program returns with ProgArgs.PC = A+1 and ProgArgs.Row = the
	// loop index, the accumulator and overflow flag stored as at the end; the
	// caller performs the service on that row -- whatever the engine has no
	// native form for -- writing any results into the register file, and calls
	// again with PC unchanged, which resumes right after this instruction on
	// the same row. A normal finish leaves PC = 0. The program never calls
	// out: native code returns, Go runs the service, native code is entered
	// again, so Go's stack, GC and preemption only ever see Go frames.
	POpService

	// POpTextLen: r[A] = length() of the TEXT cell in column slot B -- its
	// characters before the first NUL. Pure ASCII is counted natively, a
	// SIMD chunk at a time; a cell with any byte >= 0x80 jumps to IR index C
	// instead, where the lowering puts the service that counts it the way
	// SQLite's UTF-8 reader does.
	POpTextLen

	// POpEmitRow appends the current row index to Sel and counts it in the
	// accumulator, which the epilogue leaves in *Out. It is what turns a
	// compiled predicate into a SELECTION rather than a tally: the program
	// answers WHICH rows matched, and the caller visits only those.
	//
	// The index written is the loop's own, which counts UP from -n to 0, so
	// the caller adds n. That is deliberate -- it keeps the emitted store to
	// one instruction on both architectures rather than paying an add per
	// selected row to make the number prettier.
	//
	// A selection is a PERFORMANCE HINT and nothing else: the statement's real
	// predicate stays in the VDBE program and runs again on every row this
	// hands back. A row selected that should not have been costs time; the
	// answer cannot change. That asymmetry is the whole safety argument, and
	// it is why this may be reached for shapes an accumulator could not be.
	POpEmitRow
)

// ProgNextRow is the jump target meaning "abandon this row and start the next".
// The VDBE's equivalent is a jump to the loop's OpNext.
const ProgNextRow = -1

// ProgInsn is one instruction of the program JIT's IR.
type ProgInsn struct {
	Op      POp
	A, B, C int
	Cond    Cond
}

// MaxProgCols is how many distinct column blocks one compiled program may
// read. They are preloaded into registers outside the row loop, so the limit is
// the register file's, and four covers a filter of three predicates plus the
// column an aggregate sums.
const MaxProgCols = 4

// ProgArgs is the program JIT's ABI: one pointer in, everything reached from
// it, exactly as Args is for the fixed kernels.
//
// Col is an ARRAY rather than a slice of pointers on purpose. The GC scans this
// struct through callKernel's signature, so every pointer the generated code
// follows has to live inside it as a real Go pointer; a slice header would be
// scanned too, but an array keeps the offsets constant, which is what the
// emitted loads need.
//
// APPEND ONLY, for the same reason Args is.
type ProgArgs struct {
	Col      [MaxProgCols]*int64 // column bases          (offset 0)
	N        int64               // rows                  (32)
	Regs     *int64              // the register file      (40)
	Out      *int64              // accumulator out        (48)
	Overflow *int64              // set non-zero by POpAccSum on overflow (56)
	Sel      *int64              // POpEmitRow's selection buffer          (64)
	PC       int64               // EmitVM: the pc to enter at, and on return the pc to resume at (72); EmitProgram: the service to resume after, 1-based, 0 to start
	Row      int64               // EmitProgram: the loop index a POpService stopped at (80)
	// Heap is, per column slot holding TEXT cells, the base of the bytes those
	// cells point into (88..). A cell is offset | length<<32.
	Heap [MaxProgCols]*byte
}

// Byte offsets of ProgArgs' fields, asserted by TestProgArgsLayout.
const (
	POffCol      = 0
	POffN        = MaxProgCols * 8
	POffRegs     = POffN + 8
	POffOut      = POffRegs + 8
	POffOverflow = POffOut + 8
	POffSel      = POffOverflow + 8
	POffPC       = POffSel + 8
	POffRow      = POffPC + 8
	POffHeap     = POffRow + 8
)

// progHasService reports whether a program can stop for a service, and so
// needs the re-entry dispatch.
func progHasService(insns []ProgInsn) bool {
	for _, in := range insns {
		if in.Op == POpService {
			return true
		}
	}
	return false
}

// svcLabel names the resume point after service k.
func svcLabel(k int) string { return "sr" + strconv.Itoa(k) }
