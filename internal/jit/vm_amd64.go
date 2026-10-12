//go:build amd64

package jit

import "fmt"

// condAbove is x86's unsigned "above" (JA), for range checks that fold negatives.
const (
	condAbove   Cond = 0x7
	condAboveEq Cond = 0x3 // JAE: unsigned >=
	condBelowEq Cond = 0x6 // JBE: unsigned <=
)

// EmitVM compiles a whole VDBE program; see vm.go for the contract.
//
// Registers: RDI *ProgArgs, RSI the register file, R8 fuel, RCX the pc at an
// entry or an OP_Return, RAX/RDX scratch. Nothing else is touched, so the
// trampoline's saves are more than enough.
//
// Layout: a prologue that loads the entry pc and falls into the dispatcher;
// one block per pc, in pc order, so a fall-through is simply the next block;
// the exit stubs, out of line; and the dispatch table, one int32 per pc (and
// one for the pc past the end) giving its block's offset from the table.
func EmitVM(prog []VInsn, nReg int, lay ValueLayout) ([]byte, error) {
	if lay.OffTyp != 0 || lay.OffI != 8 || lay.OffF != 16 {
		// writeInt stores the type as the whole first word -- type, subtype and
		// padding together -- so the type must lead it.
		return nil, fmt.Errorf("jit: unexpected Value layout %+v", lay)
	}
	n := len(prog)
	if int64(nReg)*int64(lay.Size) > 1<<30 || n > 1<<28 {
		return nil, fmt.Errorf("jit: program too large (%d instructions, %d registers)", n, nReg)
	}
	e := &vmEmit{a: NewAsm(), lay: lay, n: n, stubs: map[int]bool{}}
	a := e.a

	a.MovRegMem(RSI, RDI, POffRegs)
	a.MovRegMemD(RCX, RDI, POffPC)
	a.MovRegImm64(R8, VMFuel)
	a.Label("dispatch")
	a.LeaRIP(RAX, "table")
	a.MovsxdRegMemIdx4(RDX, RAX, RCX)
	a.AddRegReg(RAX, RDX)
	a.JmpReg(RAX)
	a.Label("exitrcx")
	a.MovMemDReg(RDI, POffPC, RCX)
	a.Ret()

	e.blockOff = make([]int, n)
	for pc, in := range prog {
		e.blockOff[pc] = len(a.buf)
		if err := e.insn(pc, in); err != nil {
			return nil, fmt.Errorf("jit: pc %d: %w", pc, err)
		}
	}
	// Running off the last block is running off the program.
	e.exitAt(n)

	stubOff := map[int]int{}
	e.stubs[n] = true
	for k := range e.stubs {
		stubOff[k] = len(a.buf)
		e.exitAt(k)
	}
	a.Label("table")
	tab := len(a.buf)
	for pc := 0; pc <= n; pc++ {
		off := stubOff[n]
		if pc < n {
			off = e.blockOff[pc]
		}
		a.emit32(uint32(int32(off - tab)))
	}
	for _, f := range e.fix {
		var target int
		if f.stub {
			target = stubOff[f.pc]
		} else {
			target = e.blockOff[f.pc]
		}
		rel := int32(target - (f.at + 4))
		a.buf[f.at], a.buf[f.at+1], a.buf[f.at+2], a.buf[f.at+3] = byte(rel), byte(rel>>8), byte(rel>>16), byte(rel>>24)
	}
	return a.Code()
}

type vmEmit struct {
	a        *Asm
	lay      ValueLayout
	n        int
	blockOff []int
	fix      []vmFix
	stubs    map[int]bool
}

// vmFix is a rel32 to patch: to pc's block, or to the stub that exits at pc.
type vmFix struct {
	at   int
	pc   int
	stub bool
}

func (e *vmEmit) d(r, off int) int32 { return int32(r*e.lay.Size + off) }

// exitAt returns to Go asking the VDBE to run pc.
func (e *vmEmit) exitAt(pc int) {
	e.a.MovMemDImm(RDI, POffPC, int32(pc))
	e.a.Ret()
}

// jccStub / jmpStub branch to the out-of-line exit at pc.
func (e *vmEmit) jccStub(c Cond, pc int) {
	e.stubs[pc] = true
	e.a.emit(0x0F, 0x80|byte(c))
	e.fix = append(e.fix, vmFix{at: len(e.a.buf), pc: pc, stub: true})
	e.a.emit(0, 0, 0, 0)
}

func (e *vmEmit) jmpStub(pc int) {
	e.stubs[pc] = true
	e.a.emit(0xE9)
	e.fix = append(e.fix, vmFix{at: len(e.a.buf), pc: pc, stub: true})
	e.a.emit(0, 0, 0, 0)
}

// jmpBlock branches to target's block, or exits when target is past the end.
// A backward branch spends fuel first, exiting at target when it runs out.
func (e *vmEmit) jmpBlock(from, target int) {
	if target < 0 || target >= e.n {
		e.jmpStub(target)
		return
	}
	if target <= from {
		e.a.DecReg(R8)
		e.jccStub(CondE, target)
	}
	e.a.emit(0xE9)
	e.fix = append(e.fix, vmFix{at: len(e.a.buf), pc: target})
	e.a.emit(0, 0, 0, 0)
}

// jccBlock branches to target when c holds.
func (e *vmEmit) jccBlock(c Cond, from, target int) {
	if target >= 0 && target < e.n && target > from {
		e.a.emit(0x0F, 0x80|byte(c))
		e.fix = append(e.fix, vmFix{at: len(e.a.buf), pc: target})
		e.a.emit(0, 0, 0, 0)
		return
	}
	skip := len(e.a.buf)
	e.a.emit(0x0F, 0x80|byte(c.Negate()), 0, 0, 0, 0)
	e.jmpBlock(from, target)
	rel := int32(len(e.a.buf) - (skip + 6))
	e.a.buf[skip+2], e.a.buf[skip+3], e.a.buf[skip+4], e.a.buf[skip+5] = byte(rel), byte(rel>>8), byte(rel>>16), byte(rel>>24)
}

// guardInt exits at pc unless r[reg] is an INTEGER.
func (e *vmEmit) guardInt(pc, reg int) {
	e.a.CmpMemDByte(RSI, e.d(reg, e.lay.OffTyp), e.lay.TypInt)
	e.jccStub(CondNE, pc)
}

// guardNoS exits at pc unless r[reg] holds no string or blob, which is what
// lets native code overwrite it without a write barrier.
func (e *vmEmit) guardNoS(pc, reg int) {
	e.a.CmpMemDImm8(RSI, e.d(reg, e.lay.OffS), 0)
	e.jccStub(CondNE, pc)
}

// writeInt makes r[reg] the INTEGER in src: Value{Typ: Int, I: src}.
func (e *vmEmit) writeInt(reg int, src Reg) {
	e.a.MovMemDImm(RSI, e.d(reg, 0), int32(e.lay.TypInt))
	e.a.MovMemDReg(RSI, e.d(reg, e.lay.OffI), src)
	e.a.MovMemDImm(RSI, e.d(reg, e.lay.OffF), 0)
}

func (e *vmEmit) insn(pc int, in VInsn) error {
	a, L := e.a, e.lay
	switch in.Op {
	case VExit:
		e.exitAt(pc)
	case VGoto:
		e.jmpBlock(pc, in.T)
	case VInt:
		e.guardNoS(pc, in.A)
		a.MovRegImm64(RAX, in.Imm)
		e.writeInt(in.A, RAX)
	case VCopy:
		e.guardNoS(pc, in.A)
		e.guardNoS(pc, in.B)
		for _, off := range []int{0, L.OffI, L.OffF} {
			a.MovRegMemD(RAX, RSI, e.d(in.A, off))
			a.MovMemDReg(RSI, e.d(in.B, off), RAX)
		}
	case VAdd, VSub, VMul, VAnd, VOr:
		e.guardInt(pc, in.B)
		e.guardInt(pc, in.A)
		e.guardNoS(pc, in.C)
		a.MovRegMemD(RAX, RSI, e.d(in.B, L.OffI))
		op := AluOr
		switch in.Op {
		case VAdd:
			op = AluAdd
		case VSub:
			op = AluSub
		case VMul:
			op = AluImul
		case VAnd:
			op = AluAnd
		}
		a.AluRegMemD(op, RAX, RSI, e.d(in.A, L.OffI))
		if in.Op == VAdd || in.Op == VSub || in.Op == VMul {
			e.jccStub(condOverflow, pc)
		}
		e.writeInt(in.C, RAX)
	case VShl, VShr:
		e.guardInt(pc, in.B)
		e.guardInt(pc, in.A)
		e.guardNoS(pc, in.C)
		a.MovRegMemD(RCX, RSI, e.d(in.A, L.OffI))
		a.CmpRegImm32(RCX, 63)
		e.jccStub(condAbove, pc) // unsigned: a negative amount is huge
		a.MovRegMemD(RAX, RSI, e.d(in.B, L.OffI))
		if in.Op == VShl {
			a.ShlRegCL(RAX)
		} else {
			a.SarRegCL(RAX)
		}
		e.writeInt(in.C, RAX)
	case VCmpJump:
		e.guardInt(pc, in.B)
		e.guardInt(pc, in.A)
		a.MovRegMemD(RAX, RSI, e.d(in.B, L.OffI))
		a.AluRegMemD(AluCmp, RAX, RSI, e.d(in.A, L.OffI))
		e.jccBlock(in.Cond, pc, in.T)
	case VCmpSet:
		e.guardInt(pc, in.B)
		e.guardInt(pc, in.A)
		e.guardNoS(pc, in.C)
		a.XorRegReg(RDX, RDX) // before the compare: it clears the flags
		a.MovRegMemD(RAX, RSI, e.d(in.B, L.OffI))
		a.AluRegMemD(AluCmp, RAX, RSI, e.d(in.A, L.OffI))
		a.Setcc(in.Cond, RDX)
		e.writeInt(in.C, RDX)
	case VNullJump:
		a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypNull)
		if in.Imm != 0 {
			e.jccBlock(CondE, pc, in.T)
		} else {
			e.jccBlock(CondNE, pc, in.T)
		}
	case VNull:
		e.guardNoS(pc, in.C)
		a.MovMemDImm(RSI, e.d(in.C, 0), int32(L.TypNull))
		a.MovMemDImm(RSI, e.d(in.C, L.OffI), 0)
		a.MovMemDImm(RSI, e.d(in.C, L.OffF), 0)
	case VNeg:
		e.guardInt(pc, in.A)
		e.guardNoS(pc, in.C)
		a.MovRegMemD(RAX, RSI, e.d(in.A, L.OffI))
		a.NegReg(RAX)
		e.jccStub(condOverflow, pc) // -MinInt64 is the VDBE's
		e.writeInt(in.C, RAX)
	case VIf:
		// An INTEGER jumps on its truth; a NULL jumps when C says so; anything
		// else is the VDBE's.
		a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypInt)
		notInt := len(a.buf)
		a.emit(0x0F, 0x85, 0, 0, 0, 0) // jne notInt
		a.CmpMemDImm8(RSI, e.d(in.A, L.OffI), 0)
		if in.Imm != 0 {
			e.jccBlock(CondNE, pc, in.T)
		} else {
			e.jccBlock(CondE, pc, in.T)
		}
		done := len(a.buf)
		a.emit(0xE9, 0, 0, 0, 0) // jmp done
		patch(a.buf, notInt+2, len(a.buf))
		a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypNull)
		e.jccStub(CondNE, pc)
		if in.C != 0 {
			e.jmpBlock(pc, in.T)
		}
		patch(a.buf, done+1, len(a.buf))
	case VGosub:
		e.guardNoS(pc, in.A)
		a.MovRegImm64(RAX, int64(pc))
		e.writeInt(in.A, RAX)
		e.jmpBlock(pc, in.T)
	case VReturn:
		e.guardInt(pc, in.A)
		a.MovRegMemD(RCX, RSI, e.d(in.A, L.OffI))
		a.CmpRegImm32(RCX, int32(e.n-1))
		e.jccStub(condAbove, pc) // r < 0 or r >= n: the VDBE's error
		a.IncReg(RCX)
		a.DecReg(R8)
		a.Jcc(CondE, "exitrcx")
		a.Jmp("dispatch")
	case VColumn:
		// The cursor's current row's column B, copied word for word when the
		// value holds no string or blob (see VCursor for what native code may
		// assume of a cursor). R9/R10/RDX are scratch.
		cb := int32(in.A * VCursorSize)
		vb := int32(in.B) * int32(L.Size)
		a.MovRegMemD(RAX, RDI, POffCur)
		a.CmpMemDImm8(RAX, cb+VCursorOffOk, 0)
		e.jccStub(CondE, pc)
		a.MovRegMemD(RDX, RAX, cb+VCursorOffMask) // the column must be one the engine made readable
		a.MovRegImm64(R10, int64(1)<<uint(in.B))
		a.TestRegReg(RDX, R10)
		e.jccStub(CondE, pc)
		a.MovRegMemD(RDX, RAX, cb+VCursorOffPos)
		a.AluRegMemD(AluCmp, RDX, RAX, cb+VCursorOffN)
		e.jccStub(condAboveEq, pc) // unsigned: a position before the first row is huge
		a.MovRegImm64(R9, sliceHeaderSize)
		a.ImulRegReg(RDX, R9)
		a.MovRegMemD(R9, RAX, cb+VCursorOffRows)
		a.AddRegReg(R9, RDX)
		a.MovRegMemD(R10, R9, 8) // the row's length
		a.CmpRegImm32(R10, int32(in.B))
		e.jccStub(condBelowEq, pc) // a row narrower than the column: the VDBE's
		a.MovRegMemD(R9, R9, 0)    // the row's first Value
		a.CmpMemDImm8(R9, vb+int32(L.OffS), 0)
		e.jccStub(CondNE, pc)
		e.guardNoS(pc, in.C)
		for _, off := range []int{0, L.OffI, L.OffF} {
			a.MovRegMemD(RDX, R9, vb+int32(off))
			a.MovMemDReg(RSI, e.d(in.C, off), RDX)
		}
	case VRewind:
		cb := int32(in.A * VCursorSize)
		a.MovRegMemD(RAX, RDI, POffCur)
		a.CmpMemDImm8(RAX, cb+VCursorOffOk, 0)
		e.jccStub(CondE, pc)
		a.CmpMemDImm8(RAX, cb+VCursorOffRew, 0)
		e.jccStub(CondE, pc)
		a.MovMemDImm(RAX, cb+VCursorOffPos, 0)
		a.MovMemDImm(RAX, cb+VCursorOffDirty, 1)
		a.CmpMemDImm8(RAX, cb+VCursorOffN, 0)
		nonEmpty := len(a.buf)
		a.emit(0x0F, 0x80|byte(CondNE), 0, 0, 0, 0)
		e.jmpBlock(pc, in.T) // no rows
		patch(a.buf, nonEmpty+2, len(a.buf))
		a.MovMemDImm(RAX, cb+VCursorOffLast, 0)
	case VNext:
		cb := int32(in.A * VCursorSize)
		a.MovRegMemD(RAX, RDI, POffCur)
		a.CmpMemDImm8(RAX, cb+VCursorOffOk, 0)
		e.jccStub(CondE, pc)
		a.CmpMemDImm8(RAX, cb+VCursorOffAdv, 0)
		e.jccStub(CondE, pc)
		a.MovRegMemD(RDX, RAX, cb+VCursorOffPos)
		a.IncReg(RDX)
		a.MovMemDReg(RAX, cb+VCursorOffPos, RDX)
		a.MovMemDImm(RAX, cb+VCursorOffDirty, 1)
		a.AluRegMemD(AluCmp, RDX, RAX, cb+VCursorOffN)
		done := len(a.buf)
		a.emit(0x0F, 0x80|byte(condAboveEq), 0, 0, 0, 0) // off the end: fall through
		a.MovMemDReg(RAX, cb+VCursorOffLast, RDX)
		e.jmpBlock(pc, in.T)
		patch(a.buf, done+2, len(a.buf))
	default:
		return fmt.Errorf("unknown VM op %d", in.Op)
	}
	return nil
}

// patch writes the rel32 at buf[at:] so it lands on target.
func patch(buf []byte, at, target int) {
	rel := int32(target - (at + 4))
	buf[at], buf[at+1], buf[at+2], buf[at+3] = byte(rel), byte(rel>>8), byte(rel>>16), byte(rel>>24)
}
