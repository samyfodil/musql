package engine

// Recognizer for "ORDER BY <int columns> LIMIT n [OFFSET m]" over columnar
// tables (a literal OFFSET; the sorter's bound is then m+n).
// Introduces a guard and leaves the original program as a fallback for
// runtime declines (non-int64 columns, NULLs, exceptions).

// segOrderPeephole rewrites prog when it is an ORDER BY ... LIMIT over a table
// whose key and output columns are all plain integers. Reports whether it did.
func segOrderPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 14 || in[0].Op != OpInit || in[1].Op != OpSorterOpen || in[2].Op != OpOpenRead {
		return false
	}
	ki, ok := in[1].P4.(*sorterKeyInfo)
	if !ok || ki == nil || ki.nKey <= 0 || ki.bound <= 0 {
		// bound <= 0 is unbounded: nothing to optimize.
		return false
	}
	// NULLS placement must be DEFAULT (no special code needed).
	// COLLATION is passed to the driver; both paths use the same comparator.
	for i := 0; i < ki.nKey; i++ {
		if i < len(ki.nulls) && ki.nulls[i] != NullsDefault {
			return false
		}
	}
	sorterNum := in[1].P1

	rewindAt := 3
	for rewindAt < len(in) && segIsScanPrologue(in[rewindAt].Op) {
		// OpAutoIndexOrder uses index order for tie-breaking, not rowid order.
		if in[rewindAt].Op == OpAutoIndexOrder {
			return false
		}
		rewindAt++
	}
	if rewindAt >= len(in) || in[rewindAt].Op != OpRewind {
		return false
	}
	cursor := in[rewindAt].P1
	if in[2].P1 != cursor {
		return false
	}

	// Scan body: only Column and SCopy allowed, building register->column map.
	regCol := map[int]int{}
	pc := rewindAt + 1
	for pc < len(in) {
		switch in[pc].Op {
		case OpColumn:
			if in[pc].P1 != cursor {
				return false
			}
			regCol[in[pc].P3] = in[pc].P2
		case OpSCopy:
			src, okSrc := regCol[in[pc].P1]
			if !okSrc {
				return false
			}
			regCol[in[pc].P2] = src
		default:
			goto bodyDone
		}
		pc++
	}
bodyDone:
	if pc < len(in) && in[pc].Op == OpSorterCheck {
		// Sorter size guard: skipped because this path builds only top-k records.
		if in[pc].P1 != sorterNum {
			return false
		}
		pc++
	}
	if pc+2 >= len(in) || in[pc].Op != OpMakeRecord || in[pc+1].Op != OpSorterInsert ||
		in[pc+2].Op != OpNext {
		return false
	}
	recBase, nRec := in[pc].P1, in[pc].P2
	if in[pc+1].P1 != sorterNum || in[pc+2].P1 != cursor || in[pc+2].P2 != rewindAt+1 {
		return false
	}
	if nRec <= ki.nKey {
		return false
	}
	cols := make([]int, nRec)
	for i := 0; i < nRec; i++ {
		c, okc := regCol[recBase+i]
		if !okc {
			return false
		}
		cols[i] = c
	}

	offset, outBase, nOut, ok := segSorterDrain(in, pc+3, sorterNum, ki.bound)
	if !ok {
		return false
	}
	// Output column i is record field nKey + i (payload starts after keys).
	if ki.nKey+nOut > nRec {
		return false
	}
	plan := &segOrderPlan{limit: ki.bound, offset: offset, nOut: nOut}
	for i := 0; i < ki.nKey; i++ {
		plan.keyCols = append(plan.keyCols, cols[i])
		desc := false
		if i < len(ki.desc) {
			desc = ki.desc[i]
		}
		plan.keyDesc = append(plan.keyDesc, desc)
		coll := ""
		if i < len(ki.coll) {
			coll = ki.coll[i]
		}
		plan.keyColl = append(plan.keyColl, coll)
	}
	for i := 0; i < nOut; i++ {
		plan.outCols = append(plan.outCols, cols[ki.nKey+i])
	}

	// Rewrite: insert guard at index 3, shift subsequent addresses, append emit loop.
	out := make([]Instruction, 0, len(in)+5)
	out = append(out, in[0], in[1], in[2])
	emitAt := len(in) + 1
	out = append(out, Instruction{Op: OpSegOrderLimit, P1: outBase, P2: cursor, P3: emitAt, P4: plan})
	for _, ins := range in[3:] {
		switch ins.Op {
		case OpRewind, OpNext, OpIfNot, OpSorterSort, OpSorterNext, OpIf, OpGoto, OpSorterCheck:
			ins.P2++
		}
		out = append(out, ins)
	}
	out = append(out,
		Instruction{Op: OpSegEmitRow, P1: outBase, P2: emitAt + 3, P3: nOut},
		Instruction{Op: OpResultRow, P1: outBase, P2: nOut},
		Instruction{Op: OpGoto, P2: emitAt},
		Instruction{Op: OpHalt})
	prog.Insns = out
	return true
}

// segSorterDrain matches the sorter's drain from the OpClose that ends the
// fill loop at closeAt to the program's final OpHalt -- exactly, because a
// drain the recognizer misread would emit wrong rows. It reports the OFFSET,
// the first output register and the number of output columns.
func segSorterDrain(in []Instruction, closeAt, sorterNum, bound int) (offset, outBase, nOut int, ok bool) {
	at := closeAt
	if at >= len(in) || in[at].Op != OpClose {
		return 0, 0, 0, false
	}
	at++
	// "Integer limit; Integer 1", or with an OFFSET "Integer offset; Integer
	// limit; Integer 1", whose drain then skips the offset's rows first:
	// "If offset -> skip; Goto rows; skip: Subtract; Goto next".
	if at+2 >= len(in) || in[at].Op != OpInteger || in[at+1].Op != OpInteger {
		return 0, 0, 0, false
	}
	if in[at+2].Op == OpInteger {
		offset = int(in[at].P1)
		if offset < 0 || offset+int(in[at+1].P1) != bound {
			return 0, 0, 0, false
		}
		at++
	}
	at += 2
	if at >= len(in) || in[at].Op != OpSorterSort || in[at].P1 != sorterNum {
		return 0, 0, 0, false
	}
	sortAt := at
	if offset > 0 {
		if sortAt+4 >= len(in) || in[sortAt+1].Op != OpIf || in[sortAt+1].P2 != sortAt+3 ||
			in[sortAt+2].Op != OpGoto || in[sortAt+2].P2 != sortAt+5 || in[sortAt+3].Op != OpSubtract || in[sortAt+4].Op != OpGoto {
			return 0, 0, 0, false
		}
		sortAt += 4
	}
	if sortAt+3 >= len(in) || in[sortAt+1].Op != OpIf || in[sortAt+2].Op != OpGoto ||
		in[sortAt+3].Op != OpSorterData || in[sortAt+3].P1 != sorterNum {
		return 0, 0, 0, false
	}
	recReg := in[sortAt+3].P2
	at = sortAt + 4
	outBase = -1
	for at < len(in) && in[at].Op == OpRecordColumn {
		if in[at].P1 != recReg || in[at].P2 != nOut {
			return 0, 0, 0, false
		}
		if outBase < 0 {
			outBase = in[at].P3
		} else if in[at].P3 != outBase+nOut {
			return 0, 0, 0, false
		}
		nOut++
		at++
	}
	if nOut == 0 || at+3 >= len(in) {
		return 0, 0, 0, false
	}
	if in[at].Op != OpResultRow || in[at].P1 != outBase || in[at].P2 != nOut {
		return 0, 0, 0, false
	}
	if in[at+1].Op != OpSubtract || in[at+2].Op != OpSorterNext || in[at+2].P1 != sorterNum ||
		in[at+3].Op != OpHalt || at+3 != len(in)-1 {
		return 0, 0, 0, false
	}
	return offset, outBase, nOut, true
}
