package engine

import (
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// The JITted VDBE: internal/jit compiles every instruction it has a native
// form for; run() enters that code at any pc and interprets instructions
// it cannot execute (unsupported opcodes, types, overflows, etc.).

// vmJIT is one program's native code.
type vmJIT struct {
	code   *jit.Code
	insns  *Instruction // the program it was compiled from: run() checks identity
	n      int
	nReg   int
	entry  []bool        // the pcs the VDBE enters native code at
	marked []Instruction // the program with OpJIT at each entry: what run() dispatches on
}

// vmJITEnabled controls the whole-program JIT (off with WithoutJIT).
var vmJITEnabled = jitEnabled

// noVMJIT caches when a Program has no native code.
var noVMJIT = &vmJIT{}

// jitCode is prog's native code, compiled on its first run and cached on the
// Program -- which the plan cache keeps, so a prepared statement compiles once.
// nil when it has none.
//
// The cache is checked against prog.Insns rather than trusted: a Program
// copied by value (limit_offset_halt.go) and given new instructions carries
// the old pointer, and compiles its own.
func (prog *Program) jitCode() *vmJIT {
	if len(prog.Insns) == 0 {
		return nil
	}
	if jx := (*vmJIT)(atomic.LoadPointer(&prog.vmJIT)); jx != nil && (jx == noVMJIT || jx.insns == &prog.Insns[0] && jx.n == len(prog.Insns)) {
		if jx == noVMJIT {
			return nil
		}
		return jx
	}
	jx := compileVMJIT(prog)
	if jx == nil {
		atomic.StorePointer(&prog.vmJIT, unsafe.Pointer(noVMJIT))
		return nil
	}
	atomic.StorePointer(&prog.vmJIT, unsafe.Pointer(jx))
	return jx
}

var valueLayout = jit.ValueLayout{
	Size:    int(unsafe.Sizeof(Value{})),
	OffTyp:  int(unsafe.Offsetof(Value{}.Typ)),
	OffI:    int(unsafe.Offsetof(Value{}.I)),
	OffF:    int(unsafe.Offsetof(Value{}.F)),
	OffS:    int(unsafe.Offsetof(Value{}.S)),
	TypNull: byte(Null),
	TypInt:  byte(Int),
}

// compileVMJIT compiles prog, or returns nil: the JIT off, no emitter for this
// platform, or nothing in the program it could run natively. nil means the
// VDBE runs every instruction, which is always correct.
func compileVMJIT(prog *Program) *vmJIT {
	if !vmJITEnabled || len(prog.Insns) == 0 {
		return nil
	}
	vp := make([]jit.VInsn, len(prog.Insns))
	entry := make([]bool, len(prog.Insns))
	native := 0
	for pc := range prog.Insns {
		vp[pc] = lowerVM(&prog.Insns[pc], prog.NReg)
		if vp[pc].Op != jit.VExit {
			entry[pc] = true
			native++
		}
	}
	if native == 0 {
		return nil
	}
	// The VDBE enters only where the code would then do at least
	// vmJITMinWork before handing control back. Every block stays reachable
	// from native code either way.
	//
	// A program left with no entry at all gets no code: its run loop then pays
	// nothing for the JIT, not even the check.
	entries := 0
	for pc := range entry {
		if entry[pc] && nativeRun(vp, pc) < vmJITMinWork {
			entry[pc] = false
		}
		if entry[pc] {
			entries++
		}
	}
	if entries == 0 {
		return nil
	}
	b, err := jit.EmitVM(vp, prog.NReg, valueLayout)
	if err != nil {
		return nil
	}
	code, err := jit.Map(b)
	if err != nil {
		return nil
	}
	marked := append([]Instruction(nil), prog.Insns...)
	for pc, e := range entry {
		if e {
			marked[pc].Op = OpJIT
		}
	}
	return &vmJIT{code: code, insns: &prog.Insns[0], n: len(prog.Insns), nReg: prog.NReg, entry: entry, marked: marked}
}

var (
	vmArith = map[OpCode]jit.VOp{OpAdd: jit.VAdd, OpSubtract: jit.VSub, OpMultiply: jit.VMul,
		OpBitAnd: jit.VAnd, OpBitOr: jit.VOr, OpShiftLeft: jit.VShl, OpShiftRight: jit.VShr}
	vmCond = map[OpCode]jit.Cond{OpEq: jit.CondE, OpNe: jit.CondNE, OpLt: jit.CondL,
		OpLe: jit.CondLE, OpGt: jit.CondG, OpGe: jit.CondGE}
)

// vmJITMinWork is the least native work worth an entry from the VDBE, in
// nativeRun's units. An entry and its exit cost ~4.7ns natively (measured on
// a two-SCopy block), about what the VDBE spends interpreting two copies --
// so a run of copies and constants never pays, while one integer opcode, which
// the VDBE spends several ns on, does once anything rides along with it. The
// platform's figure is jit.VMMinWork: an entry on js/wasm crosses into the JS
// host and back, which costs more than a native call.
var vmJITMinWork = jit.VMMinWork

// nativeRun weighs the native instructions from pc along the fall-through and
// unconditional-jump path, up to vmJITMinWork: a copy or a constant is 1, a
// jump nothing, anything that computes or tests 2.
func nativeRun(vp []jit.VInsn, pc int) int {
	w := 0
	for steps := 0; w < vmJITMinWork && steps < 64 && pc >= 0 && pc < len(vp) && vp[pc].Op != jit.VExit; steps++ {
		switch vp[pc].Op {
		case jit.VGoto:
			pc = vp[pc].T
			continue
		case jit.VCopy, jit.VInt:
			w++
		default:
			w += 2
		}
		pc++
	}
	return w
}

// lowerVM is in's native form, or VExit. Each case is the INTEGER arm of the
// VDBE's own case for that opcode (vdbe.go), which the VDBE also tests first;
// every other operand type exits to it.
func lowerVM(in *Instruction, nReg int) jit.VInsn {
	exit := jit.VInsn{Op: jit.VExit}
	ok := func(regs ...int) bool {
		for _, r := range regs {
			if r < 0 || r >= nReg {
				return false
			}
		}
		return true
	}
	switch in.Op {
	case OpInit, OpGoto:
		return jit.VInsn{Op: jit.VGoto, T: in.P2}
	case OpInteger:
		if ok(in.P2) {
			return jit.VInsn{Op: jit.VInt, A: in.P2, Imm: int64(in.P1)}
		}
	case OpInt64:
		if v, isInt := in.P4.(int64); isInt && ok(in.P2) {
			return jit.VInsn{Op: jit.VInt, A: in.P2, Imm: v}
		}
	case OpSCopy, OpCopy:
		// copyValue differs from a plain copy only for a string or blob, which
		// the native copy refuses.
		if ok(in.P1, in.P2) {
			return jit.VInsn{Op: jit.VCopy, A: in.P1, B: in.P2}
		}
	case OpAdd, OpSubtract, OpMultiply, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		if ok(in.P1, in.P2, in.P3) {
			return jit.VInsn{Op: vmArith[in.Op], A: in.P1, B: in.P2, C: in.P3}
		}
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		// Two INTEGERs answer before NULL handling, affinity or collation
		// (compareOp's first arm, vdbe.c:2288), so P4 and P5 matter only for
		// the store-the-result form, which stays the VDBE's.
		if in.P5&p5StoreP2 == 0 && ok(in.P1, in.P3) {
			return jit.VInsn{Op: jit.VCmpJump, A: in.P1, B: in.P3, T: in.P2, Cond: vmCond[in.Op]}
		}
	case OpIf, OpIfNot:
		if ok(in.P1) {
			want := int64(0)
			if in.Op == OpIf {
				want = 1
			}
			return jit.VInsn{Op: jit.VIf, A: in.P1, T: in.P2, C: in.P3, Imm: want}
		}
	case OpGosub:
		if ok(in.P1) {
			return jit.VInsn{Op: jit.VGosub, A: in.P1, T: in.P2}
		}
	case OpReturn:
		if ok(in.P1) {
			return jit.VInsn{Op: jit.VReturn, A: in.P1}
		}
	}
	return exit
}

// enter runs native code from pc and returns the pc the VDBE runs next.
func (jx *vmJIT) enter(regs []Value, pc int) int {
	if len(regs) < jx.nReg || jx.nReg == 0 {
		return pc
	}
	args := jit.ProgArgs{Regs: (*int64)(unsafe.Pointer(&regs[0])), PC: int64(pc)}
	jx.code.Call2(&args)
	return int(args.PC)
}
