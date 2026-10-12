package jit

import (
	"sync/atomic"
	"time"
)

// FUSION: the pass every program goes through on its way to an emitter.
//
// The IR is written one small step at a time -- a column loaded into a
// register, the register compared into another, that one tested -- and an
// emitter that took it literally kept every intermediate value in the
// register file: a store, then a load, between each step and the next.
// Fusion merges adjacent steps into one wherever the value passed between
// them goes nowhere else, so the merged step keeps it in a machine register
// and the file is never touched:
//
//   - a comparison and the test of its result become one compare-and-branch
//     (POpCmpSkip, POpCmpJump);
//   - a column load that feeds only such a comparison is folded into it
//     (BCol), so the column is read once, straight into the register the
//     comparison uses;
//   - a pure step whose result nothing reads is dropped.
//
// It is one mechanism for every backend: each step's CONTRACT -- the
// registers it reads and writes, where control can go after it, whether it
// does anything besides write its register -- is stated once (progStep), a
// liveness pass over the program's control flow says which values are still
// wanted where, and the rewrite is legal exactly when the contract and the
// liveness say so. Anything it cannot prove stays as it was, so a program
// the pass does not improve emits exactly as before. The passes repeat until
// one changes nothing.
//
// What the caller reads back is part of the contract: after the run, the
// accumulate steps' row counters (r[B] of POpAccSum, POpAccMin, POpAccMax);
// at a POpService, any register at all -- the service is Go code, and may
// read whatever it likes.

var fusionOff atomic.Bool

// SetFusion turns the fusion pass on (the default) or off for programs
// emitted from now on, so a test can compare the two.
func SetFusion(on bool) { fusionOff.Store(!on) }

// Fusing reports whether programs emitted now are fused: part of what a
// cache of emitted programs is keyed by.
func Fusing() bool { return !fusionOff.Load() }

// PAY FOR USE. The pass runs only on a program that has a pair it can merge,
// and the caller knows whether it has one at no cost: it builds the program a
// step at a time, so it compares each step with the one before as it appends
// it (Fusable), and hands the answer to EmitProgramFlagged. A program with no
// such pair -- a single comparison feeding an accumulate, a bare count --
// goes straight to the emitter, paying neither the liveness pass nor the
// copy.

// Fusable reports whether next, appended right after prev, makes a pair the
// fusion pass works on: a comparison and the test of its result, a column
// load and the comparison reading it, or a step and a copy of what it wrote.
func Fusable(prev, next ProgInsn) bool {
	switch prev.Op {
	case POpCmp:
		switch next.Op {
		case POpSkipIfZero:
			return next.A == prev.A
		case POpJumpIfZero, POpJumpIfNotZero:
			return next.B == prev.A
		}
	case POpLoadCol:
		if (next.Op == POpCmp || next.Op == POpCmpSkip || next.Op == POpCmpJump) &&
			!next.BCol && (next.B == prev.A || next.C == prev.A) {
			return true
		}
	}
	// A copy of the value just produced: the VDBE's OpSCopy after an
	// OpColumn, which the lowering keeps step for step -- often a copy
	// nothing reads, which the pass drops.
	if next.Op == POpLoadReg {
		for _, r := range stepOf(prev).defs {
			if r == next.B {
				return true
			}
		}
	}
	return false
}

// AnyFusable is the Fusable flag for a program built without one: every
// adjacent pair, checked once.
func AnyFusable(insns []ProgInsn) bool {
	for i := 1; i < len(insns); i++ {
		if Fusable(insns[i-1], insns[i]) {
			return true
		}
	}
	return false
}

// EmitProgram compiles insns into a native row loop: EmitProgramFlagged, with
// the flag found by looking at every adjacent pair.
func EmitProgram(insns []ProgInsn, nCols int) ([]byte, error) {
	return EmitProgramFlagged(insns, nCols, AnyFusable(insns))
}

// EmitProgramFlagged compiles insns into a native row loop, through the fusion
// pass when fusable says the program has a pair it merges (and fusion is on).
func EmitProgramFlagged(insns []ProgInsn, nCols int, fusable bool) ([]byte, error) {
	progsEmitted.Add(1)
	if (fusable || fuseAll.Load()) && !fusionOff.Load() {
		start := time.Now()
		n := len(insns)
		insns = Fuse(insns)
		fuseNanos.Add(int64(time.Since(start)))
		progsFused.Add(1)
		if !fusable && len(insns) != n {
			progsMissed.Add(1) // only under SetFuseAllForTest: the gate would have skipped a change
		}
	}
	return emitProgram(insns, nCols)
}

// fuseAll runs the pass on every program, flagged or not (SetFuseAllForTest).
var fuseAll atomic.Bool

// SetFuseAllForTest makes every program go through the fusion pass, as
// before the Fusable gate, so a test can measure what the gate saves.
func SetFuseAllForTest(on bool) { fuseAll.Store(on) }

// The pass's own accounting, for measuring what it costs and how often it
// runs (FusionStatsForTest).
var progsEmitted, progsFused, progsMissed, fuseNanos atomic.Int64

// FusionStatsForTest reads and clears how many programs were emitted, how
// many of them went through the fusion pass, the time it took, and -- under
// SetFuseAllForTest -- how many unflagged programs the pass still changed.
func FusionStatsForTest() (emitted, fused, missed int64, spent time.Duration) {
	return progsEmitted.Swap(0), progsFused.Swap(0), progsMissed.Swap(0), time.Duration(fuseNanos.Swap(0))
}

// progStep is one instruction's contract.
type progStep struct {
	uses, defs []int // registers read, registers written
	useAll     bool  // reads every register (a service)
	pure       bool  // nothing but its writes: dropping it when they are dead is safe
	fall       bool  // control can continue at the next instruction
	targets    []int // other places control can go (IR indices, ProgNextRow)
}

func stepOf(in ProgInsn) progStep {
	s := progStep{fall: true}
	switch in.Op {
	case POpLoadCol:
		s.defs, s.pure = []int{in.A}, true
	case POpLoadReg, POpNot:
		s.uses, s.defs, s.pure = []int{in.B}, []int{in.A}, true
	case POpCmp, POpAnd, POpOr:
		s.uses, s.defs, s.pure = []int{in.B, in.C}, []int{in.A}, true
	case POpSetConst:
		s.defs, s.pure = []int{in.A}, true
	case POpSkipIfZero:
		s.uses, s.targets = []int{in.A}, []int{ProgNextRow}
	case POpJump:
		s.fall, s.targets = false, []int{in.A}
	case POpJumpIfZero, POpJumpIfNotZero:
		s.uses, s.targets = []int{in.B}, []int{in.A}
	case POpAccCount, POpEmitRow:
	case POpAccSum, POpAccMin, POpAccMax:
		s.uses, s.defs = []int{in.A, in.B}, []int{in.B}
	case POpAdd, POpSub, POpMul, POpRem:
		// Not pure: each can flag an overflow the caller declines on.
		s.uses, s.defs = []int{in.B, in.C}, []int{in.A}
	case POpAbs:
		s.uses, s.defs = []int{in.B}, []int{in.A}
	case POpService:
		s.useAll = true
	case POpTextLen, POpTextMatch:
		s.defs, s.targets = []int{in.A}, []int{in.C}
	case POpCmpSkip:
		s.uses, s.targets = []int{in.C}, []int{ProgNextRow}
		if !in.BCol {
			s.uses = append(s.uses, in.B)
		}
	case POpCmpJump:
		s.uses, s.targets = []int{in.C}, []int{in.A}
		if !in.BCol {
			s.uses = append(s.uses, in.B)
		}
	default:
		// An opcode this pass does not know: reads and writes anything, so
		// nothing around it is touched.
		s.useAll = true
	}
	return s
}

// regSet is a set of register numbers.
type regSet []uint64

func newRegSet(n int) regSet    { return make(regSet, (n+63)/64) }
func (s regSet) has(r int) bool { return r >= 0 && r/64 < len(s) && s[r/64]&(1<<(r%64)) != 0 }
func (s regSet) add(r int)      { s[r/64] |= 1 << (r % 64) }
func (s regSet) del(r int)      { s[r/64] &^= 1 << (r % 64) }
func (s regSet) fill() {
	for i := range s {
		s[i] = ^uint64(0)
	}
}
func (s regSet) union(o regSet) bool {
	changed := false
	for i := range s {
		if n := s[i] | o[i]; n != s[i] {
			s[i], changed = n, true
		}
	}
	return changed
}

// progRegs is one past the highest register the program names.
func progRegs(insns []ProgInsn) int {
	n := 0
	for _, in := range insns {
		st := stepOf(in)
		for _, r := range append(st.uses, st.defs...) {
			n = max(n, r+1)
		}
	}
	return n
}

// progLiveness returns, per instruction, the registers live after it.
func progLiveness(insns []ProgInsn, nRegs int) []regSet {
	n := len(insns)
	steps := make([]progStep, n)
	for i, in := range insns {
		steps[i] = stepOf(in)
	}
	liveIn := make([]regSet, n+1) // [n] is "next row"
	liveOut := make([]regSet, n)
	for i := range liveIn {
		liveIn[i] = newRegSet(nRegs)
	}
	for i := range liveOut {
		liveOut[i] = newRegSet(nRegs)
	}
	// After the last row the caller reads the accumulators' row counters.
	exit := newRegSet(nRegs)
	for _, in := range insns {
		if in.Op == POpAccSum || in.Op == POpAccMin || in.Op == POpAccMax {
			exit.add(in.B)
		}
	}
	at := func(t int) regSet {
		if t == ProgNextRow || t >= n || t < 0 {
			return liveIn[n]
		}
		return liveIn[t]
	}
	for changed := true; changed; {
		changed = false
		// "Next row" leads to the top of the loop, and out of it.
		if n > 0 && liveIn[n].union(liveIn[0]) {
			changed = true
		}
		if liveIn[n].union(exit) {
			changed = true
		}
		for i := n - 1; i >= 0; i-- {
			st := steps[i]
			out := newRegSet(nRegs)
			if st.fall {
				out.union(at(i + 1))
			}
			for _, t := range st.targets {
				out.union(at(t))
			}
			liveOut[i].union(out)
			in := newRegSet(nRegs)
			copy(in, liveOut[i])
			for _, r := range st.defs {
				in.del(r)
			}
			for _, r := range st.uses {
				in.add(r)
			}
			if st.useAll {
				in.fill()
			}
			if liveIn[i].union(in) {
				changed = true
			}
		}
	}
	return liveOut
}

// progJumpTargets marks every instruction some instruction can jump to,
// or a service resumes at.
func progJumpTargets(insns []ProgInsn) []bool {
	tgt := make([]bool, len(insns)+1)
	for i, in := range insns {
		for _, t := range stepOf(in).targets {
			if t >= 0 && t < len(insns) {
				tgt[t] = true
			}
		}
		if in.Op == POpService && i+1 <= len(insns) {
			tgt[i+1] = true
		}
	}
	return tgt
}

// reverseCond is c with its operands swapped: a < b is b > a.
func reverseCond(c Cond) Cond {
	switch c {
	case CondL:
		return CondG
	case CondG:
		return CondL
	case CondLE:
		return CondGE
	case CondGE:
		return CondLE
	}
	return c
}

// Fuse is the fusion pass: insns rewritten as described at the top of this
// file, jump targets renumbered to match. The result computes exactly what
// insns computes, as the caller observes it.
func Fuse(insns []ProgInsn) []ProgInsn {
	out := append([]ProgInsn(nil), insns...)
	for {
		next, changed := fuseOnce(out)
		out = next
		// Again only if this pass left a pair to merge: a comparison it fused
		// whose column load it could not fold in the same pass, say. A pass
		// that merged everything it found is the last.
		if !changed || !AnyFusable(out) {
			fusedSteps.Add(int64(len(insns) - len(out)))
			return out
		}
	}
}

// fusedSteps counts the steps fusion has removed, for a test to see it ran.
var fusedSteps atomic.Int64

// FusedStepsForTest reads and clears the count of steps fusion removed.
func FusedStepsForTest() int64 { return fusedSteps.Swap(0) }

// fuseOnce applies every rewrite liveness allows, once.
func fuseOnce(insns []ProgInsn) ([]ProgInsn, bool) {
	n := len(insns)
	nRegs := progRegs(insns)
	if nRegs == 0 || n == 0 {
		return insns, false
	}
	liveOut := progLiveness(insns, nRegs)
	tgt := progJumpTargets(insns)
	keep := make([]bool, n)
	for i := range keep {
		keep[i] = true
	}
	work := append([]ProgInsn(nil), insns...)
	changed := false
	dead := func(i, r int) bool { return !liveOut[i].has(r) }

	for i := 0; i < n; i++ {
		in := work[i]
		// A comparison tested right after, its result read nowhere else.
		if in.Op == POpCmp && i+1 < n && !tgt[i+1] {
			j := work[i+1]
			var fused ProgInsn
			ok := false
			switch {
			case j.Op == POpSkipIfZero && j.A == in.A && dead(i+1, in.A):
				fused, ok = ProgInsn{Op: POpCmpSkip, B: in.B, C: in.C, Cond: in.Cond}, true
			case j.Op == POpJumpIfNotZero && j.B == in.A && dead(i+1, in.A):
				fused, ok = ProgInsn{Op: POpCmpJump, A: j.A, B: in.B, C: in.C, Cond: in.Cond}, true
			case j.Op == POpJumpIfZero && j.B == in.A && dead(i+1, in.A):
				fused, ok = ProgInsn{Op: POpCmpJump, A: j.A, B: in.B, C: in.C, Cond: in.Cond.Negate()}, true
			}
			if ok {
				// The column load right before, feeding only this comparison,
				// folds in now rather than on another pass: it is dead after
				// the test (its liveness is unchanged by the merge), and the
				// comparison is no jump target.
				if i > 0 && keep[i-1] && !tgt[i] && work[i-1].Op == POpLoadCol && dead(i+1, work[i-1].A) {
					ld := work[i-1]
					switch {
					case fused.B == ld.A && fused.C != ld.A:
						fused.B, fused.BCol = ld.B, true
						keep[i-1] = false
					case fused.C == ld.A && fused.B != ld.A:
						fused.B, fused.C, fused.BCol, fused.Cond = ld.B, fused.B, true, reverseCond(fused.Cond)
						keep[i-1] = false
					}
				}
				work[i], keep[i+1], changed = fused, false, true
				i++
				continue
			}
		}
		// A column load feeding only the compare-and-branch right after it.
		if in.Op == POpLoadCol && i+1 < n && !tgt[i+1] {
			j := work[i+1]
			if (j.Op == POpCmpSkip || j.Op == POpCmpJump) && !j.BCol && dead(i+1, in.A) {
				switch {
				case j.B == in.A && j.C != in.A:
					j.B, j.BCol = in.B, true
				case j.C == in.A && j.B != in.A:
					j.B, j.C, j.BCol, j.Cond = in.B, j.B, true, reverseCond(j.Cond)
				default:
					goto noLoad
				}
				work[i+1], keep[i], changed = j, false, true
				continue
			}
		}
	noLoad:
		// A pure step whose results nothing reads.
		if st := stepOf(in); st.pure && !st.useAll && len(st.defs) > 0 {
			all := true
			for _, r := range st.defs {
				if !dead(i, r) {
					all = false
				}
			}
			if all {
				keep[i], changed = false, true
			}
		}
	}
	if !changed {
		return insns, false
	}
	// Renumber: an index maps to the first kept instruction at or after it.
	newIdx := make([]int, n+1)
	k := 0
	for i := 0; i < n; i++ {
		newIdx[i] = k
		if keep[i] {
			k++
		}
	}
	newIdx[n] = k
	remap := func(t int) int {
		if t == ProgNextRow || t < 0 {
			return t
		}
		if t >= n {
			return k
		}
		return newIdx[t]
	}
	out := make([]ProgInsn, 0, k)
	for i, in := range work {
		if !keep[i] {
			continue
		}
		switch in.Op {
		case POpJump, POpJumpIfZero, POpJumpIfNotZero, POpCmpJump:
			in.A = remap(in.A)
		case POpTextLen, POpTextMatch:
			in.C = remap(in.C)
		}
		out = append(out, in)
	}
	return out, true
}
