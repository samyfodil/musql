package engine

import (
	"reflect"
	"slices"
	"strings"
)

// The recognisers for an aggregate over an inner join of plain table loops
// (segment_join.go). Each leaves the loop in place behind a guard that
// declines at run time for anything the blocks cannot answer.
//
// The loop, for levels 0..k-1, as the compiler emits it:
//
//	OpenRead x k / [AutoIndexOrder, IfEmpty -> close]
//	Rewind c0 -> close
//	  [Column cs.x -> r; Seek{Rowid,Index}Hint ci r 1]  once per level i >= 1
//	  Rewind ci -> next(i-1)                           |
//	  Column a / Column b / Eq / IfNot -> next(i)      | the join's recheck
//	  <body>
//	next(k-1) .. next(0): Next ci -> body(i)
//	close: Close x k
//
// The seek hint only narrows the rows a level visits; the equality the
// recheck tests is the whole join condition, and it is that the kernel
// answers. Every instruction, register and jump is checked.

// segJoinLoop is a recognised loop.
type segJoinLoop struct {
	opened  map[int]bool
	levelOf map[int]int // cursor -> level
	cursors []int       // per level
	rewinds []int       // per level, its Rewind
	edges   []segJoinEdge
	ifNots  map[int]int // each recheck's IfNot -> its level
	ifEmpty []int
	orders  []segJoinOrder
	// preds is each level's own WHERE terms, tested right after its Rewind:
	// a column against a constant, which skip the level's row. filters are
	// the level's other terms: an expression over its columns (a LIKE, a
	// function), whose row is skipped unless the value is true.
	preds   [][]segPlanPred
	filters [][]segJoinArg
	// head is the first instruction after the opens -- the prologue's, or the
	// first Rewind -- where a guard goes, before anything reads a table.
	head    int
	rewind0 int
	body    int // the first instruction of the body
	// outer is the level a LEFT JOIN makes optional, or -1: only the
	// innermost may be. Its match flag is register flag; tail sets nullBody
	// to the copy of the body the loop runs, after a NullRow, for an outer
	// row nothing matched.
	outer    int
	flag     int
	nullBody [2]int
}

// parseSegJoinLoop reads the loop's head from the first OpenRead at open:
// the opens, the prologue, the levels and their rechecks, up to the body.
func parseSegJoinLoop(in []Instruction, open int) (*segJoinLoop, bool) {
	l := &segJoinLoop{opened: map[int]bool{}, levelOf: map[int]int{}, ifNots: map[int]int{}, outer: -1}
	pc := open
	var ok bool
	for pc < len(in) && in[pc].Op == OpOpenRead {
		l.opened[in[pc].P1] = true
		pc++
	}
	k := len(l.opened)
	if k < 2 || pc-open != k {
		return nil, false
	}
	l.head = pc
	// A prologue the answer does not depend on: an order for a level to visit
	// its rows in (whether a caller can ignore it is its own call), and a test
	// that skips the loop for an empty table, which joins no rows either way.
	for pc < len(in) && (in[pc].Op == OpAutoIndexOrder || in[pc].Op == OpIfEmpty) {
		if !l.opened[in[pc].P1] {
			return nil, false
		}
		if in[pc].Op == OpIfEmpty {
			l.ifEmpty = append(l.ifEmpty, pc)
		} else {
			key, ok := in[pc].P4.(*autoIndexKey)
			if !ok {
				return nil, false
			}
			l.orders = append(l.orders, segJoinOrder{cursor: in[pc].P1, key: key})
		}
		pc++
	}
	if pc >= len(in) || in[pc].Op != OpRewind {
		return nil, false
	}
	l.rewind0 = pc
	add := func(cursor, rewind int) bool {
		if !l.opened[cursor] {
			return false
		}
		if _, dup := l.levelOf[cursor]; dup {
			return false
		}
		l.levelOf[cursor] = len(l.cursors)
		l.cursors = append(l.cursors, cursor)
		l.rewinds = append(l.rewinds, rewind)
		return true
	}
	if !add(in[pc].P1, pc) {
		return nil, false
	}
	pc++
	// A level's own WHERE terms, right after its Rewind, each skipping to its
	// Next.
	levelPreds := func(pc, limit int) (int, bool) {
		lv := len(l.cursors) - 1
		cursor, rewind := l.cursors[lv], l.rewinds[lv]
		next := -1
		for i, ins := range in {
			if ins.Op == OpNext && ins.P1 == cursor && ins.P2 == rewind+1 {
				if next >= 0 {
					return 0, false
				}
				next = i
			}
		}
		if next < 0 {
			return 0, false
		}
		var preds []segPlanPred
		var filters []segJoinArg
		for {
			more, end, ok := segParsePredBlock(in, pc, limit, cursor, next)
			if !ok {
				return 0, false
			}
			preds, pc = append(preds, more...), end
			f, end, ok := segJoinFilter(in, pc, limit, lv, l.levelOf, next)
			if !ok {
				break
			}
			filters, pc = append(filters, f), end
		}
		l.preds = append(l.preds, preds)
		l.filters = append(l.filters, filters)
		return pc, true
	}
	if pc, ok = levelPreds(pc, len(in)); !ok {
		return nil, false
	}
	for len(l.cursors) < k {
		// The seek's key and hint, before the Rewind (and a LEFT JOIN's flag).
		rewindAt := pc + 2
		if rewindAt+1 < len(in) && in[rewindAt].Op == OpInteger && in[rewindAt].P1 == 0 {
			rewindAt++
		}
		if rewindAt < len(in) && in[pc].Op == OpColumn &&
			(in[pc+1].Op == OpSeekRowidHint || in[pc+1].Op == OpSeekIndexHint) &&
			in[pc+1].P2 == in[pc].P3 && in[pc+1].P3 == 1 && in[rewindAt].Op == OpRewind && in[rewindAt].P1 == in[pc+1].P1 {
			if _, ok := l.levelOf[in[pc].P1]; !ok {
				return nil, false // the key comes from a level not entered yet
			}
			pc += 2
		}
		// A LEFT JOIN's level clears its match flag before its Rewind.
		flag := -1
		if pc+1 < len(in) && in[pc].Op == OpInteger && in[pc].P1 == 0 && in[pc+1].Op == OpRewind {
			if len(l.cursors) != k-1 {
				return nil, false // only the innermost level
			}
			flag = in[pc].P2
			pc++
		}
		if pc+4 >= len(in) || in[pc].Op != OpRewind || !add(in[pc].P1, pc) {
			return nil, false
		}
		self := l.levelOf[in[pc].P1]
		next := -1
		for i, ins := range in {
			if ins.Op == OpNext && ins.P1 == l.cursors[self] && ins.P2 == l.rewinds[self]+1 {
				next = i
			}
		}
		// The join's recheck: an equality whose failure skips the row -- to
		// the Next, or, in a LEFT JOIN's ON tree (IfNot P3 0), to the tree's
		// false label -- between a value of this level and one of an earlier
		// level, columns or expressions over them. The level's own terms come
		// first, and a term can be an equality too: each candidate is tried
		// in turn, the first that parses whole taking it.
		eqAt, preds0, filters0 := -1, len(l.preds), len(l.filters)
		var x, y segJoinArg
		for p := pc + 1; p+1 < len(in) && eqAt < 0; p++ {
			e, sk := in[p], in[p+1]
			if e.Op != OpEq || sk.Op != OpIfNot || sk.P1 != e.P2 ||
				!(sk.P3 == 1 && sk.P2 == next || flag >= 0 && sk.P3 == 0) || !segJoinEqOK(e) {
				continue
			}
			l.preds, l.filters = l.preds[:preds0], l.filters[:filters0]
			end, ok := levelPreds(pc+1, p)
			if !ok {
				continue
			}
			tr := newSegJoinTrace(in, l.levelOf)
			traced := true
			for q := end; q < p && traced; q++ {
				traced = !segJoinIsJump(in[q]) && tr.step(q)
			}
			a, okx := tr.src[e.P1]
			b, oky := tr.src[e.P3]
			if !traced || !okx || !oky || a.guard != 0 || b.guard != 0 {
				continue
			}
			if b.level == self {
				a, b = b, a
			}
			if a.level != self || b.level < 0 || b.level >= self {
				continue // one side this level, the other an outer one
			}
			eqAt, x, y = p, a, b
		}
		if eqAt < 0 {
			return nil, false
		}
		eq, skip := in[eqAt], in[eqAt+1]
		tree := flag >= 0 && skip.P3 == 0
		l.edges = append(l.edges, segJoinEdge{a: y.level, aArg: y, b: self, bArg: x})
		pc = eqAt - 3 // the skip is at pc+4, as it was for the four-instruction recheck
		if tree {
			// The rest of the tree: terms over this level, and the tail that
			// settles the three-valued result and skips to the Next.
			next := -1
			for i, ins := range in {
				if ins.Op == OpNext && ins.P1 == l.cursors[self] && ins.P2 == l.rewinds[self]+1 {
					next = i
				}
			}
			preds, end, ok := segParsePredTree(in, pc+5, len(in), l.cursors[self], next, []int{eq.P2}, skip.P2)
			if !ok || len(preds) == 0 {
				return nil, false
			}
			l.preds[self] = append(l.preds[self], preds...)
			pc = end
		} else {
			l.ifNots[pc+4] = self
			pc += 5
		}
		if flag >= 0 {
			// ...and sets it once a row matched.
			if pc >= len(in) || in[pc].Op != OpInteger || in[pc].P1 != 1 || in[pc].P2 != flag {
				return nil, false
			}
			l.outer, l.flag = self, flag
			pc++
		}
	}
	l.body = pc
	return l, true
}

// tail checks the Next chain at pc, innermost first, each back to its level's
// body, with every skip and empty-level jump landing on the right one, and
// then the Closes. It returns the first Close and the address after them.
func (l *segJoinLoop) tail(in []Instruction, pc int) (closeAt, after int, ok bool) {
	k := len(l.cursors)
	innerEnd := -1 // where the innermost level's Rewind goes when it has no rows
	for i := k - 1; i >= 0; i-- {
		if pc >= len(in) || in[pc].Op != OpNext || in[pc].P1 != l.cursors[i] {
			return 0, 0, false
		}
		// A level's Next goes back to just after its Rewind.
		if in[pc].P2 != l.rewinds[i]+1 {
			return 0, 0, false
		}
		if i+1 < k && in[l.rewinds[i+1]].P2 != innerEnd {
			return 0, 0, false
		}
		for at, lv := range l.ifNots {
			if lv == i && in[at].P2 != pc {
				return 0, 0, false
			}
		}
		pc++
		innerEnd = pc
		if i == l.outer {
			// The LEFT JOIN's row for an outer row nothing matched: skipped
			// when the flag is set, else a NullRow and the body again, up to
			// the outer level's Next.
			if pc+1 >= len(in) || in[pc].Op != OpIf || in[pc].P1 != l.flag ||
				in[pc+1].Op != OpNullRow || in[pc+1].P1 != l.cursors[i] {
				return 0, 0, false
			}
			end := in[pc].P2
			if end <= pc+2 || end >= len(in) || in[end].Op != OpNext {
				return 0, 0, false
			}
			l.nullBody = [2]int{pc + 2, end}
			pc = end
		}
	}
	closeAt = pc
	if in[l.rewind0].P2 != closeAt {
		return 0, 0, false
	}
	for _, at := range l.ifEmpty {
		if in[at].P2 != closeAt {
			return 0, 0, false
		}
	}
	for range k {
		if pc >= len(in) || in[pc].Op != OpClose || !l.opened[in[pc].P1] {
			return 0, 0, false
		}
		pc++
	}
	return closeAt, pc, true
}

// segJoinPeephole recognises a whole-join aggregate:
//
//	Init / AggReset / <loop whose body is:
//	  [Column c.x -> r; SCopy r -> arg] ...  the aggregate's arguments
//	  AggStep>
//	AggResult ... / ResultRow / Halt
//
// and puts OpSegJoinAgg before the loop, jumping on success to the Closes,
// whose AggResults finalize what it accumulated.
func segJoinPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 14 || in[0].Op != OpInit || in[1].Op != OpAggReset || segMainEnd(in) != len(in) {
		return false
	}
	l, ok := parseSegJoinLoop(in, 2)
	if !ok {
		return false
	}
	src, pc, ok := parseSegJoinStepBody(in, l.body, len(in), l.levelOf)
	if !ok {
		return false
	}
	step := in[pc]
	plan, ok := step.P4.(*aggPlan)
	if !ok || step.P3 != 0 || plan == nil || plan.anchorCheck != nil ||
		len(plan.orderPlans) != 0 || !aggPlanReadsOnlyLoweredColsExceptOrder(plan) {
		return false
	}
	args, ok := segJoinStepArgs(plan, src)
	if !ok {
		return false
	}
	closeAt, pc, ok := l.tail(in, pc+1)
	if !ok {
		return false
	}
	if l.outer >= 0 {
		// The LEFT JOIN's null row: the same arguments, the same step.
		nsrc, end, ok := parseSegJoinStepBody(in, l.nullBody[0], l.nullBody[1], l.levelOf)
		if !ok || end+1 != l.nullBody[1] || !segJoinSameInsn(in[end], step) {
			return false
		}
		nargs, ok := segJoinStepArgs(plan, nsrc)
		if !ok || !segJoinSameArgs(nargs, args) {
			return false
		}
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

	// The guard takes the place of the first instruction after the opens and
	// runs it when it declines, so no address moves.
	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, filters: l.filters, optional: l.outer, args: args, agg: plan, seg: step.P2, orders: l.orders}
	at := len(in)
	out := append([]Instruction(nil), in...)
	out[l.head] = Instruction{Op: OpGoto, P2: at}
	out = append(out,
		Instruction{Op: OpSegJoinAgg, P2: closeAt, P4: jp},
		in[l.head],
		Instruction{Op: OpGoto, P2: l.head + 1})
	prog.Insns = out
	return true
}

// segJoinGroupPeephole recognises a GROUP BY over the loop, whose body hands
// each joined row to the drain -- by a Gosub to it (groups met in scan order,
// streamed) or through a sorter it then drains (compileScanGroupBy) -- and
// has the kernel hand the drain one row per group instead: the joined row the
// loop would have met first, so every bare column, HAVING and output reads
// what it would have, after which the group's other rows are added to the
// accumulators that row was stepped into (OpSegJoinRest, keyed by the row's
// scan sequence number).
//
// The rewrite moves no address: the first Rewind becomes a jump to the guard,
// which runs that Rewind when it declines, and the drain's AggStep a jump to a
// copy of it followed by the rest.
func segJoinGroupPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	open := 0
	for open < len(in) && in[open].Op != OpOpenRead {
		open++
	}
	if open >= len(in) || len(in) < 20 || in[0].Op != OpInit {
		return false
	}
	l, ok := parseSegJoinLoop(in, open)
	if !ok {
		return false
	}

	// The body: column loads and copies, the scan sequence counter, the record,
	// and its hand-off.
	b, pc, ok := parseSegJoinRowBody(in, l.body, len(in), l.levelOf)
	if !ok {
		return false
	}
	loads, copies, writer, seqReg, rec, mk := b.loads, b.copies, b.writer, b.seqReg, b.rec, b.mk
	hand := in[pc]
	closeAt, _, ok := l.tail(in, pc+1)
	if !ok {
		return false
	}
	if l.outer >= 0 {
		// The LEFT JOIN's null row goes through a copy of the body that
		// must build the same row from the same columns.
		nb, end, ok := parseSegJoinRowBody(in, l.nullBody[0], l.nullBody[1], l.levelOf)
		if !ok || end+1 != l.nullBody[1] || !b.same(nb) || !segJoinSameInsn(in[end], hand) {
			return false
		}
	}

	// The drain's one step, over the row the body built.
	stepAt := -1
	for i, ins := range in {
		if ins.Op == OpAggStep {
			if stepAt >= 0 || ins.P3 != 1 {
				return false
			}
			stepAt = i
		}
	}
	if stepAt < 0 {
		return false
	}
	step := in[stepAt]
	rowRec := step.P1
	plan, ok := step.P4.(*aggPlan)
	if !ok || plan == nil || plan.anchorCheck != nil {
		return false
	}
	// A min()/max() picks the row a bare column reads; the first row is not it.
	if plan.magnet != nil && len(plan.magnet.sites) > 0 && !aggPlanReadsOnlyLoweredColsExceptOrder(plan) {
		return false
	}

	// Where each value of the row the drain steps comes from: payload index
	// -> the body's register.
	nKey := 0
	var unpack *recUnpack
	if hand.Op == OpGosub {
		if rowRec != rec || mk.P4 == nil {
			return false
		}
		if _, view := mk.P4.(recView); !view {
			return false
		}
	} else {
		if hand.P2 != rec {
			return false
		}
		data := -1
		for i, ins := range in {
			if ins.Op == OpSorterData && ins.P1 == hand.P1 {
				if data >= 0 {
					return false
				}
				data = i
			}
			if ins.Op == OpSorterOpen && ins.P1 == hand.P1 {
				ki, ok := ins.P4.(*sorterKeyInfo)
				if !ok {
					return false
				}
				nKey = ki.nKey
			}
		}
		if data < 0 || in[data].P2 != rowRec {
			return false
		}
		unpack, _ = in[data].P4.(*recUnpack)
	}
	nCols := plan.nCols
	regOf := func(j int) (int, bool) {
		packed := j
		if unpack != nil {
			packed = -1
			if j < unpack.nCols {
				for i, k := range unpack.at {
					if k == j {
						packed = i
					}
				}
			} else {
				packed = len(unpack.at) + j - unpack.nCols
			}
			if packed < 0 {
				return 0, false
			}
		}
		r := mk.P1 + nKey + packed
		return r, packed >= 0 && nKey+packed < mk.P2
	}
	fullOf := func(reg int) int {
		packed := reg - mk.P1 - nKey
		if unpack != nil {
			if packed < len(unpack.at) {
				return unpack.at[packed]
			}
			return unpack.nCols + packed - len(unpack.at)
		}
		return packed
	}
	if seqReg < mk.P1+nKey || seqReg >= mk.P1+mk.P2 || fullOf(seqReg) != nCols+len(plan.scopes) {
		return false
	}
	// The drain's registers before its step, traced back through the row the
	// body built to the columns they come from: the straight run of
	// instructions just before the step, from the jump target every way
	// into the step passes through.
	targets := map[int]bool{}
	for _, ins := range in {
		if segJoinJumpsP2(ins.Op) {
			targets[ins.P2] = true
		}
	}
	// The run: from the earliest jump target before the step such that
	// everything from it to the step is a read of the row or what a trace
	// follows, a CASE's jumps included, and nothing outside jumps into it
	// past its start.
	runAt := -1
	for t := stepAt; t > closeAt && (segJoinTraceable(in[t].Op) || segJoinIsJump(in[t]) || t == stepAt); t-- {
		if !targets[t] {
			continue
		}
		into := false
		for i, ins := range in {
			if (i < t || i >= stepAt) && (segJoinJumpsP2(ins.Op) || segJoinIsJump(ins)) && ins.P2 > t && ins.P2 < stepAt {
				into = true
			}
		}
		if !into {
			runAt = t
		}
	}
	if runAt < 0 {
		return false
	}
	tr := b.trace
	for i := runAt; i < stepAt; i++ {
		ins := in[i]
		if segJoinIsJump(ins) {
			end, ok := tr.region(i, stepAt)
			if !ok {
				return false
			}
			i = end - 1
			continue
		}
		if ins.Op == OpRecordColumn {
			if ins.P1 != rowRec {
				return false
			}
			r, ok := regOf(ins.P2)
			if !ok {
				return false
			}
			src, ok := writer[r]
			if !ok {
				return false
			}
			tr.from(i, ins.P3, src, tr.deps[r], l.cursors)
			continue
		}
		if !tr.step(i) {
			return false
		}
	}
	drainCol := func(reg int) (segJoinArg, bool) {
		a, ok := tr.src[reg]
		return a, ok
	}
	args, ok := segJoinAggArgs(plan, drainCol)
	if !ok {
		return false
	}
	// The one GROUP BY key: the record the drain compares groups by.
	var key segJoinArg
	found := false
	for i := closeAt; i < stepAt; i++ {
		if ins := in[i]; ins.Op == OpMakeRecord && ins.P2 == 1 && i > 0 && in[i-1].Op == OpRecordColumn &&
			in[i-1].P1 == rowRec && in[i-1].P3 == ins.P1 {
			if found {
				return false
			}
			r, ok := regOf(in[i-1].P2)
			if !ok {
				return false
			}
			if key, ok = writer[r]; !ok {
				return false
			}
			compared := false
			for _, g := range in {
				if g.Op == OpGroupSame && g.P1 == ins.P3 {
					// Its keys' collations, nil when BINARY: the only one the
					// kernel groups by.
					colls, _ := g.P4.([]string)
					for _, c := range colls {
						if c != "" && !strings.EqualFold(c, "BINARY") {
							return false
						}
					}
					compared = true
				}
			}
			if !compared {
				return false
			}
			found = true
		}
	}
	if !found {
		return false
	}
	seqAt := fullOf(seqReg)

	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, filters: l.filters, optional: l.outer, args: args, agg: plan, seg: step.P2,
		grouped: true, key: key, loads: loads, orders: l.orders}
	out := append([]Instruction(nil), in...)
	guard := len(out)
	out[l.head] = Instruction{Op: OpGoto, P2: guard}
	// guard: the kernel, or the loop as it was.
	out = append(out,
		Instruction{Op: OpSegJoinGroups, P2: guard + 3, P4: jp},
		in[l.head],
		Instruction{Op: OpGoto, P2: l.head + 1})
	// guard+3: one row per group through the body's own copies and hand-off.
	top := len(out)
	out = append(out, Instruction{Op: OpSegJoinNext, P2: closeAt, P4: jp})
	out = append(out, copies...)
	out = append(out, Instruction{Op: OpGoto, P2: top})
	// The drain's step, then the group's other rows.
	rest := len(out)
	out[stepAt] = Instruction{Op: OpGoto, P2: rest}
	out = append(out, step,
		Instruction{Op: OpSegJoinRest, P1: rowRec, P2: seqAt, P4: jp},
		Instruction{Op: OpGoto, P2: stepAt + 1})
	prog.Insns = out
	return true
}

// segJoinStepArgs is the source of each aggregate's argument register.
func segJoinStepArgs(plan *aggPlan, src map[int]segJoinArg) ([]segJoinArg, bool) {
	return segJoinAggArgs(plan, func(reg int) (segJoinArg, bool) {
		a, ok := src[reg]
		return a, ok
	})
}

// segJoinStepRegs is every register aggregate it's step reads: its
// argument, separator and FILTER, and its own ORDER BY's keys.
func segJoinStepRegs(it *aggItem) []int {
	var regs []int
	for _, which := range []int{aggExprArg, aggExprSep, aggExprFilter} {
		if r := it.rowRegs[which] - 1; r >= 0 {
			regs = append(regs, r)
		}
	}
	for _, r := range it.obRegs {
		if r > 0 {
			regs = append(regs, r-1)
		}
	}
	return regs
}

// segJoinAggArgs is the source of every register the aggregates step from --
// each one's argument and, for group_concat and its kin, separator -- found
// by lookup, each register once.
func segJoinAggArgs(plan *aggPlan, lookup func(reg int) (segJoinArg, bool)) ([]segJoinArg, bool) {
	var args []segJoinArg
	seen := map[int]bool{}
	for _, it := range aggPlanTemplates(plan) {
		for _, reg := range segJoinStepRegs(it) {
			a, ok := lookup(reg)
			if !ok {
				return nil, false
			}
			// A value written under a truth is read only by the aggregate
			// whose FILTER is that truth, which reads it under it.
			if a.guard != 0 && (it.filter == nil || it.rowRegs[aggExprFilter] != a.guard || reg == a.guard-1) {
				return nil, false
			}
			if seen[reg] {
				continue
			}
			a.reg = reg
			args = append(args, a)
			seen[reg] = true
		}
	}
	return args, true
}

// segJoinSameArgs reports whether two bodies' arguments come from the same
// columns. An expression is compared by being the same one, which two
// bodies never share: a LEFT JOIN over one declines.
func segJoinSameArgs(a, b []segJoinArg) bool {
	return slices.EqualFunc(a, b, func(x, y segJoinArg) bool {
		return x.expr == nil && y.expr == nil && x.reg == y.reg && x.level == y.level && x.col == y.col
	})
}

// segJoinTrace follows a loop body's registers to where their values come
// from: a level's column, a constant, or an expression over one level's
// columns and constants (segJoinExpr), which the kernel computes once per row
// of that level's table by running the body's own instructions.
type segJoinTrace struct {
	in      []Instruction
	levelOf map[int]int
	src     map[int]segJoinArg // register -> its source
	deps    map[int][]int      // register -> the instructions computing it, ascending
	// stand is an instruction an expression runs in place of the one at its
	// address: a column read, or a copy, for a drain's read of the row the
	// body built (from).
	stand map[int]Instruction
}

func newSegJoinTrace(in []Instruction, levelOf map[int]int) *segJoinTrace {
	return &segJoinTrace{in: in, levelOf: levelOf, src: map[int]segJoinArg{}, deps: map[int][]int{}, stand: map[int]Instruction{}}
}

// step follows instruction pc, reporting false for one it cannot: a jump, a
// function whose value may differ between calls, an expression over two
// levels' columns or over a register it has not seen written.
func (tr *segJoinTrace) step(pc int) bool {
	ins := tr.in[pc]
	switch ins.Op {
	case OpColumn, OpRowid:
		lv, c, ok := segJoinColRead(ins, tr.levelOf)
		if !ok {
			return false
		}
		r := segJoinColDest(ins)
		tr.src[r], tr.deps[r] = segJoinArg{reg: r, level: lv, col: c}, []int{pc}
		return true
	case OpSCopy, OpCopy:
		src, ok := tr.src[ins.P1]
		if !ok {
			return false
		}
		src.reg = ins.P2
		// The copy is part of computing its register: an expression reading
		// it runs the copy too.
		tr.src[ins.P2], tr.deps[ins.P2] = src, append(slices.Clone(tr.deps[ins.P1]), pc)
		return true
	case OpFunction:
		name, isName := ins.P4.(string)
		if !isName || name == "rtreecheck" || name == "vec1_config" || nonConstantFuncs[r33sFoldIdent(name)] {
			return false
		}
	}
	reads, writes, jump, ok := segInsnRegs(ins)
	if !ok || jump || len(writes) != 1 {
		return false
	}
	level := -1
	var deps []int
	for _, r := range reads {
		src, ok := tr.src[r]
		if !ok {
			return false
		}
		if src.level >= 0 {
			if level >= 0 && level != src.level {
				return false
			}
			level = src.level
		}
		deps = append(deps, tr.deps[r]...)
	}
	deps = append(deps, pc)
	slices.Sort(deps)
	deps = slices.Compact(deps)
	tr.src[writes[0]] = segJoinArg{reg: writes[0], level: level, expr: tr.expr(deps, writes[0])}
	tr.deps[writes[0]] = deps
	return true
}

// expr is the expression the instructions at deps compute into out.
func (tr *segJoinTrace) expr(deps []int, out int) *segJoinExpr {
	e := &segJoinExpr{out: out, pcs: slices.Clone(deps)}
	for _, d := range deps {
		ins, ok := tr.stand[d]
		if !ok {
			ins = tr.in[d]
		}
		e.insns = append(e.insns, ins)
	}
	return e
}

// writtenOnEveryPath reports whether every path from pc to the instruction
// at end (exclusive) writes register r. Every jump in the run goes forward,
// so one pass in address order settles each instruction: written on entry
// when written on leaving every instruction that reaches it.
func (tr *segJoinTrace) writtenOnEveryPath(pc, end, r int) bool {
	in := make([]int8, end-pc+1) // per instruction and end: 1 written on entry, -1 not, 0 unreached
	set := func(at int, written bool) {
		v := int8(1)
		if !written {
			v = -1
		}
		if i := at - pc; i >= 0 && i < len(in) && (in[i] == 0 || in[i] == 1 && v == -1) {
			in[i] = v
		}
	}
	in[0] = -1
	for i := pc; i < end; i++ {
		if in[i-pc] == 0 {
			continue // no path reaches it
		}
		ins := tr.in[i]
		out := in[i-pc] == 1
		if !out {
			if _, writes, _, ok := segInsnRegs(ins); ok && slices.Contains(writes, r) {
				out = true
			} else if (ins.Op == OpColumn || ins.Op == OpRowid) && segJoinColDest(ins) == r {
				out = true
			}
		}
		if segJoinIsJump(ins) && ins.P2 <= end {
			set(ins.P2, out) // a jump past end reaches it no way
		}
		if ins.Op != OpGoto {
			set(i+1, out)
		}
	}
	return in[end-pc] == 1
}

// segJoinIsJump reports whether ins may jump: a test, a comparison that
// jumps rather than stores, a Goto.
func segJoinIsJump(ins Instruction) bool {
	switch ins.Op {
	case OpIf, OpIfNot, OpIsNull, OpNotNull, OpGoto:
		return true
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		return ins.P5&p5StoreP2 == 0
	}
	return false
}

// region follows the run of instructions from the jump at pc to where its
// jumps all meet again -- a CASE, an IFNULL -- as one expression: every jump
// in it forward and inside it or to its end, before limit, and no jump from
// outside into its middle. Every register the run writes gets the run, and
// what its inputs need, as its expression. It returns the address after the
// run.
func (tr *segJoinTrace) region(pc, limit int) (int, bool) {
	end := pc + 1
	for i := pc; i < end; i++ {
		if i >= limit {
			return 0, false
		}
		if ins := tr.in[i]; segJoinIsJump(ins) {
			if ins.P2 <= i || ins.P2 > limit {
				return 0, false
			}
			end = max(end, ins.P2)
		}
		end = max(end, i+1)
	}
	for i, ins := range tr.in {
		if (i < pc || i >= end) && (segJoinJumpsP2(ins.Op) || segJoinIsJump(ins)) && ins.P2 > pc && ins.P2 < end {
			return 0, false
		}
	}
	level := -1
	var deps []int
	written := map[int]bool{}
	firstWrite := map[int]int{}
	var outs []int
	for i := pc; i < end; i++ {
		ins := tr.in[i]
		if ins.Op == OpGoto {
			deps = append(deps, i)
			continue
		}
		if ins.Op == OpFunction {
			name, isName := ins.P4.(string)
			if !isName || name == "rtreecheck" || name == "vec1_config" || nonConstantFuncs[r33sFoldIdent(name)] {
				return 0, false
			}
		}
		var reads, writes []int
		switch ins.Op {
		case OpColumn, OpRowid:
			lv, _, ok := segJoinColRead(ins, tr.levelOf)
			if !ok || level >= 0 && lv != level {
				return 0, false
			}
			level, writes = lv, []int{segJoinColDest(ins)}
		default:
			var ok bool
			if reads, writes, _, ok = segInsnRegs(ins); !ok {
				return 0, false
			}
		}
		for _, r := range reads {
			// A register written before the run carries that value into
			// every path that does not write it again: its definition is
			// part of the expression whether or not the run writes it too.
			src, ok := tr.src[r]
			if !ok {
				// Only the run's own write, which must then be on every path
				// to this read: no jump from before it lands past it.
				if _, mine := firstWrite[r]; !mine || !tr.writtenOnEveryPath(pc, i, r) {
					return 0, false
				}
				continue
			}
			if src.level >= 0 {
				if level >= 0 && level != src.level {
					return 0, false
				}
				level = src.level
			}
			deps = append(deps, tr.deps[r]...)
		}
		for _, w := range writes {
			if !written[w] {
				outs = append(outs, w)
				firstWrite[w] = i
			}
			written[w] = true
		}
		deps = append(deps, i)
	}
	// A register the run writes on only some paths keeps, on the others, the
	// value it had before: that definition is part of the expression too. One
	// with none, not written on every path, would read whatever the register
	// last held -- another row's value -- so it is left untraced, and
	// anything that reads it declines.
	var partial []int
	for _, o := range outs {
		if src, ok := tr.src[o]; ok {
			if src.level >= 0 {
				if level >= 0 && level != src.level {
					return 0, false
				}
				level = src.level
			}
			deps = append(deps, tr.deps[o]...)
		} else if !tr.writtenOnEveryPath(pc, end, o) {
			partial = append(partial, o)
		}
	}
	slices.Sort(deps)
	deps = slices.Compact(deps)
	// A run that is "if not G, skip to the end" over the rest: what the rest
	// writes on every path is written exactly when G is true.
	guard := 0
	if first := tr.in[pc]; first.Op == OpIfNot && first.P2 == end && first.P3 != 0 {
		guard = first.P1 + 1
	}
	for _, o := range outs {
		if slices.Contains(partial, o) {
			if guard == 0 || !tr.writtenOnEveryPath(pc+1, end, o) {
				delete(tr.src, o)
				delete(tr.deps, o)
				continue
			}
			tr.src[o] = segJoinArg{reg: o, level: level, expr: tr.expr(deps, o), guard: guard}
			tr.deps[o] = deps
			continue
		}
		tr.src[o] = segJoinArg{reg: o, level: level, expr: tr.expr(deps, o)}
		tr.deps[o] = deps
	}
	return end, true
}

// from follows the RecordColumn at pc, a drain's read into register dst of
// the row the body built, whose value src is: the read stands for a read of
// src's column, or for a copy of src's expression.
func (tr *segJoinTrace) from(pc, dst int, src segJoinArg, srcDeps []int, cursors []int) {
	src.reg = dst
	switch {
	case src.expr != nil:
		tr.stand[pc] = Instruction{Op: OpSCopy, P1: src.expr.out, P2: dst}
		deps := append(slices.Clone(srcDeps), pc)
		src.expr = tr.expr(deps, dst)
		tr.deps[dst] = deps
	case src.col < 0:
		tr.stand[pc] = Instruction{Op: OpRowid, P1: cursors[src.level], P2: dst}
		tr.deps[dst] = []int{pc}
	default:
		tr.stand[pc] = Instruction{Op: OpColumn, P1: cursors[src.level], P2: src.col, P3: dst}
		tr.deps[dst] = []int{pc}
	}
	tr.src[dst] = src
}

// parseSegJoinStepBody reads a body of argument loads -- a column read and
// its copy into the argument's register -- up to its OpAggStep, returning the
// arguments and the step's address.
func parseSegJoinStepBody(in []Instruction, pc, limit int, levelOf map[int]int) (map[int]segJoinArg, int, bool) {
	tr := newSegJoinTrace(in, levelOf)
	for ; pc < limit && in[pc].Op != OpAggStep; pc++ {
		if segJoinIsJump(in[pc]) {
			end, ok := tr.region(pc, limit)
			if !ok {
				return nil, 0, false
			}
			pc = end - 1
			continue
		}
		if !tr.step(pc) {
			return nil, 0, false
		}
	}
	return tr.src, pc, pc < limit
}

// parseSegJoinHashBody reads a hashed GROUP BY's body: column loads and
// copies, the key's record, up to its OpHashAggStep, returning the column
// each register holds, the key record and register, and the step's address.
func parseSegJoinHashBody(in []Instruction, pc, limit int, levelOf map[int]int) (writer map[int]segJoinArg, keyRec, keyReg, step int, ok bool) {
	tr := newSegJoinTrace(in, levelOf)
	keyRec, keyReg = -1, -1
	for ; pc < limit && in[pc].Op != OpHashAggStep; pc++ {
		if segJoinIsJump(in[pc]) {
			end, ok := tr.region(pc, limit)
			if !ok {
				return nil, 0, 0, 0, false
			}
			pc = end - 1
			continue
		}
		if ins := in[pc]; ins.Op == OpMakeRecord {
			if keyRec >= 0 || ins.P2 != 1 {
				return nil, 0, 0, 0, false
			}
			keyRec, keyReg = ins.P3, ins.P1
		} else if !tr.step(pc) {
			return nil, 0, 0, 0, false
		}
	}
	return tr.src, keyRec, keyReg, pc, pc < limit && keyRec >= 0
}

// segJoinRowBody is a loop body that builds a row and hands it on: the
// columns it loads, its other instructions, the column each register holds,
// the scan sequence number's register, and the record.
type segJoinRowBody struct {
	trace  *segJoinTrace
	loads  []segJoinArg
	copies []Instruction
	writer map[int]segJoinArg
	seqReg int
	rec    int
	mk     Instruction
}

// parseSegJoinRowBody reads a body from pc up to its hand-off (a Gosub or a
// SorterInsert), returning the hand-off's address.
func parseSegJoinRowBody(in []Instruction, pc, limit int, levelOf map[int]int) (*segJoinRowBody, int, bool) {
	tr := newSegJoinTrace(in, levelOf)
	b := &segJoinRowBody{trace: tr, writer: tr.src, seqReg: -1, rec: -1}
	for ; pc < limit; pc++ {
		ins := in[pc]
		switch {
		case ins.Op == OpColumn || ins.Op == OpRowid:
			if !tr.step(pc) {
				return nil, 0, false
			}
			b.loads = append(b.loads, tr.src[segJoinColDest(ins)])
			continue
		case ins.Op == OpSCopy && b.seqReg < 0 && pc+1 < limit && in[pc+1].Op == OpAdd &&
			in[pc+1].P2 == ins.P1 && in[pc+1].P3 == ins.P1:
			b.seqReg = ins.P2 // the sequence number, then the counter's increment
		case ins.Op == OpAdd && b.seqReg >= 0 && in[pc-1].Op == OpSCopy && in[pc-1].P2 == b.seqReg && in[pc-1].P1 == ins.P2:
		case ins.Op != OpClearSubtype && ins.Op != OpMakeRecord && ins.Op != OpGosub && ins.Op != OpSorterInsert:
			// An expression the drain's row reads: the body's copy computes
			// it again from the loaded columns.
			if !tr.step(pc) {
				return nil, 0, false
			}
		}
		switch ins.Op {
		case OpMakeRecord:
			if b.rec >= 0 {
				return nil, 0, false
			}
			b.rec, b.mk = ins.P3, ins
		case OpGosub, OpSorterInsert:
			b.copies = append(b.copies, ins)
			if b.rec < 0 || b.seqReg < 0 {
				return nil, 0, false
			}
			return b, pc, true
		}
		b.copies = append(b.copies, ins)
	}
	return nil, 0, false
}

// same reports whether body o builds the row b does: the same record over
// the same registers, each holding the same column, the same sequence
// number. The scratch registers a column passes through may differ.
func (b *segJoinRowBody) same(o *segJoinRowBody) bool {
	if b.seqReg != o.seqReg || b.rec != o.rec || !segJoinSameInsn(b.mk, o.mk) {
		return false
	}
	for r := b.mk.P1; r < b.mk.P1+b.mk.P2; r++ {
		x, okx := b.writer[r]
		y, oky := o.writer[r]
		if okx != oky || x.level != y.level || x.col != y.col || x.expr != nil || y.expr != nil {
			return false
		}
	}
	return true
}

// segJoinSameInsn reports whether two instructions are the same operation:
// opcode, operands and payload.
func segJoinSameInsn(a, b Instruction) bool {
	if a.Op != b.Op || a.P1 != b.P1 || a.P2 != b.P2 || a.P3 != b.P3 || a.P5 != b.P5 {
		return false
	}
	if a.P4 == nil || b.P4 == nil {
		return a.P4 == nil && b.P4 == nil
	}
	ta := reflect.TypeOf(a.P4)
	return ta == reflect.TypeOf(b.P4) && ta.Comparable() && a.P4 == b.P4
}

// segJoinFilter reads a term at pc that segParsePredBlock does not: a run of
// instructions a segJoinTrace follows, over level lv's columns, ending in an
// IfNot that skips the row to next when the value is false or NULL. It
// returns the value's source and the address after the IfNot.
func segJoinFilter(in []Instruction, pc, limit, lv int, levelOf map[int]int, next int) (segJoinArg, int, bool) {
	tr := newSegJoinTrace(in, levelOf)
	for i := pc; i < limit; i++ {
		ins := in[i]
		if segJoinIsJump(ins) && !(ins.Op == OpIfNot && ins.P2 == next) {
			end, ok := tr.region(i, limit)
			if !ok {
				return segJoinArg{}, 0, false
			}
			i = end - 1
			continue
		}
		if ins.Op == OpIfNot {
			src, ok := tr.src[ins.P1]
			if !ok || i == pc || ins.P2 != next || ins.P3 != 1 || src.level != lv && src.level >= 0 {
				return segJoinArg{}, 0, false
			}
			return src, i + 1, true
		}
		if ins.Op == OpColumn && levelOf[ins.P1] != lv || !tr.step(i) {
			return segJoinArg{}, 0, false
		}
	}
	return segJoinArg{}, 0, false
}

// segJoinIsRecheck reports whether a join's recheck starts at pc: two column
// reads and their equality's skip.
func segJoinIsRecheck(in []Instruction, pc int) bool {
	read := func(op OpCode) bool { return op == OpColumn || op == OpRowid }
	return read(in[pc].Op) && read(in[pc+1].Op) && in[pc+2].Op == OpEq && in[pc+3].Op == OpIfNot
}

// segJoinJumpsP2 reports whether op's P2 is a jump target.
func segJoinJumpsP2(op OpCode) bool {
	switch op {
	case OpGroupSame, OpGosub, OpSorterSort, OpSorterNext, OpIfEmpty, OpHashAggSort, OpHashAggNext, OpSorterCheck:
		return true
	}
	return segJumpsP2(op)
}

// segJoinTraceable reports whether a drain's run before its step may hold op:
// a read of the row, or what a segJoinTrace follows.
func segJoinTraceable(op OpCode) bool {
	switch op {
	case OpRecordColumn, OpSCopy, OpCopy, OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable,
		OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpNot, OpFunction,
		OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		return true
	}
	return false
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

// segJoinColDest is the register an OpColumn or OpRowid writes.
func segJoinColDest(in Instruction) int {
	if in.Op == OpRowid {
		return in.P2
	}
	return in.P3
}

// segJoinEqOK reports whether an OpEq is the plain equality the kernel
// answers over integers: its result stored, NULL not equal to NULL. Its
// affinity and collation cannot change how two integers compare.
func segJoinEqOK(eq Instruction) bool {
	return eq.P5&p5StoreP2 != 0 && eq.P5&(p5JumpIfNull|p5NullEq) == 0
}

// segJoinHashPeephole recognises a GROUP BY over the loop that the compiler
// hashed (compileScanGroupByHash): a body that loads the key and the
// aggregates' arguments into registers and steps OpHashAggStep, drained by
// OpHashAggSort. The kernel fills the buckets instead, one row per group
// stepped through hashAggStepRow and the rest added, as segGroupBulk does,
// and jumps to the drain. Nothing here reads a row, so the order a level
// visits its rows in cannot show.
func segJoinHashPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	open := 0
	for open < len(in) && in[open].Op != OpOpenRead {
		open++
	}
	if open >= len(in) || len(in) < 16 || in[0].Op != OpInit {
		return false
	}
	l, ok := parseSegJoinLoop(in, open)
	if !ok {
		return false
	}
	writer, keyRec, keyReg, pc, ok := parseSegJoinHashBody(in, l.body, len(in), l.levelOf)
	if !ok {
		return false
	}
	if pc >= len(in) || keyRec < 0 || in[pc].P1 != keyRec {
		return false
	}
	step := in[pc]
	plan, ok := step.P4.(*aggPlan)
	if !ok || plan == nil || plan.anchorCheck != nil || !aggPlanReadsOnlyLoweredColsExceptOrder(plan) {
		return false
	}
	key, ok := writer[keyReg]
	if !ok {
		return false
	}
	args, ok := segJoinAggArgs(plan, func(reg int) (segJoinArg, bool) {
		a, ok := writer[reg]
		return a, ok
	})
	if !ok {
		return false
	}
	closeAt, after, ok := l.tail(in, pc+1)
	if !ok || after >= len(in) || in[after].Op != OpHashAggSort {
		return false
	}
	if l.outer >= 0 {
		// The LEFT JOIN's null row: the same key and arguments, the same step.
		nw, nrec, nkey, end, ok := parseSegJoinHashBody(in, l.nullBody[0], l.nullBody[1], l.levelOf)
		if !ok || end+1 != l.nullBody[1] || nrec != keyRec || nkey != keyReg || !segJoinSameInsn(in[end], step) {
			return false
		}
		regs := []int{keyReg}
		for _, a := range args {
			regs = append(regs, a.reg)
		}
		for _, r := range regs {
			if x, y := writer[r], nw[r]; x.level != y.level || x.col != y.col || x.expr != nil || y.expr != nil {
				return false
			}
		}
	}
	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, filters: l.filters, optional: l.outer, args: args, agg: plan, seg: step.P2,
		grouped: true, key: key, orders: l.orders}
	key.reg = keyReg
	jp.key = key
	out := append([]Instruction(nil), in...)
	guard := len(out)
	out[l.head] = Instruction{Op: OpGoto, P2: guard}
	out = append(out,
		Instruction{Op: OpSegJoinHash, P2: closeAt, P4: jp},
		in[l.head],
		Instruction{Op: OpGoto, P2: l.head + 1})
	prog.Insns = out
	return true
}

// segJoinTopPeephole recognises an ORDER BY ... LIMIT over the loop: a body
// that loads the sort key and the output into a record, checks it against
// the bounded sorter (SorterCheck) and inserts it. The kernel hands the body
// only the joined rows that can be kept (segJoinTopOf), in the loop's order,
// through a copy of the body's own instructions; the sorter and its drain
// run as they did.
func segJoinTopPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 16 || in[0].Op != OpInit || in[1].Op != OpSorterOpen {
		return false
	}
	sorter := in[1].P1
	ki, ok := in[1].P4.(*sorterKeyInfo)
	if !ok || ki.bound <= 0 {
		return false
	}
	open := 2
	l, ok := parseSegJoinLoop(in, open)
	if !ok || l.outer >= 0 {
		return false
	}
	tr := newSegJoinTrace(in, l.levelOf)
	writer := tr.src
	var loads []segJoinArg
	var copies []Instruction
	var mk Instruction
	pc := l.body
	check := -1
body:
	for ; pc < len(in); pc++ {
		ins := in[pc]
		switch ins.Op {
		case OpColumn, OpRowid:
			if !tr.step(pc) {
				return false
			}
			loads = append(loads, tr.src[segJoinColDest(ins)])
			continue
		case OpSorterCheck:
			if check >= 0 || ins.P1 != sorter {
				return false
			}
			check = len(copies)
		case OpMakeRecord:
			if mk.Op == OpMakeRecord || check < 0 {
				return false
			}
			mk = ins
		case OpSorterInsert:
			if mk.Op != OpMakeRecord || ins.P1 != sorter || ins.P2 != mk.P3 {
				return false
			}
			copies = append(copies, ins)
			break body
		default:
			// An expression the record holds: the body's copy computes it
			// again from the loaded columns.
			if !tr.step(pc) {
				return false
			}
		}
		copies = append(copies, ins)
	}
	if pc >= len(in) || check < 0 {
		return false
	}
	closeAt, _, ok := l.tail(in, pc+1)
	if !ok || copies[check].P2 != pc+1 || copies[check].P3 != mk.P1 || mk.P2 < ki.nKey {
		return false // the check skips to the innermost Next, over the key
	}
	// The sort key's columns, and every value of the record a column.
	var order []segJoinArg
	for r := mk.P1; r < mk.P1+mk.P2; r++ {
		a, ok := writer[r]
		if !ok {
			return false
		}
		if r < mk.P1+ki.nKey {
			order = append(order, a)
		}
	}
	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, filters: l.filters, optional: l.outer, agg: nil, orders: l.orders,
		loads: loads, order: order, keys: ki}
	out := append([]Instruction(nil), in...)
	guard := len(out)
	out[l.head] = Instruction{Op: OpGoto, P2: guard}
	out = append(out,
		Instruction{Op: OpSegJoinTop, P2: guard + 3, P4: jp},
		in[l.head],
		Instruction{Op: OpGoto, P2: l.head + 1})
	// guard+3: one candidate at a time through the body's own copies, the
	// check skipping back here rather than into the loop.
	top := len(out)
	out = append(out, Instruction{Op: OpSegJoinNext, P2: closeAt, P4: jp})
	for i, ins := range copies {
		if i == check {
			ins.P2 = top
		}
		out = append(out, ins)
	}
	out = append(out, Instruction{Op: OpGoto, P2: top})
	prog.Insns = out
	return true
}
