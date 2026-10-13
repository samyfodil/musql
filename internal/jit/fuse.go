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
	case POpCmp, POpAnd, POpOr, POpNot:
		// A boolean feeding AND/OR/NOT: the start of a condition fuseConds
		// turns into branches.
		switch next.Op {
		case POpAnd, POpOr:
			if next.B == prev.A || next.C == prev.A {
				return true
			}
		case POpNot:
			if next.B == prev.A {
				return true
			}
		}
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
	progCount.Add(1)
	progIRIn.Add(int64(len(insns)))
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
	progIROut.Add(int64(len(insns)))
	code, err := emitProgram(insns, nCols)
	progCodeBytes.Add(int64(len(code)))
	return code, err
}

// fuseAll runs the pass on every program, flagged or not (SetFuseAllForTest).
var fuseAll atomic.Bool

// SetFuseAllForTest makes every program go through the fusion pass, as
// before the Fusable gate, so a test can measure what the gate saves.
func SetFuseAllForTest(on bool) { fuseAll.Store(on) }

// The pass's own accounting, for measuring what it costs and how often it
// runs (FusionStatsForTest).
var progsEmitted, progsFused, progsMissed, fuseNanos atomic.Int64

// What the emitted programs look like (ProgramStatsForTest): IR steps in and
// out of the fusion pass, bytes of machine code, and the lengths of the runs
// fused -- how many IR steps each merge took into one.
var progCount, progIRIn, progIROut, progCodeBytes atomic.Int64
var progRuns [maxRunBucket + 1]atomic.Int64

const maxRunBucket = 16

func noteRun(n int) { progRuns[min(n, maxRunBucket)].Add(1) }

// ProgramStats is ProgramStatsForTest's answer.
type ProgramStats struct {
	Programs, IRIn, IROut, CodeBytes int64
	Allocated, Spilled, Coalesced    int64
	Runs                             [maxRunBucket + 1]int64 // [n]: runs of n steps fused into one; [16]: 16 or more
}

// ProgramStatsForTest reads and clears the counts behind ProgramStats.
func ProgramStatsForTest() ProgramStats {
	st := ProgramStats{
		Programs: progCount.Swap(0), IRIn: progIRIn.Swap(0), IROut: progIROut.Swap(0), CodeBytes: progCodeBytes.Swap(0),
	}
	st.Allocated, st.Spilled, st.Coalesced = RegAllocStatsForTest()
	for i := range progRuns {
		st.Runs[i] = progRuns[i].Swap(0)
	}
	return st
}

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
	out, _ := progLivenessInOut(insns, nRegs)
	return out
}

// progLivenessInOut is progLiveness with the registers live BEFORE each
// instruction too; [len(insns)] is the next row's (and the exit's).
func progLivenessInOut(insns []ProgInsn, nRegs int) (liveOut, liveIn []regSet) {
	n := len(insns)
	steps := make([]progStep, n)
	for i, in := range insns {
		steps[i] = stepOf(in)
	}
	liveIn = make([]regSet, n+1) // [n] is "next row"
	liveOut = make([]regSet, n)
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
	return liveOut, liveIn
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
		next, changedConds := fuseConds(out)
		next, changed := fuseOnce(next)
		changed = changed || changedConds
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
				run := 2
				if fused.BCol {
					run = 3 // the load folded in
				}
				noteRun(run)
				work[i], keep[i+1], changed = fused, false, true
				i++
				continue
			}
		}
		// A column load feeding only one compare-and-branch further on, in the
		// same straight run: the comparison reads the column itself, at the
		// same row, and the load goes. Nothing between may be a jump target,
		// redefine the register, or read it.
		if in.Op == POpLoadCol {
			for p := i + 1; p < n && !tgt[p]; p++ {
				j := work[p]
				if !keep[p] {
					continue
				}
				st := stepOf(j)
				if (j.Op == POpCmpSkip || j.Op == POpCmpJump) && !j.BCol && dead(p, in.A) {
					switch {
					case j.B == in.A && j.C != in.A:
						j.B, j.BCol = in.B, true
					case j.C == in.A && j.B != in.A:
						j.B, j.C, j.BCol, j.Cond = in.B, j.B, true, reverseCond(j.Cond)
					default:
						break
					}
					if j.BCol {
						work[p], keep[i], changed = j, false, true
					}
					break
				}
				if st.useAll || !st.fall || len(st.targets) > 0 || slicesContains(st.uses, in.A) || slicesContains(st.defs, in.A) {
					break
				}
			}
			if !keep[i] {
				continue
			}
		}
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
	return progCompact(work, keep), true
}

// progCompact is work without the steps keep drops, every jump target
// renumbered: an index maps to the first kept step at or after it.
func progCompact(work []ProgInsn, keep []bool) []ProgInsn {
	n := len(work)
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
	return out
}

func slicesContains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ---- conditions as branches ---------------------------------------------------

// fuseConds rewrites each boolean expression a test reads -- comparisons
// joined by AND, OR and NOT, each result read once, all of it computed in the
// straight run right before the test -- into compare-and-branches, with no
// boolean ever materialized: an AND of n comparisons becomes n branches to
// the row's end, an OR jumps past the rest on its first true term. The
// loads and constants interleaved with the comparisons (pure, reading
// nothing the expression computes) are kept, moved to the run's top, where
// they ran unconditionally all the same.
func fuseConds(insns []ProgInsn) ([]ProgInsn, bool) {
	n := len(insns)
	nRegs := progRegs(insns)
	if n == 0 || nRegs == 0 {
		return insns, false
	}
	uses := make([]int, nRegs)
	for _, in := range insns {
		st := stepOf(in)
		if st.useAll {
			return insns, false // a service reads any register
		}
		for _, r := range st.uses {
			uses[r]++
		}
	}
	liveOut := progLiveness(insns, nRegs)
	tgt := progJumpTargets(insns)
	work := append([]ProgInsn(nil), insns...)
	keep := make([]bool, n)
	for i := range keep {
		keep[i] = true
	}
	changed := false
	isNode := func(op POp) bool { return op == POpCmp || op == POpAnd || op == POpOr || op == POpNot }
	isHoistable := func(op POp) bool { return op == POpLoadCol || op == POpLoadReg || op == POpSetConst }
	for j := 0; j < n; j++ {
		test := work[j]
		var root int
		switch test.Op {
		case POpSkipIfZero:
			root = test.A
		case POpJumpIfZero, POpJumpIfNotZero:
			root = test.B
			if test.A < 0 {
				continue
			}
		default:
			continue
		}
		// Walk back over the run feeding the test. A register may hold one
		// node's value and then its parent's (the lowering reuses them), so
		// nodes are steps, and an operand's node is the nearest one before.
		need := map[int]bool{root: true}
		nodeRegs := map[int]bool{} // registers some node writes
		var nodes, hoist []int
		k := j
		for len(need) > 0 && k > 0 && keep[k-1] && !tgt[k] {
			p := k - 1
			in := work[p]
			switch {
			case isNode(in.Op) && need[in.A]:
				delete(need, in.A)
				nodeRegs[in.A] = true
				nodes = append(nodes, p)
				if in.Op != POpCmp {
					if in.Op != POpNot && in.B == in.C {
						k = -1
						break
					}
					need[in.B] = true
					if in.Op != POpNot {
						need[in.C] = true
					}
				}
			case isHoistable(in.Op) && !need[in.A]:
				hoist = append(hoist, p)
			default:
				k = -1
			}
			if k < 0 {
				break
			}
			k = p
		}
		if k < 0 || len(need) > 0 || len(nodes) < 2 {
			continue // not a whole expression, or a single comparison (fuseOnce's)
		}
		// Every value a node computes is read once, by its parent or the
		// test, and is dead once read; a comparison reads no node's value.
		ok := true
		for _, q := range nodes {
			in := work[q]
			d, readers, redef := in.A, 0, false
			for s := q + 1; s <= j && !redef; s++ {
				st := stepOf(work[s])
				if slicesContains(st.uses, d) {
					readers++
				}
				redef = slicesContains(st.defs, d)
			}
			if readers != 1 || (!redef && liveOut[j].has(d)) {
				ok = false
			}
			if in.Op == POpCmp && (nodeRegs[in.B] || nodeRegs[in.C]) {
				ok = false
			}
		}
		childOf := func(p, r int) int {
			for q := p - 1; q >= k; q-- {
				if slicesContains(nodes, q) && work[q].A == r {
					return q
				}
			}
			return -1
		}
		// A column load whose value one comparison alone reads is folded
		// into it: the comparison reads the column, the load goes. Loads
		// reusing one register for each term are the common case, and could
		// not be hoisted above the terms otherwise.
		folded := map[int]bool{}      // load steps that go
		leafCol := map[int]ProgInsn{} // a comparison step -> its folded form
		for _, p := range hoist {
			in := work[p]
			if in.Op != POpLoadCol {
				continue
			}
			d, reader, readers, redef := in.A, -1, 0, false
			for q := p + 1; q <= j && !redef; q++ {
				st := stepOf(work[q])
				if slicesContains(st.uses, d) {
					readers++
					reader = q
				}
				redef = slicesContains(st.defs, d)
			}
			if readers != 1 || (!redef && liveOut[j].has(d)) || work[reader].Op != POpCmp {
				continue
			}
			if _, done := leafCol[reader]; done {
				continue
			}
			leaf := work[reader]
			switch {
			case leaf.B == d && leaf.C != d:
				leaf.B, leaf.BCol = in.B, true
			case leaf.C == d && leaf.B != d:
				leaf.B, leaf.C, leaf.BCol, leaf.Cond = in.B, leaf.B, true, reverseCond(leaf.Cond)
			default:
				continue
			}
			leafCol[reader], folded[p] = leaf, true
		}
		// A hoisted step moves above the comparisons before it: none of them
		// may read what it writes.
		for _, p := range hoist {
			if folded[p] {
				continue
			}
			st := stepOf(work[p])
			for _, r := range append(st.uses, st.defs...) {
				if nodeRegs[r] {
					ok = false
				}
			}
			for _, q := range nodes {
				if q < p {
					in := work[q]
					for _, d := range st.defs {
						if in.Op == POpCmp && (in.B == d || in.C == d) {
							ok = false
						}
					}
				}
			}
		}
		if !ok {
			continue
		}
		// Emit into the run's own slots: the hoisted steps in their order,
		// then one branch per comparison.
		var out []ProgInsn
		for p := k; p < j; p++ {
			if slicesContains(hoist, p) && !folded[p] {
				out = append(out, work[p])
			}
		}
		base := k // the slot of out[0]; out[i] is old index base+i
		const fall = -2
		var placeholders []int // indices into out whose A awaits a "here" target
		var gen func(q, onTrue, onFalse int)
		here := func() int { return base + len(out) }
		gen = func(q, onTrue, onFalse int) {
			in := work[q]
			if f, ok := leafCol[q]; ok {
				in = f
			}
			switch in.Op {
			case POpCmp:
				leaf := ProgInsn{B: in.B, BCol: in.BCol, C: in.C, Cond: in.Cond}
				switch {
				case onFalse == fall:
					leaf.Op, leaf.A = POpCmpJump, onTrue
				case onFalse == ProgNextRow:
					leaf.Op = POpCmpSkip
				default:
					leaf.Op, leaf.A, leaf.Cond = POpCmpJump, onFalse, in.Cond.Negate()
				}
				out = append(out, leaf)
			case POpNot:
				gen(childOf(q, in.B), onFalse, onTrue)
			case POpAnd:
				if onTrue == fall {
					gen(childOf(q, in.B), fall, onFalse)
					gen(childOf(q, in.C), fall, onFalse)
				} else {
					mark := len(placeholders)
					placeholders = append(placeholders, -1)
					gen(childOf(q, in.B), fall, -3-mark)
					gen(childOf(q, in.C), onTrue, fall)
					placeholders[mark] = here()
				}
			case POpOr:
				if onFalse == fall {
					gen(childOf(q, in.B), onTrue, fall)
					gen(childOf(q, in.C), onTrue, fall)
				} else {
					mark := len(placeholders)
					placeholders = append(placeholders, -1)
					gen(childOf(q, in.B), -3-mark, fall)
					gen(childOf(q, in.C), fall, onFalse)
					placeholders[mark] = here()
				}
			}
		}
		top := childOf(j, root)
		switch test.Op {
		case POpSkipIfZero:
			gen(top, fall, ProgNextRow)
		case POpJumpIfZero:
			gen(top, fall, test.A)
		case POpJumpIfNotZero:
			gen(top, test.A, fall)
		}
		for i := range out {
			if out[i].Op == POpCmpJump && out[i].A <= -3 {
				out[i].A = placeholders[-3-out[i].A]
			}
		}
		if len(out) > j-k+1 {
			continue // cannot happen: each comparison is one branch
		}
		for i := k; i <= j; i++ {
			keep[i] = false
		}
		for i, in := range out {
			work[k+i], keep[k+i] = in, true
		}
		noteRun(j - k + 1)
		changed = true
	}
	if !changed {
		return insns, false
	}
	return progCompact(work, keep), true
}

// ---- predicated rows ---------------------------------------------------------

// progPredicated reports whether a fused program's row ends in a run of
// filters (POpCmpSkip, up to maxPredFilters) followed only by work that can
// run for every row and count only for the rows that pass -- column loads,
// POpAccSum, POpAccCount -- and returns where the filters start. The emitter
// then computes the filters' conjunction without a branch and applies it to
// the accumulators (a row that fails adds 0 and counts 0), instead of
// branching to the next row on each filter.
//
// Branches a filter decides by the data are the ones a predictor cannot
// learn: on random data each one is a misprediction a few rows in ten, which
// is most of the kernel's time and made it vary 2x with where the code
// happened to land. The predicated row costs the same whatever the data.
//
// A load in the tail runs for rows it used not to, so the register it writes
// must not be one a row reads before writing it (carried from the last row)
// or the caller reads after the loop. Nothing may jump into the run.
// Unfused emission (SetFusion(false)) never predicates: it is the reference.
func progPredicated(insns []ProgInsn) (int, bool) {
	if !Fusing() {
		return 0, false
	}
	n := len(insns)
	t := n - 1
	acc := false
	for ; t >= 0; t-- {
		op := insns[t].Op
		if op == POpAccSum || op == POpAccCount {
			acc = true
		} else if op != POpLoadCol {
			break
		}
	}
	if !acc || t < 0 || insns[t].Op != POpCmpSkip {
		return 0, false
	}
	s := t
	for s > 0 && insns[s-1].Op == POpCmpSkip && t-s+1 < maxPredFilters {
		s--
	}
	tgt := progJumpTargets(insns)
	for i := s + 1; i < n; i++ {
		if tgt[i] {
			return 0, false
		}
	}
	nRegs := progRegs(insns)
	if nRegs > 0 {
		_, liveIn := progLivenessInOut(insns, nRegs)
		for _, in := range insns[t+1:] {
			if in.Op == POpLoadCol && (liveIn[0].has(in.A) || liveIn[n].has(in.A)) {
				return 0, false
			}
		}
	}
	progPredicatedRows.Add(1)
	return s, true
}

// maxPredFilters bounds the filters a predicated row computes for every row.
const maxPredFilters = 6

// progPredicatedRows counts the programs emitted with a predicated row.
var progPredicatedRows atomic.Int64

// PredicatedStatsForTest reads and clears how many programs were emitted
// with a predicated row.
func PredicatedStatsForTest() int64 { return progPredicatedRows.Swap(0) }
