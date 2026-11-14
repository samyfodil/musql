package engine

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
		if !recInlineOperands(in) {
			return false
		}
		switch in.Op {
		case OpOpenDerived:
			ds, ok := in.P4.(*derivedSource)
			if !ok || ds == nil {
				return false
			}
			if ds.recSelf == nil && !derivedReadsSameRows(ds) ||
				ds.recSelf != nil && (ds.catalogScope != scopeAny || ds.vtabWrite != nil || ds.dbIdx != 0) {
				return false
			}
		case OpRewind:
			if in.P4 != nil {
				return false // a row-filter plan: not read here, so not relocated
			}
		case OpFunction:
			if _, isName := in.P4.(string); !isName {
				return false
			}
		}
	}
	return true
}

// recInlineOperands reports whether in is an opcode whose every operand this
// pass knows: the whitelist recInline relocates, operand by operand.
func recInlineOperands(in Instruction) bool {
	switch in.Op {
	case OpInit, OpGoto, OpHalt, OpOpenDerived, OpRewind, OpNext, OpIfEmpty, OpClose, OpColumn,
		OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable, OpSCopy, OpCopy, OpNot, OpBitNot, OpNegative,
		OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight,
		OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIf, OpIfNot, OpIsNull, OpNotNull,
		OpFunction, OpResultRow, OpGosub, OpReturn:
		return true
	}
	return false
}

// recInline replaces prog.Insns[at], "RecQueueFill 1 arm", with a Gosub into a
// relocated copy of arm appended to prog, and the caps check after it.
//
// The fill is one instruction and the Gosub plus the check are two, so the
// fill becomes a Gosub to a trampoline at the end that runs the arm and then
// RecQueueCheck before returning: no address in prog moves.
func recInline(prog *Program, at int, arm *Program) {
	regOff, curOff, slotOff := prog.NReg, prog.NCursors, prog.NSubCache
	retReg := prog.NReg + arm.NReg
	base := len(prog.Insns)
	body := base                  // arm address a lands at body+a
	done := body + len(arm.Insns) // where the arm's Halt goes: the check, then Return
	reg := func(r int) int { return r + regOff }
	cur := func(c int) int { return c + curOff }
	jump := func(a int) int { return body + a }

	out := prog.Insns
	for _, in := range arm.Insns {
		r := in
		switch in.Op {
		case OpInit:
			r = Instruction{Op: OpGoto, P2: jump(in.P2)}
		case OpGoto:
			r.P2 = jump(in.P2)
		case OpHalt:
			r = Instruction{Op: OpGoto, P2: done}
		case OpResultRow:
			r = Instruction{Op: OpRecQueuePush, P1: reg(in.P1), P2: in.P2}
		case OpOpenDerived:
			r.P1, r.P2, r.P4 = cur(in.P1), in.P2+slotOff, inlinedSource(in.P4.(*derivedSource))
		case OpClose:
			r.P1 = cur(in.P1)
		case OpRewind, OpNext, OpIfEmpty:
			r.P1, r.P2 = cur(in.P1), jump(in.P2)
		case OpColumn:
			r.P1, r.P3 = cur(in.P1), reg(in.P3)
		case OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable:
			r.P2 = reg(in.P2)
		case OpSCopy, OpCopy, OpNot, OpBitNot, OpNegative:
			r.P1, r.P2 = reg(in.P1), reg(in.P2)
		case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
			r.P1, r.P2, r.P3 = reg(in.P1), reg(in.P2), reg(in.P3)
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			r.P1, r.P3 = reg(in.P1), reg(in.P3)
			if in.P5&p5StoreP2 != 0 {
				r.P2 = reg(in.P2)
			} else {
				r.P2 = jump(in.P2)
			}
		case OpIf, OpIfNot, OpIsNull, OpNotNull:
			r.P1, r.P2 = reg(in.P1), jump(in.P2)
		case OpFunction:
			r.P1, r.P3 = reg(in.P1), reg(in.P3)
		case OpGosub:
			r.P1, r.P2 = reg(in.P1), jump(in.P2)
		case OpReturn:
			r.P1 = reg(in.P1)
		}
		out = append(out, r)
	}
	out = append(out,
		Instruction{Op: OpRecQueueCheck},
		Instruction{Op: OpReturn, P1: retReg})
	out[at] = Instruction{Op: OpGosub, P1: retReg, P2: body}
	prog.Insns = out
	prog.NReg += arm.NReg + 1
	prog.NCursors += arm.NCursors
	prog.NSubCache += arm.NSubCache
}
