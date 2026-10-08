//go:build wasm

package jit

import "fmt"

const (
	opReturn    byte = 0x0F
	opBrTable   byte = 0x0E
	opI32Load8U byte = 0x2D
	opI32Ne     byte = 0x47
	opI64GtU    byte = 0x56
	opI64Shl    byte = 0x86
	opI64ShrS   byte = 0x87
	opIf        byte = 0x04
	opElse      byte = 0x05
)

// MaxVMWasmBytes bounds the module EmitVM produces on wasm. The JS API caps
// a function body at 7,654,321 bytes, and every pc is also a block and a
// br_table entry, which engines spend memory on (v86 measured deep br_tables
// as the cost that matters). A larger program declines and the VDBE runs it.
const MaxVMWasmBytes = 6 << 20

// EmitVM compiles a whole VDBE program to one wasm function, under the
// contract vm.go states.
//
// Layout: a dispatch loop around one block per pc, nested so that block k
// ends right where pc k's code starts, and a br_table on pc picks the block.
// Code then falls from each pc into the next. A forward jump is a br out of
// the blocks in between, so straight-line and forward-branching code never
// goes back through the table; a backward jump or an OP_Return sets pc,
// spends fuel and re-enters the dispatch. An exit stores the pc to resume at
// and returns.
func EmitVM(prog []VInsn, nReg int, lay ValueLayout) ([]byte, error) {
	if lay.OffTyp != 0 || lay.OffI != 8 || lay.OffF != 16 {
		return nil, fmt.Errorf("jit: unexpected Value layout %+v", lay)
	}
	n := len(prog)
	if int64(nReg)*int64(lay.Size) > 1<<30 {
		return nil, fmt.Errorf("jit: %d registers", nReg)
	}
	e := &vmEmitWasm{w: &Wasm{}, lay: lay, n: n}
	w := e.w
	e.regs, e.pc, e.fuel = w.Local(wI32), w.Local(wI32), w.Local(wI64)
	e.t1, e.t2, e.t3, e.ovf = w.Local(wI64), w.Local(wI64), w.Local(wI64), w.Local(wI64)

	w.LoadPtr(POffRegs)
	w.Set(e.regs)
	w.Get(0)
	w.LoadI64(POffPC)
	w.op(opI32WrapI64)
	w.Set(e.pc)
	w.I64(VMFuel)
	w.Set(e.fuel)

	w.op(opLoop, opVoid)  // dispatch
	w.op(opBlock, opVoid) // "end": falls into the exit at n
	for range n {
		w.op(opBlock, opVoid)
	}
	// br_table: pc k -> depth k (block k, innermost is 0); anything else ->
	// depth n, the end block.
	w.Get(e.pc)
	w.op(opBrTable)
	w.body = uleb(w.body, uint64(n))
	for k := range n {
		w.body = uleb(w.body, uint64(k))
	}
	w.body = uleb(w.body, uint64(n))
	for pc, in := range prog {
		w.op(opEnd) // block pc ends: its code follows
		if err := e.insn(pc, in); err != nil {
			return nil, fmt.Errorf("jit: pc %d: %w", pc, err)
		}
	}
	w.op(opEnd) // the end block
	e.exitAt(n)
	w.op(opEnd) // dispatch loop
	if len(w.body) > MaxVMWasmBytes {
		return nil, fmt.Errorf("jit: program too large for wasm (%d instructions, %d bytes)", n, len(w.body))
	}
	return w.Module(), nil
}

type vmEmitWasm struct {
	w                *Wasm
	lay              ValueLayout
	n                int
	regs, pc         uint32
	fuel, t1, t2, t3 uint32
	ovf              uint32
	cur              int // the pc whose code is being emitted
	extra            int // if-blocks opened inside it
}

// Within pc's code the enclosing blocks are those of pc+1 .. n-1, then the
// end block, then the dispatch loop.
// Every if opened inside it adds one more.
func (e *vmEmitWasm) depthToBlock(t int) uint32 { return uint32(t - e.cur - 1 + e.extra) }
func (e *vmEmitWasm) depthDispatch() uint32     { return uint32(e.n - e.cur + e.extra) }

func (e *vmEmitWasm) addr(r, f int) uint32 { return uint32(r*e.lay.Size + f) }

func (e *vmEmitWasm) loadI64(r, f int) { e.w.Get(e.regs); e.w.LoadI64(e.addr(r, f)) }

func (e *vmEmitWasm) loadTyp(r int) {
	e.w.Get(e.regs)
	e.w.op(opI32Load8U)
	e.w.memarg(0, e.addr(r, e.lay.OffTyp))
}

// exitAt stores pc as the place the VDBE resumes and returns.
func (e *vmEmitWasm) exitAt(pc int) {
	e.w.Get(0)
	e.w.I64(int64(pc))
	e.w.StoreI64(POffPC)
	e.w.op(opReturn)
}

// exitIf exits at pc when the i32 on the stack is non-zero.
func (e *vmEmitWasm) exitIf(pc int) {
	e.w.op(opIf, opVoid)
	e.exitAt(pc)
	e.w.op(opEnd)
}

// A guard pushes an i32 that is non-zero when pc must exit; guards ORs them
// so an instruction has one exit however many guards it has, which keeps
// the function inside the engines' size limit for longer programs.
type guard func(e *vmEmitWasm)

// notInt fails unless r[reg] is an INTEGER.
func notInt(reg int) guard {
	return func(e *vmEmitWasm) {
		e.loadTyp(reg)
		e.w.I32(int32(e.lay.TypInt))
		e.w.op(opI32Ne)
	}
}

// hasS fails unless r[reg] holds no string or blob.
func hasS(reg int) guard {
	return func(e *vmEmitWasm) {
		e.loadI64(reg, e.lay.OffS)
		e.w.op(opI64Eqz)
		e.w.op(opI32Eqz)
	}
}

func (e *vmEmitWasm) guards(pc int, gs ...guard) {
	for i, g := range gs {
		g(e)
		if i > 0 {
			e.w.op(opI32Or)
		}
	}
	e.exitIf(pc)
}

// writeInt makes r[reg] the INTEGER on top of the stack (in local src).
func (e *vmEmitWasm) writeInt(reg int, src uint32) {
	w := e.w
	w.Get(e.regs)
	w.I64(int64(e.lay.TypInt)) // the type as the whole first word
	w.StoreI64(e.addr(reg, 0))
	w.Get(e.regs)
	w.Get(src)
	w.StoreI64(e.addr(reg, e.lay.OffI))
	w.Get(e.regs)
	w.I64(0)
	w.StoreI64(e.addr(reg, e.lay.OffF))
}

// jump goes to target unconditionally: a br for a forward block, the
// dispatch with fuel spent for a backward one, an exit past either end.
func (e *vmEmitWasm) jump(target int) {
	w := e.w
	switch {
	case target < 0 || target >= e.n:
		e.exitAt(target)
	case target > e.cur:
		w.Br(e.depthToBlock(target))
	default:
		e.reenter(func() { w.I32(int32(target)) }, target)
	}
}

// reenter sets pc from push, spends one unit of fuel and dispatches; out of
// fuel it exits at that pc instead. exitPC is the pc when it is a constant,
// or -1 to read it from the pc local.
func (e *vmEmitWasm) reenter(push func(), exitPC int) {
	w := e.w
	push()
	w.Set(e.pc)
	w.Get(e.fuel)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(e.fuel)
	w.Get(e.fuel)
	w.op(opI64Eqz)
	w.op(opIf, opVoid)
	if exitPC >= 0 {
		e.exitAt(exitPC)
	} else {
		w.Get(0)
		w.Get(e.pc)
		w.op(opI64ExtendI32U)
		w.StoreI64(POffPC)
		w.op(opReturn)
	}
	w.op(opEnd)
	w.Br(e.depthDispatch())
}

// jumpIf jumps to target when the i32 on the stack is non-zero.
func (e *vmEmitWasm) jumpIf(target int) {
	if target > e.cur && target < e.n {
		e.w.BrIf(e.depthToBlock(target))
		return
	}
	e.openIf()
	e.jump(target)
	e.closeIf()
}

func (e *vmEmitWasm) openIf()  { e.w.op(opIf, opVoid); e.extra++ }
func (e *vmEmitWasm) closeIf() { e.w.op(opEnd); e.extra-- }

func (e *vmEmitWasm) insn(pc int, in VInsn) error {
	w, L := e.w, e.lay
	e.cur, e.extra = pc, 0
	switch in.Op {
	case VExit:
		e.exitAt(pc)
	case VGoto:
		e.jump(in.T)
	case VInt:
		e.guards(pc, hasS(in.A))
		w.I64(in.Imm)
		w.Set(e.t1)
		e.writeInt(in.A, e.t1)
	case VCopy:
		e.guards(pc, hasS(in.A), hasS(in.B))
		for _, f := range []int{0, L.OffI, L.OffF} {
			w.Get(e.regs)
			e.loadI64(in.A, f)
			w.StoreI64(e.addr(in.B, f))
		}
	case VAdd, VSub, VMul, VAnd, VOr:
		e.guards(pc, notInt(in.B), notInt(in.A), hasS(in.C))
		e.loadI64(in.B, L.OffI)
		w.Set(e.t1)
		e.loadI64(in.A, L.OffI)
		w.Set(e.t2)
		switch in.Op {
		case VAnd, VOr:
			w.Get(e.t1)
			w.Get(e.t2)
			if in.Op == VAnd {
				w.op(opI64And)
			} else {
				w.op(opI64Or)
			}
			w.Set(e.t1)
		default:
			// Overflow exits before anything is written, so the VDBE runs
			// this pc again with its own (promoting) arithmetic.
			op := map[VOp]POp{VAdd: POpAdd, VSub: POpSub, VMul: POpMul}[in.Op]
			w.I64(0)
			w.Set(e.ovf)
			emitCheckedArith(w, op, e.t1, e.t2, e.t3, e.ovf)
			w.Get(e.ovf)
			w.op(opI64Eqz)
			w.op(opI32Eqz)
			e.exitIf(pc)
			w.Get(e.t3)
			w.Set(e.t1)
		}
		e.writeInt(in.C, e.t1)
	case VShl, VShr:
		e.guards(pc, notInt(in.B), notInt(in.A), hasS(in.C))
		e.loadI64(in.A, L.OffI)
		w.Set(e.t2)
		w.Get(e.t2)
		w.I64(63)
		w.op(opI64GtU) // unsigned: a negative amount is huge
		e.exitIf(pc)
		e.loadI64(in.B, L.OffI)
		w.Get(e.t2)
		if in.Op == VShl {
			w.op(opI64Shl)
		} else {
			w.op(opI64ShrS)
		}
		w.Set(e.t1)
		e.writeInt(in.C, e.t1)
	case VCmpJump:
		e.guards(pc, notInt(in.B), notInt(in.A))
		e.loadI64(in.B, L.OffI)
		e.loadI64(in.A, L.OffI)
		w.op(scalarCmp(in.Cond))
		e.jumpIf(in.T)
	case VIf:
		// An INTEGER jumps on its truth; a NULL jumps when C says so; anything
		// else is the VDBE's.
		e.loadTyp(in.A)
		w.I32(int32(L.TypInt))
		w.op(opI32Ne)
		e.openIf() // not an integer
		e.loadTyp(in.A)
		w.I32(int32(L.TypNull))
		w.op(opI32Ne)
		e.exitIf(pc)
		if in.C != 0 {
			e.jump(in.T)
		}
		w.op(opElse)
		e.loadI64(in.A, L.OffI)
		w.op(opI64Eqz)
		if in.Imm != 0 {
			w.op(opI32Eqz) // jump when non-zero
		}
		e.jumpIf(in.T)
		e.closeIf()
	case VGosub:
		e.guards(pc, hasS(in.A))
		w.I64(int64(pc))
		w.Set(e.t1)
		e.writeInt(in.A, e.t1)
		e.jump(in.T)
	case VReturn:
		e.guards(pc, notInt(in.A))
		e.loadI64(in.A, L.OffI)
		w.Set(e.t1)
		w.Get(e.t1)
		w.I64(int64(e.n - 1))
		w.op(opI64GtU) // r < 0 or r >= n: the VDBE's error
		e.exitIf(pc)
		e.reenter(func() {
			w.Get(e.t1)
			w.op(opI32WrapI64)
			w.I32(1)
			w.op(opI32Add)
		}, -1)
	default:
		return fmt.Errorf("unknown VM op %d", in.Op)
	}
	return nil
}
