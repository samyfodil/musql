package jit

import (
	"slices"
	"sync/atomic"
)

// FUSION FOR THE WHOLE-PROGRAM JIT: the same pass fuse.go runs over a segment
// program, over a VDBE program's native form. Each step's contract -- the
// registers it reads and writes, where control goes after it -- is stated once
// (vmStepOf, in progStep's terms), a liveness pass over the program's control
// flow says which values are still wanted where, and a rewrite is made only
// where the contract and the liveness say it is exact:
//
//   - a comparison stored into a register and the test of that register right
//     after become one compare-and-branch (VCmpBr), and a NOT and its test one
//     test (VNotBr). A chain of any length -- the VDBE spells "a AND b AND c" as
//     compare, test, compare, test -- fuses pair by pair, and the emitter keeps
//     each operand in a machine register from the step that produced it;
//   - a store whose value nothing reads before the register is written again
//     is marked Dead, and the emitter skips it.
//
// The register file stays the VDBE's (vm.go's contract): every store a later
// instruction, an exit or the VDBE could read is still made, in order. What
// changes is how often a value goes through memory between two native steps.
//
// The emitters add the other half (vmKnow): within a straight run, which
// registers are known to hold an INTEGER, to hold no string, or to be held in
// a machine register already, so a guard or a load a step before it already
// made is not made again.
//
// A VDBE program can be entered at any pc the engine marks (entry), jumped to
// at any target, and returned into after any Gosub. Those are the BOUNDARIES:
// a fused pair never spans one, and the emitters forget what they know at
// each.

// VFusable reports whether next, appended right after prev, is a pair the VM
// fusion pass works on: a comparison or NOT stored into a register and the
// test of it, or a step reading what the one before wrote.
func VFusable(prev, next VInsn) bool {
	if next.Op == VIf && (prev.Op == VCmpSet || prev.Op == VNot) && next.A == prev.C {
		return true
	}
	ps, ns := vmStepOf(prev, 0), vmStepOf(next, 1)
	for _, d := range ps.defs {
		if slicesContains(ns.uses, d) {
			return true
		}
	}
	return false
}

// vmStepOf is the contract of in at pc, in the VDBE's own terms: what the
// instruction reads and writes and where it goes, whether or not native code
// runs it (an exit hands the same instruction to the VDBE). An instruction
// with no native form reads every register: it is VDBE code, which may.
func vmStepOf(in VInsn, pc int) progStep {
	s := progStep{fall: true}
	switch in.Op {
	case VGoto:
		s.fall, s.targets = false, []int{in.T}
	case VInt:
		s.defs = []int{in.A}
	case VCopy:
		s.uses, s.defs = []int{in.A}, []int{in.B}
	case VAdd, VSub, VMul, VAnd, VOr, VShl, VShr, VDiv, VRem, VCmpSet:
		s.uses, s.defs = []int{in.A, in.B}, []int{in.C}
	case VCmpJump:
		s.uses, s.targets = []int{in.A, in.B}, []int{in.T}
	case VCmpBr:
		s.uses, s.defs, s.targets = []int{in.A, in.B}, []int{in.C}, []int{in.T}
	case VNotBr:
		s.uses, s.defs, s.targets = []int{in.A}, []int{in.C}, []int{in.T}
	case VNullJump, VIf:
		s.uses, s.targets = []int{in.A}, []int{in.T}
	case VNull:
		s.defs = []int{in.C}
	case VNeg, VNot:
		s.uses, s.defs = []int{in.A}, []int{in.C}
	case VGosub:
		// The subroutine's VReturn (which reads everything) comes back to
		// pc+1; the return address is a def.
		s.defs, s.fall, s.targets = []int{in.A}, false, []int{in.T}
	case VColumn:
		s.defs = []int{in.C}
	case VRewind, VNext:
		s.targets = []int{in.T}
	case VFused:
		// Part of the instruction before. An entry at it (VFusedTest) comes
		// from the VDBE, which keeps every register in the file.
	default:
		// VExit, VReturn, VResultRow: VDBE code, a jump anywhere, or a return
		// to Go.
		s.useAll = true
	}
	return s
}

// vmLiveness returns, per pc, the registers live after it. Past the end of the
// program -- and at any target outside it -- everything is live: the engine
// reads a program's result registers after it halts.
func vmLiveness(prog []VInsn, nReg int) []regSet {
	n := len(prog)
	steps := make([]progStep, n)
	for pc, in := range prog {
		steps[pc] = vmStepOf(in, pc)
	}
	all := newRegSet(nReg)
	all.fill()
	liveIn := make([]regSet, n)
	liveOut := make([]regSet, n)
	for i := range liveIn {
		liveIn[i], liveOut[i] = newRegSet(nReg), newRegSet(nReg)
	}
	at := func(t int) regSet {
		if t < 0 || t >= n {
			return all
		}
		return liveIn[t]
	}
	for changed := true; changed; {
		changed = false
		for i := n - 1; i >= 0; i-- {
			st := steps[i]
			if st.useAll {
				if liveIn[i].union(all) {
					changed = true
				}
				continue
			}
			if st.fall {
				liveOut[i].union(at(i + 1))
			}
			for _, t := range st.targets {
				liveOut[i].union(at(t))
			}
			in := newRegSet(nReg)
			copy(in, liveOut[i])
			for _, r := range st.defs {
				in.del(r)
			}
			for _, r := range st.uses {
				if r >= 0 && r < nReg {
					in.add(r)
				}
			}
			if liveIn[i].union(in) {
				changed = true
			}
		}
	}
	return liveOut
}

// VMBoundaries marks the pcs control can arrive at other than by falling from
// the pc before: an entry, a jump target, the return point of a Gosub. entry
// nil means every pc is an entry.
func VMBoundaries(prog []VInsn, entry []bool) []bool {
	n := len(prog)
	b := make([]bool, n+1)
	for pc, in := range prog {
		if entry == nil || (pc < len(entry) && entry[pc]) {
			b[pc] = true
		}
		for _, t := range vmStepOf(in, pc).targets {
			if t >= 0 && t < n {
				b[t] = true
			}
		}
		if in.Op == VGosub {
			b[pc+1] = true
		}
	}
	b[n] = true
	return b
}

// FuseVM is the fusion pass over a VM program: pairs fused and dead stores
// marked, as the top of this file describes. hard marks the pcs control
// reaches by a jump or a return (VMBoundaries with no entries); no pair spans
// one. An entry the engine wanted at a pair's second pc is kept: the emitters
// give it a block of its own that runs the test unfused (VFusedTest) and
// rejoins the run after the pair. The result has one instruction per pc, like
// prog.
func FuseVM(prog []VInsn, nReg int, hard, entry []bool) []VInsn {
	n := len(prog)
	out := append([]VInsn(nil), prog...)
	if n == 0 || nReg <= 0 {
		return out
	}
	live := vmLiveness(out, nReg)
	dead := func(pc, r int) bool { return r >= 0 && r < nReg && !live[pc].has(r) }
	for pc := 0; pc+1 < n; pc++ {
		in, nx := out[pc], out[pc+1]
		if hard[pc+1] || nx.Op != VIf || nx.A != in.C {
			continue
		}
		var op VOp
		switch in.Op {
		case VCmpSet:
			op = VCmpBr
		case VNot:
			op = VNotBr
		default:
			continue
		}
		// The test's NULL arm (C) cannot matter: the fused compare or NOT
		// has an INTEGER result whenever it runs natively.
		out[pc] = VInsn{Op: op, A: in.A, B: in.B, C: in.C, Cond: in.Cond, T: nx.T, Imm: nx.Imm, Dead: dead(pc+1, in.C)}
		// The second half keeps its test, so an entry there -- the VDBE ran
		// the comparison itself and comes back for the test -- runs it alone.
		out[pc+1] = VInsn{Op: VFused, A: nx.A, C: nx.C, T: nx.T, Imm: nx.Imm}
		vmFusedPairs.Add(1)
		pc++
	}
	// A store nothing reads. (A fused pair's was decided above, after its test.)
	// A VColumn is never marked: the VDBE's column read can report a corrupt
	// record, which skipping it would hide.
	for pc, in := range out {
		var d int
		switch in.Op {
		case VInt:
			d = in.A
		case VCopy:
			d = in.B
		case VAdd, VSub, VMul, VAnd, VOr, VShl, VShr, VDiv, VRem, VCmpSet, VNull, VNeg, VNot:
			d = in.C
		default:
			continue
		}
		if dead(pc, d) {
			out[pc].Dead = true
			vmDeadStores.Add(1)
		}
	}
	return out
}

// EmitVMFused compiles prog for an engine that enters it only at the pcs entry
// marks, through the fusion pass when fusable (VFusable over the program's
// adjacent pairs, found as it was built) says it has a pair to work on and
// fusion is on. The pass may give up entries (FuseVM); entry is updated, and
// the engine must enter only where it still says. EmitVM is the same with
// every pc an entry and no pass.
func EmitVMFused(prog []VInsn, nReg int, lay ValueLayout, entry []bool, fusable bool) ([]byte, error) {
	if !Fusing() {
		return emitVM(prog, nReg, lay, nil, nil, false)
	}
	if fusable {
		prog = FuseVM(prog, nReg, VMBoundaries(prog, make([]bool, len(prog))), entry)
	}
	return emitVM(prog, nReg, lay, VMBoundaries(prog, make([]bool, len(prog))), entry, true)
}

// EmitVM compiles a whole VDBE program that may be entered at any pc; see
// vm.go for the contract.
func EmitVM(prog []VInsn, nReg int, lay ValueLayout) ([]byte, error) {
	return emitVM(prog, nReg, lay, nil, nil, false)
}

// The pass's accounting, for a test to see it ran.
var vmFusedPairs, vmDeadStores atomic.Int64

// VMFusionStatsForTest reads and clears how many pairs the VM pass fused and
// how many stores it found dead.
func VMFusionStatsForTest() (pairs, dead int64) {
	return vmFusedPairs.Swap(0), vmDeadStores.Swap(0)
}

// vmKnow is what an emitter knows about the register file at the current
// point of a straight run: registers known to hold an INTEGER (a guard
// passed, or native code wrote one), known to hold no string or blob, and
// known to hold exactly Value{Typ: Int, I: x} with the rest zero -- written
// by native code in this run, so another integer needs only its I word.
// Everything is forgotten at a boundary.
type vmKnow struct {
	isInt, noS, canon map[int]bool
}

func newVMKnow() *vmKnow {
	return &vmKnow{isInt: map[int]bool{}, noS: map[int]bool{}, canon: map[int]bool{}}
}

func (k *vmKnow) reset() {
	clear(k.isInt)
	clear(k.noS)
	clear(k.canon)
}

// wroteInt records that native code made r the integer Value.
func (k *vmKnow) wroteInt(r int) { k.isInt[r], k.noS[r], k.canon[r] = true, true, true }

// wroteAny records that native code wrote r a value of unknown type (a copy
// of a column or a register, a NULL); noS is whether it is known to hold no
// string or blob.
func (k *vmKnow) wroteAny(r int, noS bool) {
	delete(k.isInt, r)
	delete(k.canon, r)
	if noS {
		k.noS[r] = true
	} else {
		delete(k.noS, r)
	}
}

// vmRunStart reports whether pc starts a new straight run: a boundary, or the
// first pc after one that does not fall through.
func vmRunStart(prog []VInsn, bound []bool, pc int) bool {
	if bound == nil || bound[pc] || pc == 0 {
		return true
	}
	prev := prog[pc-1]
	if prev.Op == VFused {
		return false
	}
	return !vmStepOf(prev, pc-1).fall || prev.Op == VExit || prev.Op == VReturn || prev.Op == VResultRow
}

// VFusedTest is the test a VFused pc was before its pair was fused: what an
// entry at that pc runs.
func VFusedTest(in VInsn) VInsn {
	return VInsn{Op: VIf, A: in.A, C: in.C, T: in.T, Imm: in.Imm}
}

// vmNextUse is the first pc at or after from, within the straight run, that
// reads r; -1 when none does.
func vmNextUse(prog []VInsn, bound []bool, from, r int) int {
	for pc := from; pc < len(prog); pc++ {
		if pc > from && vmRunStart(prog, bound, pc) {
			return -1
		}
		st := vmStepOf(prog[pc], pc)
		if st.useAll || slicesContains(st.uses, r) {
			return pc
		}
		if slicesContains(st.defs, r) {
			return -1
		}
	}
	return -1
}

// vmLoopHeads marks every pc some branch at or after it jumps back to: the
// heads of loops, which the emitters align.
func vmLoopHeads(prog []VInsn) []bool {
	h := make([]bool, len(prog)+1)
	for q, in := range prog {
		for _, t := range vmStepOf(in, q).targets {
			if t >= 0 && t <= q {
				h[t] = true
			}
		}
	}
	return h
}

// vmEmitNextUse is the first pc after pc in its straight run that reads key:
// a register, or a cursor's row (^c) -- a VColumn of the cursor before
// anything moves it. -1: none does.
func vmEmitNextUse(prog []VInsn, bound []bool, key, pc int) int {
	for q := pc + 1; q < len(prog); q++ {
		if vmRunStart(prog, bound, q) {
			return -1
		}
		in := prog[q]
		if key < 0 {
			c := ^key
			switch {
			case in.Op == VColumn && in.A == c:
				return q
			case (in.Op == VNext || in.Op == VRewind) && in.A == c:
				return -1
			case in.Op == VExit || in.Op == VReturn || in.Op == VResultRow:
				return -1
			}
			continue
		}
		st := vmStepOf(in, q)
		if st.useAll {
			return -1 // the run ends there
		}
		if slicesContains(st.uses, key) {
			return q
		}
		if slicesContains(st.defs, key) {
			return -1
		}
	}
	return -1
}

// vmRunColumns adds to mask and maxCol the columns of the cursor pc's VColumn
// reads that later VColumns of the same run read before anything moves it:
// what the first one checks for them all.
func vmRunColumns(prog []VInsn, bound []bool, pc int, mask uint64, maxCol int) (uint64, int) {
	c := prog[pc].A
	for q := pc + 1; q < len(prog) && q < pc+48; q++ {
		if vmRunStart(prog, bound, q) {
			break
		}
		in := prog[q]
		if (in.Op == VNext || in.Op == VRewind) && in.A == c {
			break
		}
		if in.Op == VColumn && in.A == c {
			mask |= uint64(1) << uint(in.B)
			maxCol = max(maxCol, in.B)
		}
	}
	return mask, maxCol
}

// ENTRIES INSIDE A RUN. The engine enters native code at many pcs -- after
// running an instruction itself, it enters at the next one native code has --
// and a run whose knowledge stopped at every entry would hardly be a run. So
// an entry that is not a jump target goes through its own ENTRY STUB, out of
// line: the stub checks what the run knows at that pc (exiting there when it
// does not hold, which hands the instruction back to the VDBE), loads the
// registers the run keeps in machine registers, and jumps into the run. What
// the run knows at an entry is first cut down to what the stub can check
// cheaply and the rest of the run uses (vmEntryKeep): no cursor's row (its
// checks are a VColumn's), no "written by native code" (a Value the VDBE
// wrote may have the same type with other bytes set), and nothing about a
// register the rest of the run does not mention.

// vmEntryKeep is the registers the run from pc on reads or writes.
func vmEntryKeep(prog []VInsn, bound []bool, pc int) map[int]bool {
	keep := map[int]bool{}
	for q := pc; q < len(prog); q++ {
		if q > pc && vmRunStart(prog, bound, q) {
			break
		}
		st := vmStepOf(prog[q], q)
		if st.useAll {
			break
		}
		for _, r := range st.uses {
			keep[r] = true
		}
		for _, r := range st.defs {
			keep[r] = true
		}
	}
	return keep
}

// pruneForEntry cuts k down to what an entry stub at pc establishes, and
// returns the registers it must check: those known to be INTEGERs, and those
// known to hold no string.
func (k *vmKnow) pruneForEntry(keep map[int]bool) (ints, noS []int) {
	clear(k.canon)
	for r := range k.isInt {
		if keep[r] {
			ints = append(ints, r)
		} else {
			delete(k.isInt, r)
		}
	}
	for r := range k.noS {
		if keep[r] {
			noS = append(noS, r)
		} else {
			delete(k.noS, r)
		}
	}
	slices.Sort(ints)
	slices.Sort(noS)
	return ints, noS
}

// loopAlign is the byte boundary the emitters start a loop's head on (the
// program JIT's row loop, the VM JIT's backward-jump targets): the loop's
// code then sits in the same fetch blocks whatever precedes it.
// 0 or 1 turns it off, for a benchmark to compare.
var loopAlign = 64

// vmFusedEntry reports whether pc is the second pc of a fused pair that the
// engine enters at: an entry that runs the pair's test alone (VFusedTest).
func vmFusedEntry(prog []VInsn, entry []bool, pc int) bool {
	return pc >= 0 && pc < len(prog) && pc < len(entry) && entry[pc] && prog[pc].Op == VFused
}
