package engine

import "github.com/samyfodil/musql/internal/jit"

// Recognizer for hash-drained GROUP BY over columnar tables. A separate matcher
// (like ORDER BY) due to its own skeleton. Replaces only the scan loop; the drain
// is unchanged, preserving group order and finalization.

// segGroupPeephole rewrites GROUP BY with all-integer keys and lowered
// arguments, optionally under a WHERE of the predicates segParsePredBlock reads.
//
//	Init; OpenRead c (or generate_series); <prologue: seek hints, ...>; Rewind c -> close
//	top:   [<predicate blocks and IN lists, each jumping to the Next>]
//	       <the key, Column c.x or computed> ...; MakeRecord -> key
//	       [Column c.x -> r; [ClearSubtype r]; SCopy r -> arg] ...   the aggregates' arguments
//	       HashAggStep; Next c -> top
//	close: Close c; HashAggSort ... <the drain, unchanged>
//
// It puts OpSegHashAgg after the seek hints, shifting the jumps after it by
// one; on success it fills the hash buckets itself and jumps to the
// HashAggSort, past the loop and its Close.
func segGroupPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 10 || in[0].Op != OpInit {
		return false
	}
	series := segSeriesOpen(in[1])
	if in[1].Op != OpOpenRead && !series {
		return false
	}
	rewindAt := 2
	for rewindAt < len(in) && segGroupPrologue(in[rewindAt].Op) {
		rewindAt++
	}
	if rewindAt >= len(in) || in[rewindAt].Op != OpRewind {
		return false
	}
	cursor := in[rewindAt].P1
	if in[1].P1 != cursor {
		return false
	}
	// The guard goes after the seek hints, so the cursor it weighs knows its
	// seek, and before an OpAutoIndexOrder, whose materializing the walk avoids.
	guardAt := 2
	for i := 2; i < rewindAt; i++ {
		switch in[i].Op {
		case OpSeekRowidHint, OpSeekRowidSetHint, OpSeekIndexHint, OpSeekRowidRangeHint:
			guardAt = i + 1
		}
	}

	// An optional WHERE first: the same predicate groups the filtered count
	// accepts, each skipping the row by jumping to the Next after the step.
	stepAt := rewindAt + 1
	for stepAt < len(in) && in[stepAt].Op != OpHashAggStep {
		stepAt++
	}
	if stepAt+1 >= len(in) {
		return false
	}
	var preds []segPlanPred
	var lists []segPlanList
	pc := rewindAt + 1
	for {
		more, npc, ok := segParsePredBlockOnce(in, pc, stepAt, cursor, stepAt+1)
		if !ok {
			return false
		}
		preds, pc = append(preds, more...), npc
		if pl, npc, ok := segParseInListGroup(in, pc, stepAt, cursor, stepAt+1); ok {
			lists, pc = append(lists, pl), npc
			continue
		}
		break
	}

	// Scan body: the key's computation up to its MakeRecord, Column/SCopy for
	// each argument, then the step. A key that is more than a column is
	// computed by a program over the body before the record (segGroupKeys).
	regCol := map[int]int{}
	bodyAt := pc
	keyAt := -1
	plainKey := true
	for ; pc < len(in) && in[pc].Op != OpHashAggStep; pc++ {
		switch op := in[pc]; {
		case op.Op == OpMakeRecord:
			if keyAt >= 0 {
				return false // one group-key record, not two
			}
			keyAt = pc
		case op.Op == OpColumn && op.P1 == cursor:
			regCol[op.P3] = op.P2
		case op.Op == OpClearSubtype && keyAt >= 0:
			// A column's value carries no subtype to drop.
			if _, ok := regCol[op.P1]; !ok {
				return false
			}
		case op.Op == OpSCopy:
			if src, ok := regCol[op.P1]; ok {
				regCol[op.P2] = src
			} else if keyAt < 0 {
				plainKey = false
			} else {
				return false
			}
		case keyAt < 0:
			plainKey = false
		default:
			return false
		}
	}
	if keyAt < 0 || in[keyAt].P2 <= 0 || pc+1 >= len(in) {
		return false
	}
	keyBase, keyN := in[keyAt].P1, in[keyAt].P2
	step := in[pc]
	if step.Op != OpHashAggStep || in[pc+1].Op != OpNext ||
		in[pc+1].P1 != cursor || in[pc+1].P2 != rewindAt+1 {
		return false
	}
	agg, okAgg := step.P4.(*aggPlan)
	if !okAgg || agg == nil || len(agg.cursors) != 1 || agg.cursors[0] != cursor {
		return false
	}
	// The walk hands each step the cells it read, or with bare columns the
	// group's whole last row (segGroupBulk), which an order other than the
	// rowids' would move. ORDER BY excluded: OpHashAggSort emits only for keys
	// ascending (no evaluation).
	bare := false
	if !aggPlanReadsOnlyLoweredColsExceptOrder(agg) {
		if series || !aggPlanBareOnly(agg) {
			return false
		}
		for i := 2; i < rewindAt; i++ {
			switch in[i].Op {
			case OpAutoIndexOrder, OpSeekRowidHint, OpSeekRowidSetHint, OpSeekIndexHint, OpSeekRowidRangeHint:
				return false
			}
		}
		bare = true
	}
	// OpAutoIndexOrder uses index order, not ROWID order; order-sensitive
	// aggregates must decline -- but for a sum, total or avg, which an order
	// changes only by where an overflow falls, the walk may prove at run time
	// that no order overflows (segIntSumsOrderFree).
	intOrderFree := false
	for i := 2; i < rewindAt; i++ {
		if in[i].Op != OpAutoIndexOrder {
			continue
		}
		for _, it := range aggPlanTemplates(agg) {
			if !it.orderSensitive() {
				continue
			}
			if (it.kind == aggSum || it.kind == aggTotal || it.kind == aggAvg) && !it.distinct && it.filter == nil && it.orderBy == nil {
				intOrderFree = true
				continue
			}
			return false
		}
	}

	gplan := &segGroupPlan{agg: agg, seg: step.P2, preds: preds, lists: lists, bare: bare, series: series, intOrderFree: intOrderFree}
	argsAt := bodyAt
	if plainKey {
		for i := 0; i < keyN; i++ {
			c, okc := regCol[keyBase+i]
			if !okc {
				return false
			}
			gplan.keyCols = append(gplan.keyCols, c)
			gplan.keyRegs = append(gplan.keyRegs, keyBase+i)
		}
	} else {
		if keyN != 1 {
			return false
		}
		for _, x := range in[bodyAt:keyAt] {
			if _, _, jump, _ := segInsnRegs(x); jump {
				return false
			}
		}
		// Its service blocks (a math kernel's rare inputs, a call native code
		// does not make) run as an aggregate program's do (segGroupKeys).
		low, okLow := segLowerBody(in[bodyAt:keyAt], bodyAt, cursor, keyAt, nil)
		if !okLow {
			return false
		}
		low.add(jit.ProgInsn{Op: jit.POpEmitVal, A: keyBase})
		gplan.keyProg, gplan.keyCols, gplan.keyRegs = low, []int{-1}, []int{keyBase}
		gplan.keyReal = low.real[keyBase]
		argsAt = keyAt + 1
	}
	// The lowered argument registers are every OTHER register the body wrote
	// that is not part of the key record and not the scratch a Column landed
	// in first. Taking them from the SCopy destinations is what identifies
	// them: planAggArgRegs reserved those registers, and the body copies each
	// argument into its own.
	for i := argsAt; i < pc; i++ {
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

	// A bound a once-block computes is computed before the walk: a copy of
	// each such block goes ahead of the guard, which then reads its register.
	// The loop's own copy, should the walk decline, finds its flag set.
	var once []Instruction
	for _, p := range preds {
		if p.Reg == 0 {
			continue
		}
		if !plainKey {
			return false
		}
		base := guardAt + len(once)
		for _, x := range in[p.once[0]:p.once[1]] {
			if x.Op == OpNotNull {
				x.P2 = base + p.once[1] - p.once[0]
			}
			once = append(once, x)
		}
	}
	shift := 1 + len(once)

	// Rewrite: the guard goes before the Rewind, and on success jumps straight
	// to the drain with the buckets already filled.
	out := make([]Instruction, 0, len(in)+shift)
	out = append(out, in[:guardAt]...)
	out = append(out, once...)
	out = append(out, Instruction{Op: OpSegHashAgg, P1: cursor, P2: sortAt + shift, P4: gplan})
	for _, ins := range in[guardAt:] {
		if segJoinJumpsP2(ins.Op) && ins.P2 >= guardAt {
			ins.P2 += shift
		}
		out = append(out, ins)
	}
	prog.Insns = out
	return true
}

// segGroupPrologue is segIsScanPrologue with every literal a seek key may load
// and the rowid IN list's hint: the walk weighs a seek rather than ignoring
// one, so its key's class does not matter.
func segGroupPrologue(op OpCode) bool {
	switch op {
	case OpString8, OpReal, OpBlob, OpNull, OpSeekRowidSetHint:
		return true
	}
	return segIsScanPrologue(op)
}
