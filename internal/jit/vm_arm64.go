//go:build arm64

package jit

import (
	"fmt"
	"strings"
)

// EmitVM compiles a whole VDBE program. Layout: a prologue, one block per pc
// in order with exit stubs in islands between them, and a dispatch table.
//
// AArch64's conditional branches reach only +-1 MB, and a large program (Doom's
// is ~12 MB) puts its last blocks far beyond that from anything at either end.
// So every exit stub lives in an island within islandBytes of the branches
// that use it, the table is found through a literal beside the prologue, and a
// forward conditional jump falls back to an inverted branch over a B (+-128 MB)
// when the short form does not reach.
func EmitVM(prog []VInsn, nReg int, lay ValueLayout) ([]byte, error) {
	b, err := emitVM(prog, nReg, lay, false)
	if err != nil && strings.Contains(err.Error(), "out of range") {
		b, err = emitVM(prog, nReg, lay, true)
	}
	return b, err
}

// islandBytes bounds the code between a stub and the branches to it, well
// inside the +-1 MB a conditional branch reaches.
const islandBytes = 256 << 10

func emitVM(prog []VInsn, nReg int, lay ValueLayout, far bool) ([]byte, error) {
	if lay.OffTyp != 0 || lay.OffI != 8 || lay.OffF != 16 {
		// writeInt stores the type as the whole first word -- type, subtype and
		// padding together -- so the type must lead it.
		return nil, fmt.Errorf("jit: unexpected Value layout %+v", lay)
	}
	n := len(prog)
	if int64(nReg)*int64(lay.Size) > 1<<30 || n > 1<<28 {
		return nil, fmt.Errorf("jit: program too large (%d instructions, %d registers)", n, nReg)
	}
	e := &vmEmitArm{a: NewArm(), lay: lay, n: n, stubs: map[int]bool{}, far: far}
	a := e.a

	a.LdrImm(X1, X0, POffRegs)
	a.LdrImm(X3, X0, POffPC)
	a.MovImm64(X2, VMFuel)
	a.Label("dispatch")
	a.Adr(X9, "tabrel")
	a.LdrImm(X10, X9, 0) // the table's offset from tabrel
	a.AddReg(X9, X9, X10)
	a.LdrswRegLSL2(X10, X9, X3)
	a.AddReg(X9, X9, X10)
	a.Br(X9)
	a.Label("tabrel")
	tabrel := len(a.buf)
	a.emit(0) // patched below: (table - tabrel) in bytes, 64-bit
	a.emit(0)

	islandAt := len(a.buf)
	for pc, in := range prog {
		a.Label(vmBlock(pc))
		if err := e.insn(pc, in); err != nil {
			return nil, fmt.Errorf("jit: pc %d: %w", pc, err)
		}
		if (len(a.buf)-islandAt)*4 >= islandBytes {
			e.island(vmBlock(pc + 1))
			islandAt = len(a.buf)
		}
	}
	// Running off the last block is running off the program.
	a.Label("end")
	e.exitAt(n)
	e.island("")

	a.Label("table")
	tab := len(a.buf)
	off := uint64(tab-tabrel) * 4
	a.buf[tabrel], a.buf[tabrel+1] = uint32(off), uint32(off>>32)
	for pc := 0; pc <= n; pc++ {
		target := a.labels["end"]
		if pc < n {
			target = a.labels[vmBlock(pc)]
		}
		a.emit(uint32(int32((target - tab) * 4)))
	}
	return a.Code()
}

// island emits the exit stubs requested since the last island, after a branch
// over them to next ("" when nothing falls through).
func (e *vmEmitArm) island(next string) {
	if len(e.stubs) == 0 && !e.exitpc {
		return
	}
	if next != "" {
		e.a.B(next)
	}
	for pc := range e.stubs {
		e.a.Label(e.stubLabel(pc))
		e.exitAt(pc)
	}
	if e.exitpc {
		e.a.Label(e.exitpcLabel())
		e.a.StrImm(X3, X0, POffPC)
		e.a.Ret()
	}
	e.stubs, e.exitpc = map[int]bool{}, false
	e.islandN++
}

func vmBlock(pc int) string { return fmt.Sprintf("b%d", pc) }

type vmEmitArm struct {
	a       *Arm
	lay     ValueLayout
	n       int
	stubs   map[int]bool // exit stubs the next island must hold
	exitpc  bool         // ...and whether it needs an "exit at X3" stub
	islandN int          // which island is next, for unique labels
	skips   int
	far     bool // forward conditional jumps use the long form
}

// off is the byte offset of field f of register r.
func (e *vmEmitArm) off(r, f int) int { return r*e.lay.Size + f }

// ldr/str move a word of register r at field f; ldrb reads its type byte. An
// offset an immediate cannot reach goes through X11.
func (e *vmEmitArm) ldr(rt Reg64, r, f int) {
	if o := e.off(r, f); o%8 == 0 && o/8 < 4096 {
		e.a.LdrImm(rt, X1, o)
	} else {
		e.a.MovImm64(X11, int64(o))
		e.a.LdrReg(rt, X1, X11)
	}
}

func (e *vmEmitArm) str(rt Reg64, r, f int) {
	if o := e.off(r, f); o%8 == 0 && o/8 < 4096 {
		e.a.StrImm(rt, X1, o)
	} else {
		e.a.MovImm64(X11, int64(o))
		e.a.StrReg(rt, X1, X11)
	}
}

func (e *vmEmitArm) ldrb(rt Reg64, r, f int) {
	if o := e.off(r, f); o < 4096 {
		e.a.LdrbImm(rt, X1, o)
	} else {
		e.a.MovImm64(X11, int64(o))
		e.a.LdrbReg(rt, X1, X11)
	}
}

// exitAt returns to Go asking the VDBE to run pc.
func (e *vmEmitArm) exitAt(pc int) {
	e.a.MovImm64(X9, int64(pc))
	e.a.StrImm(X9, X0, POffPC)
	e.a.Ret()
}

// stub is the label of the out-of-line exit at pc, in the next island.
func (e *vmEmitArm) stub(pc int) string {
	e.stubs[pc] = true
	return e.stubLabel(pc)
}

func (e *vmEmitArm) stubLabel(pc int) string { return fmt.Sprintf("x%d.%d", e.islandN, pc) }

// exitpcLabel is the next island's "exit at the pc in X3" stub.
func (e *vmEmitArm) exitpcLabel() string { return fmt.Sprintf("e%d", e.islandN) }

// jmpBlock branches to target's block, or exits when target is past the end.
// A backward branch spends fuel first, exiting at target when it runs out.
func (e *vmEmitArm) jmpBlock(from, target int) {
	if target < 0 || target >= e.n {
		e.a.B(e.stub(target))
		return
	}
	if target <= from {
		e.a.SubsImm(X2, X2, 1)
		e.a.Bcond(CondE, e.stub(target))
	}
	e.a.B(vmBlock(target))
}

// jccBlock branches to target when c holds.
func (e *vmEmitArm) jccBlock(c Cond, from, target int) {
	if !e.far && target >= 0 && target < e.n && target > from {
		e.a.Bcond(c, vmBlock(target))
		return
	}
	e.skips++
	skip := fmt.Sprintf("s%d", e.skips)
	e.a.Bcond(c.Negate(), skip)
	e.jmpBlock(from, target)
	e.a.Label(skip)
}

// guardInt exits at pc unless r[reg] is an INTEGER.
func (e *vmEmitArm) guardInt(pc, reg int) {
	e.ldrb(X9, reg, e.lay.OffTyp)
	e.a.CmpImm(X9, uint32(e.lay.TypInt))
	e.a.Bcond(CondNE, e.stub(pc))
}

// guardNoS exits at pc unless r[reg] holds no string or blob, which is what
// lets native code overwrite it without a write barrier.
func (e *vmEmitArm) guardNoS(pc, reg int) {
	e.ldr(X9, reg, e.lay.OffS)
	e.a.Cbnz(X9, e.stub(pc))
}

// writeInt makes r[reg] the INTEGER in src: Value{Typ: Int, I: src}.
func (e *vmEmitArm) writeInt(reg int, src Reg64) {
	e.a.MovImm16(X12, uint16(e.lay.TypInt))
	e.str(X12, reg, 0)
	e.str(src, reg, e.lay.OffI)
	e.str(xzr, reg, e.lay.OffF)
}

func (e *vmEmitArm) insn(pc int, in VInsn) error {
	a, L := e.a, e.lay
	switch in.Op {
	case VExit:
		e.exitAt(pc)
	case VGoto:
		e.jmpBlock(pc, in.T)
	case VInt:
		e.guardNoS(pc, in.A)
		a.MovImm64(X10, in.Imm)
		e.writeInt(in.A, X10)
	case VCopy:
		e.guardNoS(pc, in.A)
		e.guardNoS(pc, in.B)
		for _, f := range []int{0, L.OffI, L.OffF} {
			e.ldr(X9, in.A, f)
			e.str(X9, in.B, f)
		}
	case VAdd, VSub, VMul, VAnd, VOr:
		e.guardInt(pc, in.B)
		e.guardInt(pc, in.A)
		e.guardNoS(pc, in.C)
		e.ldr(X9, in.B, L.OffI)
		e.ldr(X10, in.A, L.OffI)
		switch in.Op {
		case VAdd:
			a.AddsReg(X9, X9, X10)
			a.BcondRaw(armVS, e.stub(pc))
		case VSub:
			a.SubsReg(X9, X9, X10)
			a.BcondRaw(armVS, e.stub(pc))
		case VMul:
			// No flag-setting multiply: the product fits exactly when its high
			// half is the low half's sign extension (see Smulh).
			a.Smulh(X12, X9, X10)
			a.Mul(X9, X9, X10)
			a.CmpAsr63(X12, X9)
			a.Bcond(CondNE, e.stub(pc))
		case VAnd:
			a.AndReg(X9, X9, X10)
		default:
			a.OrrReg(X9, X9, X10)
		}
		e.writeInt(in.C, X9)
	case VShl, VShr:
		e.guardInt(pc, in.B)
		e.guardInt(pc, in.A)
		e.guardNoS(pc, in.C)
		e.ldr(X10, in.A, L.OffI)
		a.CmpImm(X10, 63)
		a.BcondRaw(armHI, e.stub(pc)) // unsigned: a negative amount is huge
		e.ldr(X9, in.B, L.OffI)
		if in.Op == VShl {
			a.Lslv(X9, X9, X10)
		} else {
			a.Asrv(X9, X9, X10)
		}
		e.writeInt(in.C, X9)
	case VCmpJump:
		e.guardInt(pc, in.B)
		e.guardInt(pc, in.A)
		e.ldr(X9, in.B, L.OffI)
		e.ldr(X10, in.A, L.OffI)
		a.Cmp(X9, X10)
		e.jccBlock(in.Cond, pc, in.T)
	case VIf:
		// An INTEGER jumps on its truth; a NULL jumps when C says so; anything
		// else is the VDBE's.
		e.skips++
		notInt, done := fmt.Sprintf("s%d", e.skips), fmt.Sprintf("s%d.done", e.skips)
		e.ldrb(X9, in.A, L.OffTyp)
		a.CmpImm(X9, uint32(L.TypInt))
		a.Bcond(CondNE, notInt)
		e.ldr(X10, in.A, L.OffI)
		a.CmpImm(X10, 0)
		if in.Imm != 0 {
			e.jccBlock(CondNE, pc, in.T)
		} else {
			e.jccBlock(CondE, pc, in.T)
		}
		a.B(done)
		a.Label(notInt)
		a.CmpImm(X9, uint32(L.TypNull))
		a.Bcond(CondNE, e.stub(pc))
		if in.C != 0 {
			e.jmpBlock(pc, in.T)
		}
		a.Label(done)
	case VGosub:
		e.guardNoS(pc, in.A)
		a.MovImm64(X10, int64(pc))
		e.writeInt(in.A, X10)
		e.jmpBlock(pc, in.T)
	case VReturn:
		e.guardInt(pc, in.A)
		e.ldr(X3, in.A, L.OffI)
		a.MovImm64(X10, int64(e.n-1))
		a.Cmp(X3, X10)
		a.BcondRaw(armHI, e.stub(pc)) // r < 0 or r >= n: the VDBE's error
		a.AddImm(X3, X3, 1)
		a.SubsImm(X2, X2, 1)
		e.exitpc = true
		a.Bcond(CondE, e.exitpcLabel())
		a.B("dispatch")
	default:
		return fmt.Errorf("unknown VM op %d", in.Op)
	}
	return nil
}
