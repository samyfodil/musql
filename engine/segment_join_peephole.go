package engine

// segJoinPeephole recognises an aggregate over an inner join of plain table
// loops and puts OpSegJoinAgg (segment_join.go) in front of the loop. The loop
// stays: the guard declines at run time for anything its blocks cannot answer.
//
// The shape, for levels 0..k-1:
//
//	Init / AggReset / OpenRead x k / [AutoIndexOrder, IfEmpty -> close]
//	Rewind c0 -> close
//	  [Column cs.x -> r; Seek{Rowid,Index}Hint ci r 1]  once per level i >= 1
//	  Rewind ci -> next(i-1)                           |
//	  Column a / Column b / Eq / IfNot -> next(i)      | the join's recheck
//	  [Column c.x -> r; SCopy r -> arg] ...  the aggregate's arguments
//	  AggStep
//	next(k-1) .. next(0): Next ci -> body(i)
//	Close x k / AggResult ... / ResultRow / Halt
//
// The seek hint only narrows the rows a level visits; the equality the
// recheck tests is the whole join condition, and it is that the kernel
// answers. Every instruction, register and jump is checked.
func segJoinPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 14 || in[0].Op != OpInit || in[1].Op != OpAggReset || segMainEnd(in) != len(in) {
		return false
	}
	pc := 2
	opened := map[int]bool{}
	for pc < len(in) && in[pc].Op == OpOpenRead {
		opened[in[pc].P1] = true
		pc++
	}
	k := len(opened)
	if k < 2 || pc-2 != k {
		return false
	}
	// A prologue the answer does not depend on: an order for a level to visit
	// its rows in, which sums and counts of integers cannot show, and a test
	// that skips the loop for an empty table, which joins no rows either way.
	var ifEmpty []int
	for pc < len(in) && (in[pc].Op == OpAutoIndexOrder || in[pc].Op == OpIfEmpty) {
		if !opened[in[pc].P1] {
			return false
		}
		if in[pc].Op == OpIfEmpty {
			ifEmpty = append(ifEmpty, pc)
		}
		pc++
	}
	if pc >= len(in) || in[pc].Op != OpRewind {
		return false
	}
	rewind0 := pc

	// The levels.
	type level struct {
		cursor, rewind, body int
	}
	var levels []level
	var edges []segJoinEdge
	levelOf := map[int]int{}
	add := func(cursor, rewind int) bool {
		if !opened[cursor] {
			return false
		}
		if _, dup := levelOf[cursor]; dup {
			return false
		}
		levelOf[cursor] = len(levels)
		levels = append(levels, level{cursor: cursor, rewind: rewind, body: rewind + 1})
		return true
	}
	if !add(in[rewind0].P1, rewind0) {
		return false
	}
	pc = rewind0 + 1
	ifNots := map[int]int{} // IfNot pc -> its level
	for len(levels) < k {
		if pc+2 < len(in) && in[pc].Op == OpColumn &&
			(in[pc+1].Op == OpSeekRowidHint || in[pc+1].Op == OpSeekIndexHint) &&
			in[pc+1].P2 == in[pc].P3 && in[pc+1].P3 == 1 && in[pc+2].Op == OpRewind && in[pc+2].P1 == in[pc+1].P1 {
			if _, ok := levelOf[in[pc].P1]; !ok {
				return false // the key comes from a level not entered yet
			}
			pc += 2
		}
		if pc+4 >= len(in) || in[pc].Op != OpRewind || !add(in[pc].P1, pc) {
			return false
		}
		cur := in[pc].P1
		// The recheck: two column reads, their equality, and the skip.
		ca, cb, eq, skip := in[pc+1], in[pc+2], in[pc+3], in[pc+4]
		la, aCol, okA := segJoinColRead(ca, levelOf)
		lb, bCol, okB := segJoinColRead(cb, levelOf)
		if !okA || !okB || eq.Op != OpEq || !segJoinEqOK(eq) ||
			!(eq.P1 == ca.P3 && eq.P3 == cb.P3 || eq.P1 == cb.P3 && eq.P3 == ca.P3) ||
			skip.Op != OpIfNot || skip.P1 != eq.P2 || skip.P3 != 1 || ca.P3 == cb.P3 {
			return false
		}
		self := levelOf[cur]
		if (la == self) == (lb == self) {
			return false // one side must be this level, the other an outer one
		}
		edges = append(edges, segJoinEdge{a: la, aCol: aCol, b: lb, bCol: bCol})
		ifNots[pc+4] = self
		pc += 5
	}

	// The aggregate's arguments, then its step.
	var args []segJoinArg
	loaded := map[int]segJoinArg{}
	for pc < len(in) && in[pc].Op != OpAggStep {
		switch in[pc].Op {
		case OpColumn, OpRowid:
			l, c, ok := segJoinColRead(in[pc], levelOf)
			if !ok {
				return false
			}
			loaded[in[pc].P3] = segJoinArg{reg: in[pc].P3, level: l, col: c}
		case OpSCopy:
			src, ok := loaded[in[pc].P1]
			if !ok {
				return false
			}
			src.reg = in[pc].P2
			args = append(args, src)
		default:
			return false
		}
		pc++
	}
	if pc >= len(in) {
		return false
	}
	step := in[pc]
	plan, ok := step.P4.(*aggPlan)
	if !ok || step.P3 != 0 || plan == nil || !plan.rowFree || plan.anchorCheck != nil ||
		len(plan.orderPlans) != 0 || !aggPlanReadsOnlyLoweredColsExceptOrder(plan) {
		return false
	}
	pc++

	// The Next chain, innermost first, each back to its level's body; every
	// skip and empty-level jump lands on the right one.
	for i := k - 1; i >= 0; i-- {
		lv := levels[i]
		if pc >= len(in) || in[pc].Op != OpNext || in[pc].P1 != lv.cursor || in[pc].P2 != lv.body {
			return false
		}
		if i+1 < k && in[levels[i+1].rewind].P2 != pc {
			return false
		}
		for at, l := range ifNots {
			if l == i && in[at].P2 != pc {
				return false
			}
		}
		pc++
	}
	closeAt := pc
	if in[rewind0].P2 != closeAt {
		return false
	}
	for _, at := range ifEmpty {
		if in[at].P2 != closeAt {
			return false
		}
	}
	for range k {
		if pc >= len(in) || in[pc].Op != OpClose || !opened[in[pc].P1] {
			return false
		}
		pc++
	}
	for pc < len(in) && in[pc].Op == OpAggResult {
		if segAggPlanOf(in[pc]) != plan {
			return false
		}
		pc++
	}
	if pc+2 != len(in) || in[pc].Op != OpResultRow || in[pc+1].Op != OpHalt {
		return false
	}

	// The guard goes before the first Rewind, after the opens; on success it
	// jumps to the Closes, whose AggResults finalize what it accumulated.
	cursors := make([]int, k)
	for i, lv := range levels {
		cursors[i] = lv.cursor
	}
	jp := &segJoinPlan{cursors: cursors, edges: edges, args: args, agg: plan, seg: step.P2}
	out := make([]Instruction, 0, len(in)+1)
	shift := func(ins Instruction) Instruction {
		if (segJumpsP2(ins.Op) || ins.Op == OpIfEmpty) && ins.P2 >= rewind0 {
			ins.P2++
		}
		return ins
	}
	for _, ins := range in[:rewind0] {
		out = append(out, shift(ins))
	}
	out = append(out, Instruction{Op: OpSegJoinAgg, P2: closeAt + 1, P4: jp})
	for _, ins := range in[rewind0:] {
		out = append(out, shift(ins))
	}
	prog.Insns = out
	return true
}

// segJoinColRead is the level and column an OpColumn or OpRowid reads, the
// rowid as column -1.
func segJoinColRead(in Instruction, levelOf map[int]int) (level, col int, ok bool) {
	l, known := levelOf[in.P1]
	switch {
	case !known:
		return 0, 0, false
	case in.Op == OpRowid:
		return l, -1, true
	case in.Op == OpColumn && in.P2 >= 0:
		return l, in.P2, true
	}
	return 0, 0, false
}

// segJoinEqOK reports whether an OpEq is the plain equality the kernel
// answers over integers: its result stored, NULL not equal to NULL. Its
// affinity and collation cannot change how two integers compare.
func segJoinEqOK(eq Instruction) bool {
	return eq.P5&p5StoreP2 != 0 && eq.P5&(p5JumpIfNull|p5NullEq) == 0
}
