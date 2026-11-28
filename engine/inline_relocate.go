package engine

// An inliner copies a sub-program's instructions into the program that runs
// it (exists_inline.go, rec_inline.go, subquery_inline.go). The copy keeps
// every operand's meaning: its registers move past the program's own, its
// cursors and sub-cache slots likewise, and its jump targets to where the
// copy put them. inlineOperands is the one list of opcodes whose every
// operand that move is known for, and inlineReloc.relocate the one place that
// moves them, so an opcode added here is one every inliner can copy.
//
// What a body's ends become (Init, Halt, ResultRow), whether an outer-frame
// read can move, and which derived sources may be opened again each run are
// the site's own questions, answered before relocate runs.

// inlineReloc is where one inlined body lands.
type inlineReloc struct {
	regOff, curOff, slotOff int
	jump                    func(addr int) int
}

func (x inlineReloc) reg(r int) int { return r + x.regOff }
func (x inlineReloc) cur(c int) int { return c + x.curOff }

// inlineOperands reports whether relocate knows every operand of in.
func inlineOperands(in Instruction) bool {
	switch in.Op {
	case OpRewind:
		return in.P4 == nil || rowFilterMoves(in.P4)
	case OpFunction:
		return inlineFunctionMoves(in)
	case OpSubquery, OpExists:
		sub, ok := in.P4.(*Program)
		return ok && in.P5&p5Correlated == 0 && sameRowsEveryRun(sub)
	case OpInSub:
		plan, ok := in.P4.(*inSubPlan)
		return ok && !plan.correlated && sameRowsEveryRun(plan.prog)
	case OpRowSub:
		plan, ok := in.P4.(*rowSubPlan)
		return ok && !plan.correlated && sameRowsEveryRun(plan.prog)
	case OpInit, OpGoto, OpHalt, OpResultRow, OpGosub, OpReturn,
		OpOpenRead, OpOpenDerived, OpNext, OpIfEmpty, OpClose, OpAutoIndexOrder,
		OpColumn, OpRowid, OpOuterColumn, OpOuterRowid,
		OpSeekRowidHint, OpSeekIndexHint, OpSeekRowidRangeHint, OpRowidReadBound, OpSeekRowidSetHint, OpSortedSeek,
		OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable, OpSCopy, OpCopy, OpNot, OpBitNot, OpNegative, OpCast,
		OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight,
		OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIf, OpIfNot, OpIsNull, OpNotNull:
		return true
	}
	return false
}

// relocate is in moved into the program. Init, Halt, ResultRow and the
// outer-frame reads are the site's, which never passes them here.
func (x inlineReloc) relocate(in Instruction) Instruction {
	r := in
	switch in.Op {
	case OpGoto:
		r.P2 = x.jump(in.P2)
	case OpGosub:
		r.P1, r.P2 = x.reg(in.P1), x.jump(in.P2)
	case OpReturn, OpCast:
		r.P1 = x.reg(in.P1)
	case OpOpenRead, OpClose, OpAutoIndexOrder:
		r.P1 = x.cur(in.P1)
	case OpOpenDerived:
		ds := inlinedSource(in.P4.(*derivedSource))
		if len(ds.vtabCorr) > 0 {
			cp := *ds
			cp.vtabCorr = make([]vtabCorrTerm, len(ds.vtabCorr))
			for k, t := range ds.vtabCorr {
				t.reg = x.reg(t.reg)
				cp.vtabCorr[k] = t
			}
			ds = &cp
		}
		r.P1, r.P2, r.P4 = x.cur(in.P1), in.P2+x.slotOff, ds
	case OpRewind, OpNext, OpIfEmpty:
		r.P1, r.P2 = x.cur(in.P1), x.jump(in.P2)
	case OpColumn:
		r.P1, r.P3 = x.cur(in.P1), x.reg(in.P3)
	case OpRowid:
		r.P1, r.P2 = x.cur(in.P1), x.reg(in.P2)
	case OpSeekRowidHint, OpSeekIndexHint, OpSeekRowidRangeHint, OpRowidReadBound, OpSeekRowidSetHint:
		r.P1, r.P2 = x.cur(in.P1), x.reg(in.P2)
	case OpSortedSeek:
		r.P1, r.P2, r.P4 = x.cur(in.P1), x.reg(in.P2), in.P4.(*sortedSeekHint).movedRegs(x.reg)
	case OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable:
		r.P2 = x.reg(in.P2)
	case OpSCopy, OpCopy, OpNot, OpBitNot, OpNegative:
		r.P1, r.P2 = x.reg(in.P1), x.reg(in.P2)
	case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		r.P1, r.P2, r.P3 = x.reg(in.P1), x.reg(in.P2), x.reg(in.P3)
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		r.P1, r.P3 = x.reg(in.P1), x.reg(in.P3)
		if in.P5&p5StoreP2 != 0 {
			r.P2 = x.reg(in.P2)
		} else {
			r.P2 = x.jump(in.P2)
		}
	case OpIf, OpIfNot, OpIsNull, OpNotNull:
		r.P1, r.P2 = x.reg(in.P1), x.jump(in.P2)
	case OpFunction:
		r.P1, r.P3 = x.reg(in.P1), x.reg(in.P3)
	case OpSubquery, OpExists:
		r.P1, r.P2 = x.reg(in.P1), in.P2+x.slotOff
	case OpInSub, OpRowSub:
		r.P1, r.P2, r.P3 = x.reg(in.P1), x.reg(in.P2), in.P3+x.slotOff
	}
	return r
}

// outerRead is in, a read of a frame in.P5 levels up, in a body inlined into
// the frame one level up: level 1 is then the program's own cursor.
func (x inlineReloc) outerRead(in Instruction) Instruction {
	switch {
	case in.Op == OpOuterColumn && in.P5 == 1:
		return Instruction{Op: OpColumn, P1: in.P1, P2: in.P2, P3: x.reg(in.P3)}
	case in.Op == OpOuterRowid && in.P5 == 1:
		return Instruction{Op: OpRowid, P1: in.P1, P2: x.reg(in.P2)}
	case in.Op == OpOuterColumn:
		in.P3 = x.reg(in.P3)
	default:
		in.P2 = x.reg(in.P2)
	}
	in.P5--
	return in
}

// sameRowsEveryRun reports whether an uncorrelated subquery in a body gives
// the same rows on every run of the body, which is when keptSubquery would
// keep them. Inlined, it runs once a statement from the program's own slot,
// as C's OP_Once does, rather than once per run of the body's machine.
func sameRowsEveryRun(sub *Program) bool {
	return sub != nil && !sub.Correlated && subKeepable(sub) && subCTEsShared(sub)
}

// rowFilterMoves reports whether a Rewind's P4 moves unchanged. A row filter
// (segment_row_filter.go) reads table columns and bound parameters into
// registers of its own; a service block would run on the machine's.
func rowFilterMoves(p4 any) bool {
	f, ok := p4.(*segRowFilter)
	return ok && f.low != nil && len(f.low.blocks) == 0
}

// inlineFunctionMoves reports whether an OpFunction can run in another
// machine: these two read the one they run on.
func inlineFunctionMoves(in Instruction) bool {
	name, isName := in.P4.(string)
	return !isName || name != "rtreecheck" && name != "vec1_config"
}
