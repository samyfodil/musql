package jit

import (
	"math/bits"
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
	if next.Op == VIf && (prev.Op == VCmpSet || prev.Op == VFCmpSet || prev.Op == VNot) && next.A == prev.C {
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
	s := progStep{fall: vmFalls(in.Op)}
	switch in.Op {
	case VGoto:
		s.targets = []int{in.T}
	case VInt:
		s.defs = []int{in.A}
	case VCopy:
		s.uses, s.defs = []int{in.A}, []int{in.B}
	case VAdd, VSub, VMul, VAnd, VOr, VShl, VShr, VDiv, VRem, VCmpSet, VFAdd, VFSub, VFMul, VFDiv, VFCmpSet:
		s.uses, s.defs = []int{in.A, in.B}, []int{in.C}
	case VCmpJump, VFCmpJump:
		s.uses, s.targets = []int{in.A, in.B}, []int{in.T}
	case VCmpBr, VFCmpBr:
		s.uses, s.defs, s.targets = []int{in.A, in.B}, []int{in.C}, []int{in.T}
	case VReal, VText:
		s.defs = []int{in.A}
	case VMath:
		s.uses, s.defs = []int{in.A}, []int{in.C}
		if in.B == 2 {
			s.uses = append(s.uses, in.A+1)
		}
	case VSubstr:
		s.uses, s.defs = []int{in.A, in.A + 1, in.A + 2}, []int{in.C}
	case VTextCmpJump:
		s.uses, s.targets = []int{in.A, in.B}, []int{in.T}
	case VTextCmpSet, VLike:
		s.uses, s.defs = []int{in.A, in.B}, []int{in.C}
	case VInSub:
		s.uses, s.defs = []int{in.A}, []int{in.C}
	case VNotBr:
		s.uses, s.defs, s.targets = []int{in.A}, []int{in.C}, []int{in.T}
	case VNullJump, VIf:
		s.uses, s.targets = []int{in.A}, []int{in.T}
	case VIntGuard:
		s.uses = []int{in.A}
	case VNull:
		s.defs = []int{in.C}
	case VNeg, VNot:
		s.uses, s.defs = []int{in.A}, []int{in.C}
	case VGosub:
		// The subroutine's VReturn (which reads everything) comes back to
		// pc+1; the return address is a def.
		s.defs, s.targets = []int{in.A}, []int{in.T}
	case VColumn, VRowid:
		s.defs = []int{in.C}
	case VInSet:
		// The keys from B on are written by the VDBE only, whose exits read
		// every register.
		s.uses, s.defs = []int{in.A, int(in.Imm)}, []int{in.C}
	case VRewind, VNext:
		s.targets = []int{in.T}
	case VSeekRowid, VSeekIdx:
		s.uses, s.targets = []int{in.B}, []int{in.T}
	case VInText:
		s.uses, s.defs = []int{in.A}, []int{in.C}
	case VSeekRewind, VNullRow:
	case VOpen, VClose:
	case VResultRow:
		if in.Skip == 0 {
			s.useAll = true // VDBE code reads the row, or a return to Go
			break
		}
		// A gathered row that leaves some registers out (Skip) reads only
		// the others, and goes on to the next pc.
		for i := 0; i < in.B; i++ {
			if in.Skip&(1<<uint(i)) == 0 {
				s.uses = append(s.uses, in.A+i)
			}
		}
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
		imm := nx.Imm
		switch in.Op {
		case VCmpSet:
			if in.Imm != 0 {
				// A NULL operand: no store but NULL's, and the test's NULL arm.
				imm |= VCmpNull
				if nx.C != 0 {
					imm |= VCmpNullJumps
				}
			}
			op = VCmpBr
		case VFCmpSet:
			op = VFCmpBr
		case VNot:
			op = VNotBr
		default:
			continue
		}
		// The test's NULL arm (C) matters only to a compare that answers a
		// NULL operand (VCmpNullJumps): any other fused compare or NOT has an
		// INTEGER result whenever it runs natively.
		out[pc] = VInsn{Op: op, A: in.A, B: in.B, C: in.C, Cond: in.Cond, T: nx.T, Imm: imm, Dead: dead(pc+1, in.C)}
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
		case VAdd, VSub, VMul, VAnd, VOr, VShl, VShr, VDiv, VRem, VCmpSet, VNull, VNeg, VNot,
			VFAdd, VFSub, VFMul, VFDiv, VFCmpSet, VMath, VLike, VInSub:
			d = in.C
		case VReal, VText:
			d = in.A
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

// MapVMFused is EmitVMFused mapped (Map), the encoder's buffers reused from
// one compile to the next.
func MapVMFused(prog []VInsn, nReg int, lay ValueLayout, entry []bool, fusable bool) (*Code, error) {
	if !Fusing() {
		return mapVM(prog, nReg, lay, nil, nil, false)
	}
	if fusable {
		prog = FuseVM(prog, nReg, VMBoundaries(prog, make([]bool, len(prog))), entry)
	}
	return mapVM(prog, nReg, lay, VMBoundaries(prog, make([]bool, len(prog))), entry, true)
}

// EmitVM compiles a whole VDBE program that may be entered at any pc; see
// vm.go for the contract.
func EmitVM(prog []VInsn, nReg int, lay ValueLayout) ([]byte, error) {
	return emitVM(prog, nReg, lay, nil, nil, false)
}

// The pass's accounting, for a test to see it ran.
var vmFusedPairs, vmDeadStores atomic.Int64

// vmDivConsts counts the divisions emitted without a divide instruction
// (divConst), for a test to see they were.
var vmDivConsts atomic.Int64

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
	isInt, noS, canon knowSet
	// isFloat: known to hold a REAL (a guard passed, or native code wrote
	// one).
	isFloat knowSet
	// konst: known to hold the INTEGER konstV holds for it, which a VInt of
	// this run wrote with nothing writing the register since.
	konst  knowSet
	konstV []int64
}

// knowSet is a set of registers, a bitmap that grows to the highest one
// added.
type knowSet []uint64

func (s knowSet) has(r int) bool { return r >= 0 && r/64 < len(s) && s[r/64]&(1<<(r%64)) != 0 }

func (s *knowSet) add(r int) {
	if r < 0 {
		return
	}
	if n := r/64 + 1; n > len(*s) {
		*s = append(*s, make([]uint64, n-len(*s))...)
	}
	(*s)[r/64] |= 1 << (r % 64)
}

func (s knowSet) del(r int) {
	if r >= 0 && r/64 < len(s) {
		s[r/64] &^= 1 << (r % 64)
	}
}

func (s knowSet) any() bool {
	for _, w := range s {
		if w != 0 {
			return true
		}
	}
	return false
}

// each calls f for every register in s, in ascending order.
func (s knowSet) each(f func(r int)) {
	for i, w := range s {
		for ; w != 0; w &= w - 1 {
			f(i*64 + bits.TrailingZeros64(w))
		}
	}
}

func newVMKnow() *vmKnow { return &vmKnow{} }

func (k *vmKnow) reset() {
	clear(k.isInt)
	clear(k.noS)
	clear(k.canon)
	clear(k.isFloat)
	clear(k.konst)
}

func (k *vmKnow) konstOf(r int) (int64, bool) {
	if !k.konst.has(r) {
		return 0, false
	}
	return k.konstV[r], true
}

func (k *vmKnow) setKonst(r int, v int64) {
	if r < 0 {
		return
	}
	k.konst.add(r)
	if r >= len(k.konstV) {
		k.konstV = append(k.konstV, make([]int64, r+1-len(k.konstV))...)
	}
	k.konstV[r] = v
}

// wrote forgets the constants in the registers in writes, before it is
// emitted.
func (k *vmKnow) wrote(in VInsn, pc int) {
	for _, r := range vmStepOf(in, pc).defs {
		k.konst.del(r)
	}
}

// wroteInt records that native code made r the integer Value.
func (k *vmKnow) wroteInt(r int) {
	k.isInt.add(r)
	k.noS.add(r)
	k.canon.add(r)
	k.isFloat.del(r)
}

// wroteFloat records that native code made r a REAL Value.
func (k *vmKnow) wroteFloat(r int) {
	k.isFloat.add(r)
	k.noS.add(r)
	k.isInt.del(r)
	k.canon.del(r)
}

// wroteAny records that native code wrote r a value of unknown type (a copy
// of a column or a register, a NULL); noS is whether it is known to hold no
// string or blob.
func (k *vmKnow) wroteAny(r int, noS bool) {
	k.isInt.del(r)
	k.canon.del(r)
	k.isFloat.del(r)
	if noS {
		k.noS.add(r)
	} else {
		k.noS.del(r)
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
	return !vmFalls(prev.Op) || prev.Op == VExit || prev.Op == VReturn || prev.Op == VResultRow
}

// vmFalls reports whether op goes on to the next pc (vmStepOf's fall), which
// the run boundaries ask of every pc and so is answered without building a
// step: only a jump and a subroutine call leave it.
func vmFalls(op VOp) bool { return op != VGoto && op != VGosub }

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
	return vmEmitNextUseIn(prog, nil, bound, key, pc)
}

// vmStepsOf is every pc's step, made once for an emitter, which asks about the
// same instructions again and again (vmEmitNextUseIn, vmEntryKeepInto).
func vmStepsOf(prog []VInsn) []progStep {
	steps := make([]progStep, len(prog))
	for pc, in := range prog {
		steps[pc] = vmStepOf(in, pc)
	}
	return steps
}

// vmStepAt is pc's step, from steps when the caller made them.
func vmStepAt(prog []VInsn, steps []progStep, pc int) progStep {
	if steps != nil {
		return steps[pc]
	}
	return vmStepOf(prog[pc], pc)
}

// vmEmitNextUseIn is vmEmitNextUse reading the steps from steps (vmStepsOf).
func vmEmitNextUseIn(prog []VInsn, steps []progStep, bound []bool, key, pc int) int {
	for q := pc + 1; q < len(prog); q++ {
		if vmRunStart(prog, bound, q) {
			return -1
		}
		in := prog[q]
		if key < 0 {
			c := ^key
			if c >= vmRowKeyBase {
				c -= vmRowKeyBase
			}
			switch {
			case in.Op == VColumn && in.A == c:
				return q
			case vmMovesCursor(in.Op) && in.A == c:
				return -1
			case in.Op == VExit || in.Op == VReturn || in.Op == VResultRow:
				return -1
			}
			continue
		}
		st := vmStepAt(prog, steps, q)
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

// vmRowKeyBase sets apart a cursor's second held key, vmRowKey: the address of
// the row a segment-capable cursor is on when it reads its row list, next to
// ^c, the cursor's own.
const vmRowKeyBase = 1 << 16

// vmRowKey is cursor c's row key: wanted where c's cursor key is, and dropped
// with it.
func vmRowKey(c int) int { return ^(c + vmRowKeyBase) }

// vmMovesCursor reports whether op moves its cursor off the row a VColumn
// before it read.
func vmMovesCursor(op VOp) bool {
	return op == VNext || op == VRewind || op == VOpen || op == VClose || op == VSeekRowid || op == VSeekIdx || op == VSeekRewind ||
		op == VNullRow
}

// vmRunPredicted is vmRunColumns for a segment column read with a predicted
// kind (T): only the run's predicted reads of the cursor share its check, so
// a column read the full way, which may not be served, cannot fail it.
func vmRunPredicted(prog []VInsn, bound []bool, pc int) uint64 {
	c, mask := prog[pc].A, uint64(1)<<uint(prog[pc].B)
	for q := pc + 1; q < len(prog) && q < pc+48; q++ {
		if vmRunStart(prog, bound, q) {
			break
		}
		in := prog[q]
		if vmMovesCursor(in.Op) && in.A == c {
			break
		}
		if in.Op == VColumn && in.A == c && in.Imm == 1 && in.T != 0 {
			mask |= uint64(1) << uint(in.B)
		}
	}
	return mask
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
		if vmMovesCursor(in.Op) && in.A == c {
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

// vmEntryKeepInto is the registers the run from pc on reads or writes, into
// keep, cleared first, reading the steps from steps (vmStepsOf).
func vmEntryKeepInto(keep knowSet, prog []VInsn, steps []progStep, bound []bool, pc int) knowSet {
	clear(keep)
	for q := pc; q < len(prog); q++ {
		if q > pc && vmRunStart(prog, bound, q) {
			break
		}
		st := vmStepAt(prog, steps, q)
		if st.useAll {
			break
		}
		for _, r := range st.uses {
			keep.add(r)
		}
		for _, r := range st.defs {
			keep.add(r)
		}
	}
	return keep
}

// pruneForEntry cuts k down to what an entry stub at pc establishes, and
// returns the registers it must check: those known to be INTEGERs, and those
// known to hold no string.
//
// The two lists are carved from arena, which grows as entries are made; a
// list already handed out keeps the backing it was carved from.
func (k *vmKnow) pruneForEntry(keep knowSet, arena *[]int) (ints, noS []int) {
	clear(k.canon)
	clear(k.isFloat) // a REAL op guards its operands again
	clear(k.konst)
	start := len(*arena)
	k.isInt.each(func(r int) {
		if keep.has(r) {
			*arena = append(*arena, r)
		} else {
			k.isInt.del(r)
		}
	})
	mid := len(*arena)
	k.noS.each(func(r int) {
		if keep.has(r) {
			*arena = append(*arena, r)
		} else {
			k.noS.del(r)
		}
	})
	a := *arena
	if mid > start {
		ints = a[start:mid:mid]
	}
	if len(a) > mid {
		noS = a[mid:len(a):len(a)]
	}
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

// VReachesResultRow marks the pcs from which native code can reach a
// VResultRow without leaving native code: what an entry must hand a row buffer
// to. A VReturn may go back to any Gosub, so it reaches one when any is
// reached from the pc after a Gosub.
func VReachesResultRow(prog []VInsn) []bool {
	n := len(prog)
	reach := make([]bool, n)
	for changed := true; changed; {
		changed = false
		retReach := false
		for pc, in := range prog {
			if in.Op == VGosub && pc+1 < n && reach[pc+1] {
				retReach = true
			}
		}
		for pc := n - 1; pc >= 0; pc-- {
			if reach[pc] {
				continue
			}
			in := prog[pc]
			r := false
			switch in.Op {
			case VResultRow:
				r = true
			case VExit:
			case VReturn:
				r = retReach
			default:
				st := vmStepOf(in, pc)
				if st.fall && pc+1 < n && reach[pc+1] {
					r = true
				}
				for _, t := range st.targets {
					if t >= 0 && t < n && reach[t] {
						r = true
					}
				}
			}
			if r {
				reach[pc], changed = true, true
			}
		}
	}
	return reach
}

// VLoops reports whether some native instruction of prog can transfer back to
// itself or an earlier one: a jump back, or a subroutine.
func VLoops(prog []VInsn) bool {
	for pc, in := range prog {
		switch in.Op {
		case VExit:
			continue
		case VGosub, VReturn:
			return true
		}
		for _, t := range vmStepOf(in, pc).targets {
			if t <= pc {
				return true
			}
		}
	}
	return false
}

// VReachMarks reports, for every pc at once, the marks of the instructions
// native code entered there can run before it exits, itself included: bit m
// of reach[pc] is set when one of them has mark m, its entry in mark (-1 for
// none). A reachable VReturn may return past any VGosub. One fixpoint over
// the program, where a walk per entry cost entries times the program.
func VReachMarks(prog []VInsn, mark []int, nMarks int) [][]uint64 {
	n := len(prog)
	words := (nMarks + 63) / 64
	reach := make([][]uint64, n)
	flat := make([]uint64, n*words)
	var returns []int
	for g, in := range prog {
		if in.Op == VGosub {
			returns = append(returns, g+1)
		}
	}
	succ := make([][]int, n)
	for pc, in := range prog {
		reach[pc] = flat[pc*words : (pc+1)*words : (pc+1)*words]
		if m := mark[pc]; m >= 0 {
			reach[pc][m/64] |= 1 << uint(m%64)
		}
		switch in.Op {
		case VExit:
		case VReturn:
			succ[pc] = returns
		default:
			st := vmStepOf(in, pc)
			if st.fall {
				succ[pc] = append(succ[pc], pc+1)
			}
			succ[pc] = append(succ[pc], st.targets...)
		}
	}
	for changed := true; changed; {
		changed = false
		for pc := n - 1; pc >= 0; pc-- {
			for _, s := range succ[pc] {
				if s < 0 || s >= n || s == pc {
					continue
				}
				for w, v := range reach[s] {
					if u := reach[pc][w] | v; u != reach[pc][w] {
						reach[pc][w], changed = u, true
					}
				}
			}
		}
	}
	return reach
}
