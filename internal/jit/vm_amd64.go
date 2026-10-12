//go:build amd64

package jit

import (
	"fmt"
	"slices"
)

// condAbove is x86's unsigned "above" (JA), for range checks that fold negatives.
const (
	condAbove   Cond = 0x7
	condAboveEq Cond = 0x3 // JAE: unsigned >=
	condBelowEq Cond = 0x6 // JBE: unsigned <=
)

// vmPool is the machine registers a fused program keeps values in between
// steps (vmfuse.go): RBX, R12, R13 and R15 are saved by the trampoline, and
// R11 is free. R12 cannot be a base (its encoding is RSP's, which needs a
// SIB byte memD does not write), so it holds values only, never a row.
var vmPool = []Reg{RBX, R11, R13, R15, R12}

// emitVM compiles a whole VDBE program; see vm.go for the contract.
//
// Registers: RDI *ProgArgs, RSI the register file, R8 fuel, RCX the pc at an
// entry or an OP_Return, RAX/RDX/R9/R10 scratch, vmPool the values a fused
// program keeps in machine registers.
//
// Layout: a prologue that loads the entry pc and falls into the dispatcher;
// one block per pc, in pc order, so a fall-through is simply the next block;
// the exit stubs, out of line; and the dispatch table, one int32 per pc (and
// one for the pc past the end) giving its block's offset from the table.
//
// bound (VMBoundaries) is nil for a program entered at any pc; cache is
// whether the emitter keeps what it knows from one step to the next within a
// straight run (vmKnow) -- the fused emission. Without it every step reads
// and checks its operands from the file, as it always did.
//
// entry marks the pcs the engine enters at; one that bound does not mark is
// entered through an entry stub (vmfuse.go), which the dispatch table points at.
func emitVM(prog []VInsn, nReg int, lay ValueLayout, bound, entry []bool, cache bool) ([]byte, error) {
	if lay.OffTyp != 0 || lay.OffI != 8 || lay.OffF != 16 || lay.Size != lay.OffS+24 {
		// writeInt stores the type as the whole first word -- type, subtype and
		// padding together -- so the type must lead it.
		return nil, fmt.Errorf("jit: unexpected Value layout %+v", lay)
	}
	n := len(prog)
	if int64(nReg)*int64(lay.Size) > 1<<30 || n > 1<<28 {
		return nil, fmt.Errorf("jit: program too large (%d instructions, %d registers)", n, nReg)
	}
	e := &vmEmit{a: NewAsm(), lay: lay, n: n, stubs: map[int]bool{}, prog: prog, bound: bound,
		cache: cache && bound != nil, know: newVMKnow(), holds: map[int]Reg{}, pinned: map[Reg]bool{},
		zeroS: map[int]bool{}}
	e.heads = vmLoopHeads(prog)
	a := e.a

	a.MovRegMem(RSI, RDI, POffRegs)
	a.MovRegMemD(RCX, RDI, POffPC)
	a.MovRegImm64(R8, VMFuel)
	// The write-barrier flag, read once: it cannot change before this call
	// returns (wb.go). Only its low byte is tested.
	a.MovRegImm64(RAX, int64(wbFlagAddr))
	a.MovRegMemD(RAX, RAX, 0)
	a.MovMemDReg(RDI, POffWB, RAX)
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
		if vmRunStart(prog, bound, pc) {
			e.forget()
			e.alignLoopHead(pc)
		} else if e.cache && pc < len(entry) && entry[pc] && in.Op != VFused {
			// (An entry at a fused pair's second pc has a block of its own,
			// below; the run goes through the pair knowing what it knew.)
			e.entryPoint(pc)
		}
		e.blockOff[pc] = len(a.buf)
		clear(e.pinned)
		clear(e.zeroS)
		if err := e.insn(pc, in); err != nil {
			return nil, fmt.Errorf("jit: pc %d: %w", pc, err)
		}
		if !e.cache {
			e.forget()
		}
	}
	// Running off the last block is running off the program.
	e.exitAt(n)

	// An entry at a fused pair's second pc runs its test alone, then goes on
	// at the next pc: into its block where a run starts, through its entry
	// stub where it has one, and otherwise by exiting there. An entry stub of
	// its own would cut down what the run knows at that pc (entryPoint), which
	// the fused path -- the one that runs -- would pay for on every pass.
	// This path runs only after the VDBE ran the comparison itself.
	fusedOff := map[int]int{}
	for pc, in := range prog {
		if !vmFusedEntry(prog, entry, pc) {
			continue
		}
		fusedOff[pc] = len(a.buf)
		e.forget()
		clear(e.pinned)
		clear(e.zeroS)
		cache := e.cache
		e.cache = false
		if err := e.insn(pc, VFusedTest(in)); err != nil {
			return nil, fmt.Errorf("jit: pc %d: %w", pc, err)
		}
		e.cache = cache
		e.forget()
		switch {
		case pc+1 >= n:
			e.jmpStub(n)
		case vmRunStart(prog, bound, pc+1):
			e.jmpBlock(pc, pc+1)
		case pc+1 < len(entry) && entry[pc+1]:
			a.emit(0xE9)
			e.fix = append(e.fix, vmFix{at: len(a.buf), pc: pc + 1, entry: true})
			a.emit(0, 0, 0, 0)
		default:
			e.jmpStub(pc + 1)
		}
	}

	for _, c := range e.cold {
		c()
	}

	entryOff := map[int]int{}
	for _, en := range e.entries {
		entryOff[en.pc] = len(a.buf)
		for _, r := range en.ints {
			e.guardInt(en.pc, r)
		}
		for _, r := range en.noS {
			e.guardNoS(en.pc, r)
		}
		for _, h := range en.holds {
			a.MovRegMemD(h.m, RSI, e.d(h.r, lay.OffI))
		}
		a.emit(0xE9)
		e.fix = append(e.fix, vmFix{at: len(a.buf), pc: en.pc})
		a.emit(0, 0, 0, 0)
	}

	stubOff := map[int]int{}
	e.stubs[n] = true
	for pc, in := range prog {
		if in.Op == VFused {
			e.stubs[pc] = true // never entered; should anything dispatch there, the VDBE runs it
		}
	}
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
			if o, ok := fusedOff[pc]; ok {
				off = o
			} else if prog[pc].Op == VFused {
				off = stubOff[pc]
			} else if o, ok := entryOff[pc]; ok {
				off = o
			}
		}
		a.emit32(uint32(int32(off - tab)))
	}
	for _, f := range e.fix {
		var target int
		if f.stub {
			target = stubOff[f.pc]
		} else if f.entry {
			target = entryOff[f.pc]
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

	prog  []VInsn
	bound []bool
	cache bool
	know  *vmKnow
	// holds maps a VDBE register (>= 0) to the machine register holding its I
	// word, and a cursor c (as ^c) to the one holding its current row's first
	// Value. pinned is what the instruction being emitted is using.
	holds  map[int]Reg
	pinned map[Reg]bool
	heads  []bool // vmLoopHeads
	// entries are the entry stubs to emit after the blocks.
	entries []vmEntryStub
	// zeroS is the destinations of the instruction being emitted whose string
	// or blob pointer its store must clear (destNoS).
	zeroS map[int]bool
	// cold is the code of the paths a block branches out to and back from
	// (coldPath), emitted after the blocks.
	cold []func()
}

// coldPath branches, when the flags say NE, to body emitted out of line, which
// then jumps back here: a path that runs only when a string or blob is in
// play costs the hot one a branch not taken. body sees the machine state of
// this point; it may exit, and must leave every register but RDX as it found
// it.
func (e *vmEmit) coldPath(body func()) {
	n := len(e.cold)
	in, back := fmt.Sprintf("c%d", n), fmt.Sprintf("c%d.back", n)
	e.a.Jcc(CondNE, in)
	e.a.Label(back)
	e.cold = append(e.cold, func() {
		e.a.Label(in)
		body()
		e.a.Jmp(back)
	})
}

// vmEntryStub is an entry into the middle of a run: the registers to check
// and the ones to load into machine registers.
type vmEntryStub struct {
	pc        int
	ints, noS []int
	holds     []vmHold
}

type vmHold struct {
	r int
	m Reg
}

// entryPoint makes pc, inside a run, an entry: what the run knows there is
// cut down to what a stub can check, and the stub is noted.
func (e *vmEmit) entryPoint(pc int) {
	keep := vmEntryKeep(e.prog, e.bound, pc)
	st := vmEntryStub{pc: pc}
	st.ints, st.noS = e.know.pruneForEntry(keep)
	for k, m := range e.holds {
		if k < 0 || !keep[k] || !e.know.isInt[k] {
			delete(e.holds, k)
			continue
		}
		st.holds = append(st.holds, vmHold{k, m})
	}
	slices.SortFunc(st.holds, func(x, y vmHold) int { return x.r - y.r })
	e.entries = append(e.entries, st)
}

// vmFix is a rel32 to patch: to pc's block, to the stub that exits at pc, or
// to pc's entry stub.
type vmFix struct {
	at    int
	pc    int
	stub  bool
	entry bool
}

func (e *vmEmit) d(r, off int) int32 { return int32(r*e.lay.Size + off) }

// alignLoopHead pads to loopAlign before a block a backward branch targets:
// a loop's head then starts a fetch block, and the loop body's decoded lines
// do not shift with whatever code precedes it. The padding is a run of
// multi-byte NOPs, executed only by a fall into the head.
func (e *vmEmit) alignLoopHead(pc int) {
	if !e.cache || !e.heads[pc] {
		return
	}
	padNops(e.a, loopAlign)
}

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
// A backward branch spends fuel first, exiting at target when it runs out:
// the branch back is the fuel test's own (JNZ), so going round a loop takes
// one branch, and the exit is the fall-through nobody takes.
func (e *vmEmit) jmpBlock(from, target int) {
	if target < 0 || target >= e.n {
		e.jmpStub(target)
		return
	}
	if target <= from {
		e.a.DecReg(R8)
		e.a.emit(0x0F, 0x80|byte(CondNE))
		e.fix = append(e.fix, vmFix{at: len(e.a.buf), pc: target})
		e.a.emit(0, 0, 0, 0)
		e.jmpStub(target)
		return
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
	e.clearS(reg)
}

// clearS clears r[reg]'s string or blob -- pointer, length and capacity --
// when destNoS said the store must.
func (e *vmEmit) clearS(reg int) {
	if !e.zeroS[reg] {
		return
	}
	delete(e.zeroS, reg)
	// Only when there is a pointer: three stores on every write cost the
	// integer loops that never see one ~8%. The test must leave the flags
	// alone -- a store may sit between a compare and the branch on it
	// (VCmpBr) -- so it is JRCXZ, which reads RCX and no flags. RCX holds the
	// pc only at an entry or an OP_Return, and a shift's count only before
	// its result is stored.
	a := e.a
	a.MovRegMemD(RCX, RSI, e.d(reg, e.lay.OffS))
	a.emit(0xE3, 0) // jrcxz past the stores
	at := len(a.buf)
	for k := range 3 {
		a.MovMemDImm(RSI, e.d(reg, e.lay.OffS+8*k), 0)
	}
	a.buf[at-1] = byte(len(a.buf) - at)
}

// jccWB exits at pc when the GC's write barrier is on (wb.go).
func (e *vmEmit) jccWB(pc int) {
	e.a.CmpMemDByte(RDI, POffWB, 0)
	e.jccStub(CondNE, pc)
}

// destNoS readies r[r] to be overwritten by the instruction at pc: when it
// may hold a string or blob, native code exits there if the write barrier is
// on, and otherwise the store clears the pointer (zeroS) -- a plain store,
// which is all Go's own code makes with the barrier off.
func (e *vmEmit) destNoS(pc, r int) {
	if e.know.noS[r] || e.zeroS[r] {
		return
	}
	e.a.CmpMemDImm8(RSI, e.d(r, e.lay.OffS), 0)
	e.coldPath(func() { e.jccWB(pc) })
	e.zeroS[r] = true
}

// ---- what the emitter keeps between steps (fused emission only) -------------

// forget drops everything known: a run starts.
func (e *vmEmit) forget() {
	e.know.reset()
	clear(e.holds)
}

// unmap forgets which machine register holds key.
func (e *vmEmit) unmap(key int) { delete(e.holds, key) }

// clobber records that native code changed r[r] other than by storeInt.
func (e *vmEmit) clobber(r int) {
	e.unmap(r)
	delete(e.know.isInt, r)
	delete(e.know.canon, r)
	delete(e.know.noS, r)
}

func (e *vmEmit) nextUse(key, pc int) int { return vmEmitNextUse(e.prog, e.bound, key, pc) }

// alloc returns a pool register for a value wanted again at pc want, taking
// one from whatever the pool holds that is wanted furthest away (or never)
// when none is free; ok is false when every candidate is wanted sooner, or in
// use by this instruction. noBase excludes the register that cannot be a base.
func (e *vmEmit) alloc(pc, want int, noBase bool) (Reg, bool) {
	if !e.cache {
		return 0, false
	}
	far := map[Reg]int{} // for each pool register in use, when it is next wanted
	for k, m := range e.holds {
		u := e.nextUse(k, pc)
		if u < 0 {
			u = 1 << 30
		}
		far[m] = max(far[m], u)
	}
	var best Reg
	bestAt := -1
	for _, m := range vmPool {
		if e.pinned[m] || (noBase && m == R12) {
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

// src makes sure r[r] is an INTEGER (exiting at pc otherwise, unless that is
// known) and returns a register holding its value: the one already holding
// it, a pool register it is loaded into when wanted again, or scratch.
func (e *vmEmit) src(pc, r int, scratch Reg) Reg {
	if !e.know.isInt[r] {
		e.guardInt(pc, r)
		e.know.isInt[r] = true
	}
	if m, ok := e.holds[r]; ok {
		e.pinned[m] = true
		return m
	}
	if u := e.nextUse(r, pc); u >= 0 {
		if m, ok := e.alloc(pc, u, false); ok {
			e.a.MovRegMemD(m, RSI, e.d(r, e.lay.OffI))
			e.holds[r] = m
			return m
		}
	}
	e.a.MovRegMemD(scratch, RSI, e.d(r, e.lay.OffI))
	return scratch
}

// needNoS exits at pc unless r[r] holds no string or blob, unless known.
func (e *vmEmit) needNoS(pc, r int) {
	if !e.know.noS[r] {
		e.guardNoS(pc, r)
		e.know.noS[r] = true
	}
}

// dest is the register to compute r's new value in: a pool register when the
// run reads r again, else scratch.
func (e *vmEmit) dest(pc, r int, scratch Reg) Reg {
	if u := e.nextUse(r, pc); u >= 0 {
		if m, ok := e.alloc(pc, u, false); ok {
			return m
		}
	}
	return scratch
}

// inPlace is the register holding r's value when the instruction at pc may
// overwrite it: no later step of the run reads it (or any register sharing
// it), so the result can be computed there. Otherwise ok is false.
func (e *vmEmit) inPlace(pc, r int) (Reg, bool) {
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

// storeInt makes r[r] the INTEGER in m and records it: only the I word when r
// is known to hold an integer Value native code wrote. A Dead store is not
// made at all. Every guard of the instruction has run.
func (e *vmEmit) storeInt(pc int, in VInsn, r int, m Reg) {
	for k, mk := range e.holds {
		if mk == m {
			delete(e.holds, k) // m now holds r's new value
		}
	}
	if in.Dead {
		e.clobber(r)
		return
	}
	if e.know.canon[r] {
		e.a.MovMemDReg(RSI, e.d(r, e.lay.OffI), m)
	} else {
		e.writeInt(r, m)
	}
	e.unmap(r)
	e.know.wroteInt(r)
	if !e.cache {
		return
	}
	if isPool(m) {
		e.holds[r] = m
	}
}

func isPool(m Reg) bool {
	for _, p := range vmPool {
		if p == m {
			return true
		}
	}
	return false
}

// operand2 loads the two integer operands of r[c] = r[b] op r[a]: x holds
// r[b] in a register the result may be computed in, y holds r[a].
func (e *vmEmit) operand2(pc int, in VInsn) (x, y Reg) {
	y = e.src(pc, in.A, RDX)
	bm := e.src(pc, in.B, RAX)
	if !in.Dead {
		e.destNoS(pc, in.C)
	}
	if bm == RAX {
		return RAX, y
	}
	if m, ok := e.inPlace(pc, in.B); ok && m == bm && in.A != in.B {
		return m, y
	}
	x = e.dest(pc, in.C, RAX)
	e.a.MovRegReg(x, bm)
	return x, y
}

func (e *vmEmit) insn(pc int, in VInsn) error {
	a, L := e.a, e.lay
	switch in.Op {
	case VExit:
		e.exitAt(pc)
	case VFused:
		// The instruction before is this one too.
	case VGoto:
		e.jmpBlock(pc, in.T)
	case VInt:
		if in.Dead {
			e.clobber(in.A)
			break
		}
		e.destNoS(pc, in.A)
		m := e.dest(pc, in.A, RAX)
		a.MovRegImm64(m, in.Imm)
		e.storeInt(pc, in, in.A, m)
	case VCopy:
		if in.Dead {
			e.clobber(in.B)
			break
		}
		if in.Imm != 0 && !e.know.noS[in.A] {
			e.copyAny(pc, in)
			break
		}
		e.needNoS(pc, in.A)
		e.destNoS(pc, in.B)
		if e.know.canon[in.A] && in.A != in.B {
			// An integer native code wrote: the copy is one, coalesced with
			// the source's register when it has one.
			m := e.src(pc, in.A, RAX)
			if e.know.canon[in.B] {
				a.MovMemDReg(RSI, e.d(in.B, L.OffI), m)
			} else {
				e.writeInt(in.B, m)
			}
			e.unmap(in.B)
			e.know.wroteInt(in.B)
			if isPool(m) {
				e.holds[in.B] = m
			}
			break
		}
		for _, off := range []int{0, L.OffI, L.OffF} {
			a.MovRegMemD(RAX, RSI, e.d(in.A, off))
			a.MovMemDReg(RSI, e.d(in.B, off), RAX)
		}
		e.clearS(in.B)
		if in.A != in.B {
			isInt := e.know.isInt[in.A]
			e.unmap(in.B)
			e.know.wroteAny(in.B, true)
			if isInt {
				e.know.isInt[in.B] = true
			}
			if m, ok := e.holds[in.A]; ok {
				e.holds[in.B] = m
			}
		}
	case VAdd, VSub, VMul, VAnd, VOr:
		x, y := e.operand2(pc, in)
		switch in.Op {
		case VAdd:
			a.AddRegReg(x, y)
		case VSub:
			a.SubRegReg(x, y)
		case VMul:
			a.ImulRegReg(x, y)
		case VAnd:
			a.AndRegReg(x, y)
		default:
			a.OrRegReg(x, y)
		}
		if in.Op == VAdd || in.Op == VSub || in.Op == VMul {
			e.jccStub(condOverflow, pc)
		}
		e.storeInt(pc, in, in.C, x)
	case VShl, VShr:
		sh := e.src(pc, in.A, RCX)
		if sh != RCX {
			a.MovRegReg(RCX, sh)
		}
		a.CmpRegImm32(RCX, 63)
		e.jccStub(condAbove, pc) // unsigned: a negative amount is huge
		x, _ := e.operand2(pc, in)
		if in.Op == VShl {
			a.ShlRegCL(x)
		} else {
			a.SarRegCL(x)
		}
		e.storeInt(pc, in, in.C, x)
	case VDiv, VRem:
		y := e.src(pc, in.A, R9)
		bm := e.src(pc, in.B, RAX)
		if !in.Dead {
			e.destNoS(pc, in.C)
		}
		// A divisor of 0 (NULL) or -1 (the one overflow) is the VDBE's.
		a.CmpRegImm32(y, 0)
		e.jccStub(CondE, pc)
		a.CmpRegImm32(y, -1)
		e.jccStub(CondE, pc)
		if bm != RAX {
			a.MovRegReg(RAX, bm)
		}
		a.Cqo()
		a.IdivReg(y)
		res := RAX
		if in.Op == VRem {
			res = RDX
		}
		if m := e.dest(pc, in.C, res); m != res {
			a.MovRegReg(m, res)
			res = m
		}
		e.storeInt(pc, in, in.C, res)
	case VCmpJump:
		x := e.src(pc, in.B, RAX)
		y := e.src(pc, in.A, RDX)
		a.CmpRegReg(x, y)
		e.jccBlock(in.Cond, pc, in.T)
	case VCmpSet, VCmpBr:
		x := e.src(pc, in.B, RAX)
		y := e.src(pc, in.A, RDX)
		if !in.Dead {
			e.destNoS(pc, in.C)
			a.MovRegImm64(R10, 0) // before the compare: a xor would clear the flags after it
		}
		a.CmpRegReg(x, y)
		if !in.Dead {
			a.Setcc(in.Cond, R10)
			m := e.dest(pc, in.C, R10)
			if m != R10 {
				a.MovRegReg(m, R10)
			}
			e.storeInt(pc, in, in.C, m) // MOVs leave the flags alone
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
		x := e.src(pc, in.A, RAX)
		if !in.Dead {
			e.destNoS(pc, in.C)
			a.MovRegImm64(R10, 0)
		}
		a.TestRegReg(x, x)
		if !in.Dead {
			a.Setcc(CondE, R10)
			m := e.dest(pc, in.C, R10)
			if m != R10 {
				a.MovRegReg(m, R10)
			}
			e.storeInt(pc, in, in.C, m)
		} else {
			e.clobber(in.C)
		}
		if in.Op == VNotBr {
			c := CondE // the NOT is 1: the operand was 0
			if in.Imm == 0 {
				c = CondNE
			}
			e.jccBlock(c, pc, in.T)
		}
	case VNullJump:
		if e.know.isInt[in.A] {
			// An INTEGER is not NULL.
			if in.Imm == 0 {
				e.jmpBlock(pc, in.T)
			}
			break
		}
		a.CmpMemDByte(RSI, e.d(in.A, L.OffTyp), L.TypNull)
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
		e.destNoS(pc, in.C)
		a.MovMemDImm(RSI, e.d(in.C, 0), int32(L.TypNull))
		a.MovMemDImm(RSI, e.d(in.C, L.OffI), 0)
		a.MovMemDImm(RSI, e.d(in.C, L.OffF), 0)
		e.clearS(in.C)
		e.unmap(in.C)
		e.know.wroteAny(in.C, true)
	case VNeg:
		am := e.src(pc, in.A, RAX)
		if !in.Dead {
			e.destNoS(pc, in.C)
		}
		x := RAX
		if m, ok := e.inPlace(pc, in.A); ok && m == am {
			x = m
		} else {
			x = e.dest(pc, in.C, RAX)
			if x != am {
				a.MovRegReg(x, am)
			}
		}
		a.NegReg(x)
		e.jccStub(condOverflow, pc) // -MinInt64 is the VDBE's
		e.storeInt(pc, in, in.C, x)
	case VIf:
		if e.know.isInt[in.A] {
			x := e.src(pc, in.A, RAX)
			a.TestRegReg(x, x)
			if in.Imm != 0 {
				e.jccBlock(CondNE, pc, in.T)
			} else {
				e.jccBlock(CondE, pc, in.T)
			}
			break
		}
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
		// Falling past here, r[A] was an integer or a NULL.
	case VGosub:
		e.destNoS(pc, in.A)
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
		if in.Dead {
			e.clobber(in.C)
			break
		}
		e.column(pc, in)
	case VResultRow:
		e.resultRow(pc, in)
	case VRewind:
		e.unmap(^in.A)
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
		e.unmap(^in.A)
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

// column is VColumn: r[C] = column B of cursor A's current row. R9/R10/RDX
// are scratch.
//
// The cursor's checks -- usable, positioned on a row, the row wide enough, the
// column readable -- are made by the first VColumn of the cursor in a run for
// every column the run reads from it before anything moves it, and the row's
// address is kept in a pool register for the rest; a later VColumn of the same
// cursor only checks its own value for a string. When one of those checks
// fails, native code exits at the FIRST VColumn, before anything of the run
// past it has run, which the VDBE then runs itself.
func (e *vmEmit) column(pc int, in VInsn) {
	a, L := e.a, e.lay
	cb := int32(in.A * VCursorSize)
	vb := int32(in.B) * int32(L.Size)
	row, have := e.holds[^in.A]
	if have {
		e.pinned[row] = true
	} else {
		mask, maxCol := uint64(1)<<uint(in.B), in.B
		if e.cache {
			mask, maxCol = vmRunColumns(e.prog, e.bound, pc, mask, maxCol)
		}
		a.MovRegMemD(RAX, RDI, POffCur)
		a.CmpMemDImm8(RAX, cb+VCursorOffOk, 0)
		e.jccStub(CondE, pc)
		a.MovRegMemD(RDX, RAX, cb+VCursorOffMask) // the columns must be ones the engine made readable
		a.MovRegImm64(R10, int64(mask))
		a.AndRegReg(RDX, R10)
		a.CmpRegReg(RDX, R10)
		e.jccStub(CondNE, pc)
		a.MovRegMemD(RDX, RAX, cb+VCursorOffPos)
		a.AluRegMemD(AluCmp, RDX, RAX, cb+VCursorOffN)
		e.jccStub(condAboveEq, pc) // unsigned: a position before the first row is huge
		a.MovRegImm64(R9, sliceHeaderSize)
		a.ImulRegReg(RDX, R9)
		a.MovRegMemD(R9, RAX, cb+VCursorOffRows)
		a.AddRegReg(R9, RDX)
		a.MovRegMemD(R10, R9, 8) // the row's length
		a.CmpRegImm32(R10, int32(maxCol))
		e.jccStub(condBelowEq, pc) // a row narrower than the column: the VDBE's
		row = R9
		if u := e.nextUse(^in.A, pc); u >= 0 {
			if m, ok := e.alloc(pc, u, true); ok {
				row = m
				e.holds[^in.A] = m
			}
		}
		a.MovRegMemD(row, R9, 0) // the row's first Value
	}
	// A string or blob is copied with its pointer (OP_Column's own copy is a
	// shallow one): a plain store with the write barrier off. With it on, a
	// pointer read or overwritten exits.
	// When neither the column nor the destination holds one, the S words
	// are nil in both and only the other three are copied.
	a.MovRegMemD(RDX, row, vb+int32(L.OffS))
	if !e.know.noS[in.C] {
		a.AluRegMemD(AluOr, RDX, RSI, e.d(in.C, L.OffS))
	} else {
		a.TestRegReg(RDX, RDX)
	}
	dst := e.d(in.C, 0)
	e.coldPath(func() {
		e.jccWB(pc)
		for _, off := range []int{L.OffS, L.OffS + 8, L.OffS + 16} {
			a.MovRegMemD(RDX, row, vb+int32(off))
			a.MovMemDReg(RSI, dst+int32(off), RDX)
		}
	})
	val := RDX
	if u := e.nextUse(in.C, pc); u >= 0 {
		if m, ok := e.alloc(pc, u, false); ok {
			val = m
		}
	}
	a.MovRegMemD(RDX, row, vb)
	a.MovMemDReg(RSI, e.d(in.C, 0), RDX)
	a.MovRegMemD(val, row, vb+int32(L.OffI))
	a.MovMemDReg(RSI, e.d(in.C, L.OffI), val)
	a.MovRegMemD(RDX, row, vb+int32(L.OffF))
	a.MovMemDReg(RSI, e.d(in.C, L.OffF), RDX)
	for k, m := range e.holds {
		if m == val && k != ^in.A {
			delete(e.holds, k)
		}
	}
	e.unmap(in.C)
	e.know.wroteAny(in.C, false)
	if val != RDX {
		e.holds[in.C] = val // its I word; whether it is an INTEGER is still to be checked
	}
}

// copyAny is VCopy of any Value (OP_SCopy): all six words, the string or blob
// pointer shared. A pointer read or overwritten needs the write barrier off;
// with it on, the copy exits.
func (e *vmEmit) copyAny(pc int, in VInsn) {
	a, L := e.a, e.lay
	if in.A == in.B {
		return // a copy onto itself changes nothing
	}
	a.MovRegMemD(RAX, RSI, e.d(in.A, L.OffS))
	if !e.know.noS[in.B] {
		a.AluRegMemD(AluOr, RAX, RSI, e.d(in.B, L.OffS))
	} else {
		a.TestRegReg(RAX, RAX)
	}
	src, dst := e.d(in.A, 0), e.d(in.B, 0)
	e.coldPath(func() {
		e.jccWB(pc)
		for _, off := range []int{L.OffS, L.OffS + 8, L.OffS + 16} {
			a.MovRegMemD(RDX, RSI, src+int32(off))
			a.MovMemDReg(RSI, dst+int32(off), RDX)
		}
	})
	for _, off := range []int{0, L.OffI, L.OffF} {
		a.MovRegMemD(RAX, RSI, e.d(in.A, off))
		a.MovMemDReg(RSI, e.d(in.B, off), RAX)
	}
	isInt := e.know.isInt[in.A]
	e.unmap(in.B)
	e.know.wroteAny(in.B, false)
	if isInt {
		e.know.isInt[in.B] = true
	}
	if m, ok := e.holds[in.A]; ok {
		e.holds[in.B] = m
	}
}

// resultRow is VResultRow (vm.go; ProgArgs.RowBuf has the protocol). With
// too little room, RowCap -1, or the write barrier on, it exits at pc. RCX,
// R9 and R10 are scratch: nothing is kept across it (vmRunStart).
func (e *vmEmit) resultRow(pc int, in VInsn) {
	a, L := e.a, e.lay
	e.jccWB(pc)
	a.MovRegMemD(RDX, RDI, POffRowsBuf)
	a.TestRegReg(RDX, RDX)
	gather := fmt.Sprintf("rg%d", pc)
	a.Jcc(CondNE, gather)

	// One row, handed over: copy it to RowBuf's head and return.
	a.MovRegMemD(RAX, RDI, POffRowCap)
	a.CmpRegImm32(RAX, int32(in.B))
	e.jccStub(CondL, pc)
	if in.B > 0 {
		a.MovRegMemD(R10, RDI, POffRowBuf)
		e.copyRow(pc, in)
	}
	a.MovMemDImm(RDI, POffYield, int32(in.B+1))
	a.MovMemDImm(RDI, POffPC, int32(pc+1))
	a.Ret()

	// Rows gathered: copy this one past the Values used, write its header
	// past the headers written, and go on. An empty row is the VDBE's: its
	// header would point past the buffer.
	a.Label(gather)
	if in.B == 0 {
		e.jmpStub(pc)
		return
	}
	a.MovRegMemD(RAX, RDI, POffYield)
	a.AluRegMemD(AluCmp, RAX, RDI, POffRowsCap)
	e.jccStub(CondGE, pc)
	a.MovRegMemD(R9, RDI, POffRowUsed)
	a.MovRegMemD(RCX, RDI, POffRowCap)
	a.SubRegReg(RCX, R9)
	a.CmpRegImm32(RCX, int32(in.B))
	e.jccStub(CondL, pc)
	// R10 = RowBuf + RowUsed*Size, the row's first Value.
	a.MovRegImm64(R10, int64(L.Size))
	a.ImulRegReg(R10, R9)
	a.AluRegMemD(AluAdd, R10, RDI, POffRowBuf)
	// The header at RowsBuf + Yield*24: {R10, B, B}.
	a.MovRegImm64(RCX, sliceHeaderSize)
	a.ImulRegReg(RCX, RAX)
	a.AddRegReg(RDX, RCX)
	a.MovMemDReg(RDX, 0, R10)
	a.MovMemDImm(RDX, 8, int32(in.B))
	a.MovMemDImm(RDX, 16, int32(in.B))
	a.IncReg(RAX)
	a.MovMemDReg(RDI, POffYield, RAX)
	a.MovRegImm64(RAX, int64(in.B))
	a.AddRegReg(R9, RAX)
	a.MovMemDReg(RDI, POffRowUsed, R9)
	e.copyRow(pc, in)
	// Falls into the next pc's block, which starts a run.
}

// copyRow copies the B registers from A, word by word, to R10 onward. RAX,
// RCX, R9 and R10 are scratch.
func (e *vmEmit) copyRow(pc int, in VInsn) {
	a, L := e.a, e.lay
	a.MovRegImm64(R9, int64(e.d(in.A, 0)))
	a.AddRegReg(R9, RSI)
	a.MovRegImm64(RCX, int64(in.B*L.Size/8))
	loop := fmt.Sprintf("rr%d.%d", pc, len(a.buf))
	a.Label(loop)
	a.MovRegMemD(RAX, R9, 0)
	a.MovMemDReg(R10, 0, RAX)
	a.AddRegImm8(R9, 8)
	a.AddRegImm8(R10, 8)
	a.DecReg(RCX)
	a.Jcc(CondNE, loop)
}

// patch writes the rel32 at buf[at:] so it lands on target.
func patch(buf []byte, at, target int) {
	rel := int32(target - (at + 4))
	buf[at], buf[at+1], buf[at+2], buf[at+3] = byte(rel), byte(rel>>8), byte(rel>>16), byte(rel>>24)
}

// padNops pads a's code to a multiple of align with the recommended
// multi-byte NOPs, the fewest instructions that fill the gap.
func padNops(a *Asm, align int) {
	if align <= 1 {
		return
	}
	nops := [][]byte{
		{0x90},
		{0x66, 0x90},
		{0x0F, 0x1F, 0x00},
		{0x0F, 0x1F, 0x40, 0x00},
		{0x0F, 0x1F, 0x44, 0x00, 0x00},
		{0x66, 0x0F, 0x1F, 0x44, 0x00, 0x00},
		{0x0F, 0x1F, 0x80, 0x00, 0x00, 0x00, 0x00},
		{0x0F, 0x1F, 0x84, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0x66, 0x0F, 0x1F, 0x84, 0x00, 0x00, 0x00, 0x00, 0x00},
	}
	for gap := (align - len(a.buf)%align) % align; gap > 0; {
		k := min(gap, len(nops))
		a.emit(nops[k-1]...)
		gap -= k
	}
}
