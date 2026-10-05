package engine

// Recognizer for hash-drained GROUP BY over columnar tables. A separate matcher
// (like ORDER BY) due to its own skeleton. Replaces only the scan loop; the drain
// is unchanged, preserving group order and finalization.

// segGroupPeephole rewrites GROUP BY with all-integer keys and lowered arguments.
func segGroupPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 10 || in[0].Op != OpInit || in[1].Op != OpOpenRead {
		return false
	}
	rewindAt := 2
	for rewindAt < len(in) && segIsScanPrologue(in[rewindAt].Op) {
		rewindAt++
	}
	if rewindAt >= len(in) || in[rewindAt].Op != OpRewind {
		return false
	}
	cursor := in[rewindAt].P1
	if in[1].P1 != cursor {
		return false
	}

	// Scan body: Column/SCopy, MakeRecord for key, Column/SCopy for args, then step.
	regCol := map[int]int{}
	pc := rewindAt + 1
	var keyBase, keyN int
	sawKey := false
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
		case OpMakeRecord:
			if sawKey {
				return false // one group-key record, not two
			}
			sawKey, keyBase, keyN = true, in[pc].P1, in[pc].P2
		default:
			goto bodyDone
		}
		pc++
	}
bodyDone:
	if !sawKey || keyN <= 0 || pc+1 >= len(in) {
		return false
	}
	step := in[pc]
	if step.Op != OpHashAggStep || in[pc+1].Op != OpNext ||
		in[pc+1].P1 != cursor || in[pc+1].P2 != rewindAt+1 {
		return false
	}
	agg, okAgg := step.P4.(*aggPlan)
	if !okAgg || agg == nil || len(agg.cursors) != 1 || agg.cursors[0] != cursor {
		return false
	}
	// Driver hands empty rows; plans reading them (bare columns, anchors) must decline.
	// ORDER BY excluded: OpHashAggSort emits only for keys ascending (no evaluation).
	if !aggPlanReadsOnlyLoweredColsExceptOrder(agg) {
		return false
	}
	// OpAutoIndexOrder uses index order, not ROWID order; order-sensitive aggregates must decline.
	for i := 2; i < rewindAt; i++ {
		if in[i].Op != OpAutoIndexOrder {
			continue
		}
		for _, it := range aggPlanTemplates(agg) {
			if it.orderSensitive() {
				return false
			}
		}
	}

	gplan := &segGroupPlan{agg: agg, seg: step.P2}
	for i := 0; i < keyN; i++ {
		c, okc := regCol[keyBase+i]
		if !okc {
			return false
		}
		gplan.keyCols = append(gplan.keyCols, c)
		gplan.keyRegs = append(gplan.keyRegs, keyBase+i)
	}
	// The lowered argument registers are every OTHER register the body wrote
	// that is not part of the key record and not the scratch a Column landed
	// in first. Taking them from the SCopy destinations is what identifies
	// them: planAggArgRegs reserved those registers, and the body copies each
	// argument into its own.
	for i := rewindAt + 1; i < pc; i++ {
		if in[i].Op != OpSCopy {
			continue
		}
		dst := in[i].P2
		if dst >= keyBase && dst < keyBase+keyN {
			continue // part of the group key
		}
		c, okc := regCol[dst]
		if !okc {
			return false
		}
		gplan.argCols = append(gplan.argCols, c)
		gplan.argRegs = append(gplan.argRegs, dst)
	}

	// The drain must be the hash one, untouched, and must follow immediately.
	at := pc + 2
	if at >= len(in) || in[at].Op != OpClose {
		return false
	}
	at++
	if at >= len(in) || in[at].Op != OpHashAggSort {
		return false
	}
	sortAt := at

	// Rewrite: the guard goes before the Rewind, and on success jumps straight
	// to the drain with the buckets already filled.
	out := make([]Instruction, 0, len(in)+1)
	out = append(out, in[0], in[1])
	out = append(out, Instruction{Op: OpSegHashAgg, P1: cursor, P2: sortAt + 1, P4: gplan})
	for _, ins := range in[2:] {
		switch ins.Op {
		case OpRewind, OpNext, OpIfNot, OpHashAggSort, OpHashAggNext, OpIf, OpGoto, OpSorterCheck:
			ins.P2++
		}
		out = append(out, ins)
	}
	prog.Insns = out
	return true
}
