//go:build arm64

package jit

import (
	"fmt"
	"slices"
	"strings"
)

// vmPool is the machine registers a fused program keeps values in between
// steps (vmfuse.go). Under the ABI0 trampoline every one of them is the
// kernel's (trampoline_arm64.s); X9-X13 are the emitter's scratch, X0-X3 its
// fixed registers, and X18, X26-X28 are never touched.
var vmPool = []Reg64{X14, X15, X16, X17, X19, X20, X21, X22, X23, X24, X25}

// emitVM compiles a whole VDBE program. Layout: a prologue, one block per pc
// in order with exit stubs in islands between them, and a dispatch table.
//
// AArch64's conditional branches reach only +-1 MB, and a large program (Doom's
// is ~12 MB) puts its last blocks far beyond that from anything at either end.
// So every exit stub lives in an island within islandBytes of the branches
// that use it, the table is found through a literal beside the prologue, and a
// forward conditional jump falls back to an inverted branch over a B (+-128 MB)
// when the short form does not reach.
//
// bound and cache are as on amd64 (vm_amd64.go's emitVM).
func emitVM(prog []VInsn, nReg int, lay ValueLayout, bound, entry []bool, cache bool) ([]byte, error) {
	b, err := emitVMArm(prog, nReg, lay, bound, entry, cache, false)
	if err != nil && strings.Contains(err.Error(), "out of range") {
		b, err = emitVMArm(prog, nReg, lay, bound, entry, cache, true)
	}
	return b, err
}

// islandBytes bounds the code between a stub and the branches to it, well
// inside the +-1 MB a conditional branch reaches.
const islandBytes = 256 << 10

func emitVMArm(prog []VInsn, nReg int, lay ValueLayout, bound, entry []bool, cache, far bool) ([]byte, error) {
	if lay.OffTyp != 0 || lay.OffI != 8 || lay.OffF != 16 {
		// writeInt stores the type as the whole first word -- type, subtype and
		// padding together -- so the type must lead it.
		return nil, fmt.Errorf("jit: unexpected Value layout %+v", lay)
	}
	n := len(prog)
	if int64(nReg)*int64(lay.Size) > 1<<30 || n > 1<<28 {
		return nil, fmt.Errorf("jit: program too large (%d instructions, %d registers)", n, nReg)
	}
	e := &vmEmitArm{a: NewArm(), lay: lay, n: n, stubs: map[int]bool{}, far: far,
		prog: prog, bound: bound, cache: cache && bound != nil, know: newVMKnow(),
		holds: map[int]Reg64{}, pinned: map[Reg64]bool{}, fusedStub: map[int]string{}}
	e.heads = vmLoopHeads(prog)
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
		if vmRunStart(prog, bound, pc) {
			e.forget()
			if e.cache && e.heads[pc] {
				// A loop's head starts a 32-byte fetch block, so the loop's
				// code does not shift with whatever precedes it.
				for len(a.buf)%8 != 0 {
					a.emit(armNop)
				}
			}
		} else if e.cache && pc < len(entry) && entry[pc] {
			e.entryPoint(pc)
		}
		a.Label(vmBlock(pc))
		clear(e.pinned)
		if in.Op == VFused {
			// Never entered; should anything dispatch here, the VDBE runs it.
			e.fusedStub[pc] = e.stub(pc)
		}
		if err := e.insn(pc, in); err != nil {
			return nil, fmt.Errorf("jit: pc %d: %w", pc, err)
		}
		if !e.cache {
			e.forget()
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
	for _, en := range e.entries {
		a.Label(fmt.Sprintf("n%d", en.pc))
		for _, r := range en.ints {
			e.guardInt(en.pc, r)
		}
		for _, r := range en.noS {
			e.guardNoS(en.pc, r)
		}
		for _, h := range en.holds {
			e.ldr(h.m, h.r, lay.OffI)
		}
		a.B(vmBlock(en.pc))
		if (len(a.buf)-islandAt)*4 >= islandBytes {
			e.island("")
			islandAt = len(a.buf)
		}
	}
	e.island("")

	a.Label("table")
	tab := len(a.buf)
	off := uint64(tab-tabrel) * 4
	a.buf[tabrel], a.buf[tabrel+1] = uint32(off), uint32(off>>32)
	for pc := 0; pc <= n; pc++ {
		target := a.labels["end"]
		if pc < n {
			target = a.labels[vmBlock(pc)]
			if l, ok := e.fusedStub[pc]; ok {
				target = a.labels[l]
			} else if l, ok := a.labels[fmt.Sprintf("n%d", pc)]; ok {
				target = l
			}
		}
		a.emit(uint32(int32((target - tab) * 4)))
	}
	return a.Code()
}

// armNop is AArch64's NOP.
const armNop uint32 = 0xD503201F

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

	prog      []VInsn
	bound     []bool
	cache     bool
	know      *vmKnow
	holds     map[int]Reg64 // see vmEmit.holds
	pinned    map[Reg64]bool
	heads     []bool
	fusedStub map[int]string // a VFused pc's exit stub, which the table points at
	entries   []vmEntryStubArm
}

// vmEntryStubArm is vm_amd64.go's vmEntryStub.
type vmEntryStubArm struct {
	pc        int
	ints, noS []int
	holds     []vmHoldArm
}

type vmHoldArm struct {
	r int
	m Reg64
}

// entryPoint is vm_amd64.go's.
func (e *vmEmitArm) entryPoint(pc int) {
	keep := vmEntryKeep(e.prog, e.bound, pc)
	st := vmEntryStubArm{pc: pc}
	st.ints, st.noS = e.know.pruneForEntry(keep)
	for k, m := range e.holds {
		if k < 0 || !keep[k] || !e.know.isInt[k] {
			delete(e.holds, k)
			continue
		}
		st.holds = append(st.holds, vmHoldArm{k, m})
	}
	slices.SortFunc(st.holds, func(x, y vmHoldArm) int { return x.r - y.r })
	e.entries = append(e.entries, st)
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

// ldrAt/strAt are ldr/str over a plain base register: a displacement an
// immediate cannot reach goes through X13.
func (e *vmEmitArm) ldrAt(rt, base Reg64, off int) {
	if off%8 == 0 && off/8 < 4096 {
		e.a.LdrImm(rt, base, off)
		return
	}
	e.a.MovImm64(X13, int64(off))
	e.a.LdrReg(rt, base, X13)
}

func (e *vmEmitArm) strAt(rt, base Reg64, off int) {
	if off%8 == 0 && off/8 < 4096 {
		e.a.StrImm(rt, base, off)
		return
	}
	e.a.MovImm64(X13, int64(off))
	e.a.StrReg(rt, base, X13)
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

// guardInt exits at pc unless r[reg] is an INTEGER. Guards read into X12, so
// an operand already loaded into X9 or X10 survives one.
func (e *vmEmitArm) guardInt(pc, reg int) {
	e.ldrb(X12, reg, e.lay.OffTyp)
	e.a.CmpImm(X12, uint32(e.lay.TypInt))
	e.a.Bcond(CondNE, e.stub(pc))
}

// guardNoS exits at pc unless r[reg] holds no string or blob, which is what
// lets native code overwrite it without a write barrier.
func (e *vmEmitArm) guardNoS(pc, reg int) {
	e.ldr(X12, reg, e.lay.OffS)
	e.a.Cbnz(X12, e.stub(pc))
}

// writeInt makes r[reg] the INTEGER in src: Value{Typ: Int, I: src}.
func (e *vmEmitArm) writeInt(reg int, src Reg64) {
	e.a.MovImm16(X12, uint16(e.lay.TypInt))
	e.str(X12, reg, 0)
	e.str(src, reg, e.lay.OffI)
	e.str(xzr, reg, e.lay.OffF)
}

// ---- what the emitter keeps between steps: vm_amd64.go's, on arm64 ---------

func (e *vmEmitArm) forget() {
	e.know.reset()
	clear(e.holds)
}

func (e *vmEmitArm) unmap(key int) { delete(e.holds, key) }

func (e *vmEmitArm) clobber(r int) {
	e.unmap(r)
	delete(e.know.isInt, r)
	delete(e.know.canon, r)
	delete(e.know.noS, r)
}

func (e *vmEmitArm) nextUse(key, pc int) int { return vmEmitNextUse(e.prog, e.bound, key, pc) }

func (e *vmEmitArm) alloc(pc, want int) (Reg64, bool) {
	if !e.cache {
		return 0, false
	}
	far := map[Reg64]int{}
	for k, m := range e.holds {
		u := e.nextUse(k, pc)
		if u < 0 {
			u = 1 << 30
		}
		far[m] = max(far[m], u)
	}
	var best Reg64
	bestAt := -1
	for _, m := range vmPool {
		if e.pinned[m] {
			continue
		}
		at, used := far[m]
		if !used {
			best, bestAt = m, 1<<31
			break
		}
		if at > bestAt {
			best, bestAt = m, at
		}
	}
	if bestAt < 0 || bestAt <= want {
		return 0, false
	}
	for k, m := range e.holds {
		if m == best {
			delete(e.holds, k)
		}
	}
	e.pinned[best] = true
	return best, true
}

func (e *vmEmitArm) src(pc, r int, scratch Reg64) Reg64 {
	if !e.know.isInt[r] {
		e.guardInt(pc, r)
		e.know.isInt[r] = true
	}
	if m, ok := e.holds[r]; ok {
		e.pinned[m] = true
		return m
	}
	if u := e.nextUse(r, pc); u >= 0 {
		if m, ok := e.alloc(pc, u); ok {
			e.ldr(m, r, e.lay.OffI)
			e.holds[r] = m
			return m
		}
	}
	e.ldr(scratch, r, e.lay.OffI)
	return scratch
}

func (e *vmEmitArm) needNoS(pc, r int) {
	if !e.know.noS[r] {
		e.guardNoS(pc, r)
		e.know.noS[r] = true
	}
}

func (e *vmEmitArm) dest(pc, r int, scratch Reg64) Reg64 {
	if u := e.nextUse(r, pc); u >= 0 {
		if m, ok := e.alloc(pc, u); ok {
			return m
		}
	}
	return scratch
}

func (e *vmEmitArm) inPlace(pc, r int) (Reg64, bool) {
	m, ok := e.holds[r]
	if !ok {
		return 0, false
	}
	for k, mk := range e.holds {
		if mk == m && e.nextUse(k, pc) >= 0 {
			return 0, false
		}
	}
	return m, true
}

func (e *vmEmitArm) storeInt(in VInsn, r int, m Reg64) {
	for k, mk := range e.holds {
		if mk == m {
			delete(e.holds, k)
		}
	}
	if in.Dead {
		e.clobber(r)
		return
	}
	if e.know.canon[r] {
		e.str(m, r, e.lay.OffI)
	} else {
		e.writeInt(r, m)
	}
	e.unmap(r)
	e.know.wroteInt(r)
	if e.cache && isPoolArm(m) {
		e.holds[r] = m
	}
}

func isPoolArm(m Reg64) bool {
	for _, p := range vmPool {
		if p == m {
			return true
		}
	}
	return false
}

func (e *vmEmitArm) insn(pc int, in VInsn) error {
	a, L := e.a, e.lay
	switch in.Op {
	case VExit:
		e.exitAt(pc)
	case VFused:
	case VGoto:
		e.jmpBlock(pc, in.T)
	case VInt:
		if in.Dead {
			e.clobber(in.A)
			break
		}
		e.needNoS(pc, in.A)
		m := e.dest(pc, in.A, X10)
		a.MovImm64(m, in.Imm)
		e.storeInt(in, in.A, m)
	case VCopy:
		if in.Dead {
			e.clobber(in.B)
			break
		}
		e.needNoS(pc, in.A)
		e.needNoS(pc, in.B)
		if e.know.canon[in.A] && in.A != in.B {
			m := e.src(pc, in.A, X10)
			if e.know.canon[in.B] {
				e.str(m, in.B, L.OffI)
			} else {
				e.writeInt(in.B, m)
			}
			e.unmap(in.B)
			e.know.wroteInt(in.B)
			if isPoolArm(m) {
				e.holds[in.B] = m
			}
			break
		}
		for _, f := range []int{0, L.OffI, L.OffF} {
			e.ldr(X9, in.A, f)
			e.str(X9, in.B, f)
		}
		if in.A != in.B {
			isInt := e.know.isInt[in.A]
			e.unmap(in.B)
			e.know.wroteAny(in.B)
			if isInt {
				e.know.isInt[in.B] = true
			}
			if m, ok := e.holds[in.A]; ok {
				e.holds[in.B] = m
			}
		}
	case VAdd, VSub, VMul, VAnd, VOr:
		y := e.src(pc, in.A, X10)
		b := e.src(pc, in.B, X9)
		if !in.Dead {
			e.needNoS(pc, in.C)
		}
		x := X9
		if m, ok := e.inPlace(pc, in.B); ok && m == b && in.A != in.B {
			x = m
		} else {
			x = e.dest(pc, in.C, X9)
		}
		switch in.Op {
		case VAdd:
			a.AddsReg(x, b, y)
			a.BcondRaw(armVS, e.stub(pc))
		case VSub:
			a.SubsReg(x, b, y)
			a.BcondRaw(armVS, e.stub(pc))
		case VMul:
			// No flag-setting multiply: the product fits exactly when its high
			// half is the low half's sign extension (see Smulh).
			a.Smulh(X12, b, y)
			a.Mul(x, b, y)
			a.CmpAsr63(X12, x)
			a.Bcond(CondNE, e.stub(pc))
		case VAnd:
			a.AndReg(x, b, y)
		default:
			a.OrrReg(x, b, y)
		}
		e.storeInt(in, in.C, x)
	case VShl, VShr:
		y := e.src(pc, in.A, X10)
		a.CmpImm(y, 63)
		a.BcondRaw(armHI, e.stub(pc)) // unsigned: a negative amount is huge
		b := e.src(pc, in.B, X9)
		if !in.Dead {
			e.needNoS(pc, in.C)
		}
		x := e.dest(pc, in.C, X9)
		if in.Op == VShl {
			a.Lslv(x, b, y)
		} else {
			a.Asrv(x, b, y)
		}
		e.storeInt(in, in.C, x)
	case VDiv, VRem:
		y := e.src(pc, in.A, X10)
		b := e.src(pc, in.B, X9)
		if !in.Dead {
			e.needNoS(pc, in.C)
		}
		// A divisor of 0 (NULL) or -1 (the one overflow) is the VDBE's.
		a.Cbz(y, e.stub(pc))
		a.MovImm64(X12, -1)
		a.Cmp(y, X12)
		a.Bcond(CondE, e.stub(pc))
		x := e.dest(pc, in.C, X9)
		a.Sdiv(X12, b, y)
		if in.Op == VRem {
			a.Msub(x, X12, y, b) // b - (b/y)*y
		} else {
			a.MovReg(x, X12)
		}
		e.storeInt(in, in.C, x)
	case VCmpJump:
		x := e.src(pc, in.B, X9)
		y := e.src(pc, in.A, X10)
		a.Cmp(x, y)
		e.jccBlock(in.Cond, pc, in.T)
	case VCmpSet, VCmpBr:
		x := e.src(pc, in.B, X9)
		y := e.src(pc, in.A, X10)
		if !in.Dead {
			e.needNoS(pc, in.C)
		}
		a.Cmp(x, y)
		if !in.Dead {
			m := e.dest(pc, in.C, X9)
			a.Cset(m, in.Cond)
			e.storeInt(in, in.C, m) // stores leave the flags alone
		} else {
			e.clobber(in.C)
		}
		if in.Op == VCmpBr {
			c := in.Cond
			if in.Imm == 0 {
				c = c.Negate()
			}
			e.jccBlock(c, pc, in.T)
		}
	case VNot, VNotBr:
		x := e.src(pc, in.A, X9)
		if !in.Dead {
			e.needNoS(pc, in.C)
		}
		a.CmpImm(x, 0)
		if !in.Dead {
			m := e.dest(pc, in.C, X9)
			a.Cset(m, CondE)
			e.storeInt(in, in.C, m)
		} else {
			e.clobber(in.C)
		}
		if in.Op == VNotBr {
			c := CondE
			if in.Imm == 0 {
				c = CondNE
			}
			e.jccBlock(c, pc, in.T)
		}
	case VNullJump:
		if e.know.isInt[in.A] {
			if in.Imm == 0 {
				e.jmpBlock(pc, in.T)
			}
			break
		}
		e.ldrb(X9, in.A, L.OffTyp)
		a.CmpImm(X9, uint32(L.TypNull))
		if in.Imm != 0 {
			e.jccBlock(CondE, pc, in.T)
		} else {
			e.jccBlock(CondNE, pc, in.T)
		}
	case VNull:
		if in.Dead {
			e.clobber(in.C)
			break
		}
		e.needNoS(pc, in.C)
		e.str(xzr, in.C, 0)
		e.str(xzr, in.C, L.OffI)
		e.str(xzr, in.C, L.OffF)
		e.unmap(in.C)
		e.know.wroteAny(in.C)
	case VNeg:
		y := e.src(pc, in.A, X10)
		if !in.Dead {
			e.needNoS(pc, in.C)
		}
		x := X9
		if m, ok := e.inPlace(pc, in.A); ok && m == y {
			x = m
		} else {
			x = e.dest(pc, in.C, X9)
		}
		a.SubsReg(x, xzr, y)
		a.BcondRaw(armVS, e.stub(pc)) // -MinInt64 is the VDBE's
		e.storeInt(in, in.C, x)
	case VIf:
		if e.know.isInt[in.A] {
			x := e.src(pc, in.A, X10)
			a.CmpImm(x, 0)
			if in.Imm != 0 {
				e.jccBlock(CondNE, pc, in.T)
			} else {
				e.jccBlock(CondE, pc, in.T)
			}
			break
		}
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
		e.needNoS(pc, in.A)
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
	case VColumn:
		e.column(pc, in)
	case VRewind:
		e.unmap(^in.A)
		cb := in.A * VCursorSize
		e.skips++
		nonEmpty := fmt.Sprintf("s%d.rows", e.skips)
		a.LdrImm(X9, X0, POffCur)
		e.ldrAt(X10, X9, cb+VCursorOffOk)
		a.Cbz(X10, e.stub(pc))
		e.ldrAt(X10, X9, cb+VCursorOffRew)
		a.Cbz(X10, e.stub(pc))
		e.strAt(xzr, X9, cb+VCursorOffPos)
		a.MovImm16(X11, 1)
		e.strAt(X11, X9, cb+VCursorOffDirty)
		e.ldrAt(X10, X9, cb+VCursorOffN)
		a.Cbnz(X10, nonEmpty)
		e.jmpBlock(pc, in.T) // no rows
		a.Label(nonEmpty)
		e.strAt(xzr, X9, cb+VCursorOffLast)
	case VNext:
		e.unmap(^in.A)
		cb := in.A * VCursorSize
		e.skips++
		done := fmt.Sprintf("s%d.done", e.skips)
		a.LdrImm(X9, X0, POffCur)
		e.ldrAt(X10, X9, cb+VCursorOffOk)
		a.Cbz(X10, e.stub(pc))
		e.ldrAt(X10, X9, cb+VCursorOffAdv)
		a.Cbz(X10, e.stub(pc))
		e.ldrAt(X10, X9, cb+VCursorOffPos)
		a.AddImm(X10, X10, 1)
		e.strAt(X10, X9, cb+VCursorOffPos)
		a.MovImm16(X11, 1)
		e.strAt(X11, X9, cb+VCursorOffDirty)
		e.ldrAt(X11, X9, cb+VCursorOffN)
		a.Cmp(X10, X11)
		a.BcondRaw(armHS, done) // off the end: fall through
		e.strAt(X10, X9, cb+VCursorOffLast)
		e.jmpBlock(pc, in.T)
		a.Label(done)
	default:
		return fmt.Errorf("unknown VM op %d", in.Op)
	}
	return nil
}

// column is VColumn; see vm_amd64.go's column for the run-wide checks.
func (e *vmEmitArm) column(pc int, in VInsn) {
	a, L := e.a, e.lay
	cb := in.A * VCursorSize
	vb := in.B * L.Size
	row, have := e.holds[^in.A]
	if have {
		e.pinned[row] = true
	} else {
		mask, maxCol := uint64(1)<<uint(in.B), in.B
		if e.cache {
			mask, maxCol = vmRunColumns(e.prog, e.bound, pc, mask, maxCol)
		}
		a.LdrImm(X9, X0, POffCur)
		e.ldrAt(X10, X9, cb+VCursorOffOk)
		a.Cbz(X10, e.stub(pc))
		e.ldrAt(X10, X9, cb+VCursorOffMask) // the columns must be ones the engine made readable
		a.MovImm64(X11, int64(mask))
		a.AndReg(X10, X10, X11)
		a.Cmp(X10, X11)
		a.Bcond(CondNE, e.stub(pc))
		e.ldrAt(X10, X9, cb+VCursorOffPos)
		e.ldrAt(X11, X9, cb+VCursorOffN)
		a.Cmp(X10, X11)
		a.BcondRaw(armHS, e.stub(pc)) // unsigned: a position before the first row is huge
		a.MovImm64(X11, sliceHeaderSize)
		a.Mul(X10, X10, X11)
		e.ldrAt(X11, X9, cb+VCursorOffRows)
		a.AddReg(X10, X11, X10)
		a.LdrImm(X11, X10, 8) // the row's length
		a.MovImm64(X12, int64(maxCol))
		a.Cmp(X11, X12)
		a.BcondRaw(armLS, e.stub(pc)) // a row narrower than the column: the VDBE's
		row = X10
		if u := e.nextUse(^in.A, pc); u >= 0 {
			if m, ok := e.alloc(pc, u); ok {
				row = m
				e.holds[^in.A] = m
			}
		}
		a.LdrImm(row, X10, 0) // the row's first Value
	}
	e.ldrAt(X11, row, vb+L.OffS)
	a.Cbnz(X11, e.stub(pc))
	e.needNoS(pc, in.C)
	val := X9
	if u := e.nextUse(in.C, pc); u >= 0 {
		if m, ok := e.alloc(pc, u); ok {
			val = m
		}
	}
	e.ldrAt(X12, row, vb)
	e.str(X12, in.C, 0)
	e.ldrAt(val, row, vb+L.OffI)
	e.str(val, in.C, L.OffI)
	e.ldrAt(X12, row, vb+L.OffF)
	e.str(X12, in.C, L.OffF)
	for k, m := range e.holds {
		if m == val && k != ^in.A {
			delete(e.holds, k)
		}
	}
	e.unmap(in.C)
	e.know.wroteAny(in.C)
	if val != X9 {
		e.holds[in.C] = val
	}
}
