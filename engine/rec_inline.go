package engine

import "slices"

// A recursive CTE's step -- "SELECT i+1 FROM c WHERE i < N" -- ran as its own
// sub-program for every row the queue pops: a machine from the pool, its
// registers and cursors set up, the self cursor opened, the result row copied
// out into a fresh slice, the machine scrubbed and returned. For a 100k-row
// recursive INSERT that was most of the work. C SQLite codes the step inside
// the queue's own program (generateWithRecursiveQuery, select.c), pushing each
// result straight onto the queue.
//
// recInlinePeephole does the same to a compiled queue program, as
// existsInlinePeephole does to an EXISTS: each "RecQueueFill 1 arm" whose arm
// it can relocate becomes a Gosub into the arm's instructions, appended to the
// queue program with registers and cursors renumbered past its own, each
// ResultRow becoming a RecQueuePush, followed by RecQueueCheck for the caps
// RecQueueFill applies after a step. The arm reads the current row through its
// self cursor exactly as before: the queue machine is the one that popped it.
// Anything with an opcode outside recInlineOperands keeps its sub-program.

// recInlinePeephole inlines every recursive step of prog it can.
func recInlinePeephole(prog *Program) {
	if prog == nil {
		return
	}
	for i := range prog.Insns {
		op := prog.Insns[i]
		if op.Op != OpRecQueueFill || op.P1 != 1 {
			continue
		}
		arm, ok := op.P4.(*Program)
		if !ok || !recInlinable(arm) {
			continue
		}
		recInline(prog, i, arm)
	}
}

// recInlinable reports whether arm is a step this pass can relocate. Its
// slots move past the queue program's own: the self reference's, which
// openRecursiveSelf serves without the cache, and those of the derived
// sources that read the same rows on every run (derivedReadsSameRows), which
// the queue program's slots then keep across steps.
func recInlinable(arm *Program) bool {
	if arm == nil || arm.Compound != nil || arm.LiveSource != nil || arm.NSorters != 0 || arm.NDistinct != 0 ||
		arm.NRecRegs != 0 || len(arm.Insns) == 0 || arm.Insns[0].Op != OpInit {
		return false
	}
	for _, in := range arm.Insns {
		if !inlineOperands(in) {
			return false
		}
		switch in.Op {
		case OpOuterColumn, OpOuterRowid:
			return false
		case OpOpenDerived:
			ds, ok := in.P4.(*derivedSource)
			if !ok || ds == nil {
				return false
			}
			if ds.recSelf == nil && !derivedReadsSameRows(ds) ||
				ds.recSelf != nil && (ds.catalogScope != scopeAny || ds.vtabWrite != nil || ds.dbIdx != 0) {
				return false
			}
		case OpFunction:
			if _, isName := in.P4.(string); !isName {
				return false
			}
		}
	}
	return true
}

// recInline replaces prog.Insns[at], "RecQueueFill 1 arm", with a jump into a
// relocated copy of arm appended to prog, which runs RecQueueCheck for the caps
// RecQueueFill applies after a step and jumps back: no address in prog moves.
// Each fill gets its own copy, so its way back is a Goto, threaded through the
// Goto that follows the fill.
//
// The copy is leaner than the arm. Its Init and a closing Halt go: the jump
// enters where Init jumped, and the Halt was the way to the check. When the
// self cursor is read only by Column (recSelfCursor), its one-row loop is
// straight-line code: the open, Rewind, Next, IfEmpty and Close on it go too,
// and each Column becomes a RecSelfColumn of the popped row, as
// openRecursiveSelf's cursor would answer. recArmSimplify drops what the step
// computes once per row for nothing.
func recInline(prog *Program, at int, arm *Program) {
	regOff, curOff, slotOff := prog.NReg, prog.NCursors, prog.NSubCache
	reg := func(r int) int { return r + regOff }

	self, spec := recSelfCursor(arm)
	if spec == nil {
		self = -1
	}
	ins, drop, hoist := recArmSimplify(arm, self, prog.Insns[0].Op == OpRecQueueOpen || prog.Insns[0].Op == OpGoto)
	last := len(ins) - 1
	dropped := func(a int) bool {
		in := ins[a]
		switch {
		case drop[a]:
			return true
		case in.Op == OpInit:
			return true
		case in.Op == OpHalt:
			return a == last
		case in.Op == OpOpenDerived, in.Op == OpRewind, in.Op == OpNext, in.Op == OpIfEmpty, in.Op == OpClose:
			return spec != nil && in.P1 == self
		}
		return false
	}
	// newAddr[a] is where arm address a lands: a dropped one on the next kept
	// instruction, len(ins) on the check, and Init on where it jumped.
	newAddr := make([]int, len(ins)+1)
	pc := len(prog.Insns)
	for a := range ins {
		newAddr[a] = pc
		if !dropped(a) {
			pc++
		}
	}
	done := pc
	newAddr[len(ins)] = done
	newAddr[0] = newAddr[ins[0].P2]
	back := at + 1
	if back < len(prog.Insns) && prog.Insns[back].Op == OpGoto {
		back = prog.Insns[back].P2
	}

	x := inlineReloc{regOff: regOff, curOff: curOff, slotOff: slotOff, jump: func(a int) int { return newAddr[a] }}
	relocate := func(in Instruction) Instruction {
		switch {
		case in.Op == OpHalt:
			return Instruction{Op: OpGoto, P2: done}
		case in.Op == OpResultRow:
			return Instruction{Op: OpRecQueuePush, P1: reg(in.P1), P2: in.P2}
		case in.Op == OpColumn && spec != nil && in.P1 == self:
			return Instruction{Op: OpRecSelfColumn, P2: in.P2, P3: reg(in.P3), P4: spec}
		}
		return x.relocate(in)
	}

	out := prog.Insns
	for a, in := range ins {
		if !dropped(a) {
			out = append(out, relocate(in))
		}
	}
	out = append(out,
		Instruction{Op: OpRecQueueCheck},
		Instruction{Op: OpGoto, P2: back})
	out[at] = Instruction{Op: OpGoto, P2: newAddr[0]}
	// The constants load once, ahead of the program's first instruction, which
	// moves to after them and goes on to where it fell through to.
	if len(hoist) > 0 {
		first := out[0]
		out[0] = Instruction{Op: OpGoto, P2: len(out)}
		for _, in := range hoist {
			out = append(out, relocate(in))
		}
		out = append(out, first)
		if first.Op != OpGoto {
			out = append(out, Instruction{Op: OpGoto, P2: 1})
		}
	}
	prog.Insns = out
	prog.NReg += arm.NReg
	prog.NCursors += arm.NCursors
	prog.NSubCache += arm.NSubCache
}

// recSelfCursor is arm's self cursor and the CTE it reads, when the arm opens
// it once and reads it only by Column; otherwise -1 and nil, and the cursor
// stays. A sorted seek into it is a read Column alone cannot replace.
func recSelfCursor(arm *Program) (int, *recQueueSpec) {
	self, spec := -1, (*recQueueSpec)(nil)
	for _, in := range arm.Insns {
		if in.Op != OpOpenDerived {
			continue
		}
		if ds := in.P4.(*derivedSource); ds.recSelf != nil {
			if spec != nil {
				return -1, nil
			}
			self, spec = in.P1, ds.recSelf
		}
	}
	for _, in := range arm.Insns {
		if in.P1 != self {
			continue
		}
		switch in.Op {
		case OpSortedSeek:
			return -1, nil
		case OpOpenDerived:
			if in.P4.(*derivedSource).recSelf == nil {
				return -1, nil
			}
		}
	}
	return self, spec
}

// recArmSimplify is arm's instructions with three per-row costs taken out,
// each only where every read and write of the registers involved is known
// (recArmOperands): drop[a] marks an instruction that goes, and hoist holds
// the constant loads that run once instead, when canHoist.
//
//   - A comparison storing its result for the If or IfNot after it, the only
//     reader, jumps instead: the same comparison for If, its inverse for IfNot,
//     with a NULL operand jumping iff the If's P3 says so.
//   - Arithmetic whose result only an SCopy after it reads writes the copy's
//     register itself.
//   - A second read of a self column (self, its cursor, -1 for none), with
//     nothing jumping in between the two, is the first: the row does not change
//     within a step, so its readers read the first read's register.
//   - An integer or real constant loaded into a register nothing else writes,
//     and read only after it, is loaded once. Nothing changes it in between: a comparison applies
//     affinity to copies, and a function reads its arguments through one.
//
// None of the merged pairs may have its second half as a jump target.
func recArmSimplify(arm *Program, self int, canHoist bool) (ins []Instruction, drop []bool, hoist []Instruction) {
	ins = slices.Clone(arm.Insns)
	drop = make([]bool, len(ins))
	reads, writes := map[int]int{}, map[int]int{}
	readAt := map[int][]int{}
	target := map[int]bool{}
	var jumps [][2]int
	for a, in := range ins {
		rs, w, ok := recArmOperands(in)
		if !ok {
			return ins, drop, nil
		}
		for _, r := range rs {
			reads[r]++
			readAt[r] = append(readAt[r], a)
		}
		if w >= 0 {
			writes[w]++
		}
		if t, isJump := recArmJump(in); isJump {
			target[t] = true
			jumps = append(jumps, [2]int{a, t})
		}
	}
	// covers reports whether every read of r comes after address a and is
	// reached only through it: a register read before it is written, as
	// compileFunc's once-only flag is, keeps its write where it is.
	covers := func(a, r int) bool {
		last := a
		for _, at := range readAt[r] {
			if at <= a {
				return false
			}
			last = max(last, at)
		}
		for _, j := range jumps {
			if j[1] > a && j[1] <= last && (j[0] < a || j[0] > last) {
				return false
			}
		}
		return true
	}
	for a := 0; a+1 < len(ins); a++ {
		in, next := ins[a], ins[a+1]
		if target[a+1] || drop[a] {
			continue
		}
		switch {
		case recInverseCompare(in.Op) != 0 && in.P5&p5StoreP2 != 0 && (next.Op == OpIf || next.Op == OpIfNot) &&
			next.P1 == in.P2 && reads[in.P2] == 1 && writes[in.P2] == 1:
			op := in.Op
			if next.Op == OpIfNot {
				op = recInverseCompare(op)
			}
			p5 := in.P5 &^ (p5StoreP2 | p5JumpIfNull)
			if next.P3 != 0 {
				p5 |= p5JumpIfNull
			}
			ins[a] = Instruction{Op: op, P1: in.P1, P2: next.P2, P3: in.P3, P4: in.P4, P5: p5}
			drop[a+1] = true
		case recArith(in.Op) && next.Op == OpSCopy && next.P1 == in.P3 &&
			reads[in.P3] == 1 && writes[in.P3] == 1 && next.P2 != in.P1 && next.P2 != in.P2:
			ins[a].P3 = next.P2
			drop[a+1] = true
		}
	}
	if self >= 0 {
		first := map[int]int{}
		for a, in := range ins {
			if in.Op != OpColumn || in.P1 != self {
				continue
			}
			f, seen := first[in.P2]
			if !seen {
				first[in.P2] = a
				continue
			}
			from, to := in.P3, ins[f].P3
			jumpsIn := false
			for t := range target {
				jumpsIn = jumpsIn || f < t && t <= a
			}
			if writes[from] != 1 || writes[to] != 1 || jumpsIn || !covers(a, from) || !recRenameReads(ins, from, to, true) {
				continue
			}
			recRenameReads(ins, from, to, false)
			drop[a] = true
		}
	}
	if !canHoist {
		return ins, drop, nil
	}
	for a, in := range ins {
		switch in.Op {
		case OpInteger, OpInt64, OpReal:
			if !drop[a] && writes[in.P2] == 1 && covers(a, in.P2) {
				hoist = append(hoist, in)
				drop[a] = true
			}
		}
	}
	return ins, drop, hoist
}

// recArmOperands is the registers an arm instruction reads and the one it
// writes (-1 for none), for every opcode recInlineOperands admits; ok is false
// for one whose registers it cannot list.
func recArmOperands(in Instruction) (reads []int, w int, ok bool) {
	span := func(from, n int) []int {
		rs := make([]int, n)
		for i := range rs {
			rs[i] = from + i
		}
		return rs
	}
	switch in.Op {
	case OpInit, OpGoto, OpHalt, OpOpenDerived, OpRewind, OpNext, OpIfEmpty, OpClose:
		return nil, -1, true
	case OpColumn:
		return nil, in.P3, true
	case OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable:
		return nil, in.P2, true
	case OpSCopy, OpCopy, OpNot, OpBitNot, OpNegative:
		return []int{in.P1}, in.P2, true
	case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		return []int{in.P1, in.P2}, in.P3, true
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		if in.P5&p5StoreP2 != 0 {
			return []int{in.P1, in.P3}, in.P2, true
		}
		return []int{in.P1, in.P3}, -1, true
	case OpIf, OpIfNot, OpIsNull, OpNotNull, OpReturn:
		return []int{in.P1}, -1, true
	case OpFunction:
		return span(in.P1, in.P2), in.P3, true
	case OpCast:
		return []int{in.P1}, in.P1, true
	case OpResultRow:
		return span(in.P1, in.P2), -1, true
	case OpGosub:
		return nil, in.P1, true
	}
	return nil, -1, false
}

// recArmJump is the address an arm instruction may jump to.
func recArmJump(in Instruction) (int, bool) {
	switch in.Op {
	case OpInit, OpGoto, OpRewind, OpNext, OpIfEmpty, OpIf, OpIfNot, OpIsNull, OpNotNull, OpGosub:
		return in.P2, true
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		return in.P2, in.P5&p5StoreP2 == 0
	}
	return 0, false
}

// recInverseCompare is the comparison that holds exactly when op's does not, for
// two non-NULL operands; 0 for an opcode that is not one of the six.
func recInverseCompare(op OpCode) OpCode {
	switch op {
	case OpEq:
		return OpNe
	case OpNe:
		return OpEq
	case OpLt:
		return OpGe
	case OpGe:
		return OpLt
	case OpGt:
		return OpLe
	case OpLe:
		return OpGt
	}
	return 0
}

func recArith(op OpCode) bool {
	switch op {
	case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		return true
	}
	return false
}

// recRenameReads makes every instruction of ins that reads register from read
// to instead, or with dry only reports whether it can: not when from is read as
// part of a span (a function's arguments, a result row).
func recRenameReads(ins []Instruction, from, to int, dry bool) bool {
	for i := range ins {
		in := &ins[i]
		var slots []*int
		switch in.Op {
		case OpSCopy, OpCopy, OpNot, OpBitNot, OpNegative, OpIf, OpIfNot, OpIsNull, OpNotNull, OpReturn, OpCast:
			slots = []*int{&in.P1}
		case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
			slots = []*int{&in.P1, &in.P2}
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			slots = []*int{&in.P1, &in.P3}
		case OpFunction, OpResultRow:
			if from >= in.P1 && from < in.P1+in.P2 {
				return false
			}
		}
		for _, p := range slots {
			if *p == from && !dry {
				*p = to
			}
		}
	}
	return true
}
