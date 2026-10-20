package engine

import "strings"

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
	sorted  bool // an AutoIndexOrder reorders some level
	// preds is each level's own WHERE terms, tested right after its Rewind:
	// a column against a constant, which skip the level's row.
	preds [][]segPlanPred
	// head is the first instruction after the opens -- the prologue's, or the
	// first Rewind -- where a guard goes, before anything reads a table.
	head    int
	rewind0 int
	body    int // the first instruction of the body
}

// parseSegJoinLoop reads the loop's head from the first OpenRead at open:
// the opens, the prologue, the levels and their rechecks, up to the body.
func parseSegJoinLoop(in []Instruction, open int) (*segJoinLoop, bool) {
	l := &segJoinLoop{opened: map[int]bool{}, levelOf: map[int]int{}, ifNots: map[int]int{}}
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
			l.sorted = true
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
		preds, end, ok := segParsePredBlock(in, pc, limit, cursor, next)
		if !ok {
			return 0, false
		}
		l.preds = append(l.preds, preds)
		return end, true
	}
	if pc, ok = levelPreds(pc, len(in)); !ok {
		return nil, false
	}
	for len(l.cursors) < k {
		if pc+2 < len(in) && in[pc].Op == OpColumn &&
			(in[pc+1].Op == OpSeekRowidHint || in[pc+1].Op == OpSeekIndexHint) &&
			in[pc+1].P2 == in[pc].P3 && in[pc+1].P3 == 1 && in[pc+2].Op == OpRewind && in[pc+2].P1 == in[pc+1].P1 {
			if _, ok := l.levelOf[in[pc].P1]; !ok {
				return nil, false // the key comes from a level not entered yet
			}
			pc += 2
		}
		if pc+4 >= len(in) || in[pc].Op != OpRewind || !add(in[pc].P1, pc) {
			return nil, false
		}
		self := l.levelOf[in[pc].P1]
		// The terms end where the join's recheck begins: two column reads
		// and their equality, which would otherwise read as a term.
		recheck := pc + 1
		for recheck+3 < len(in) && !segJoinIsRecheck(in, recheck) {
			recheck++
		}
		end, ok := levelPreds(pc+1, recheck)
		if !ok || end != recheck || end+3 >= len(in) {
			return nil, false
		}
		pc = end - 1 // so the recheck reads from pc+1
		// The recheck: two column reads, their equality, and the skip.
		ca, cb, eq, skip := in[pc+1], in[pc+2], in[pc+3], in[pc+4]
		la, aCol, okA := segJoinColRead(ca, l.levelOf)
		lb, bCol, okB := segJoinColRead(cb, l.levelOf)
		ra, rb := segJoinColDest(ca), segJoinColDest(cb)
		if !okA || !okB || eq.Op != OpEq || !segJoinEqOK(eq) ||
			!(eq.P1 == ra && eq.P3 == rb || eq.P1 == rb && eq.P3 == ra) ||
			skip.Op != OpIfNot || skip.P1 != eq.P2 || skip.P3 != 1 || ra == rb {
			return nil, false
		}
		if (la == self) == (lb == self) {
			return nil, false // one side must be this level, the other an outer one
		}
		l.edges = append(l.edges, segJoinEdge{a: la, aCol: aCol, b: lb, bCol: bCol})
		l.ifNots[pc+4] = self
		pc += 5
	}
	l.body = pc
	return l, true
}

// tail checks the Next chain at pc, innermost first, each back to its level's
// body, with every skip and empty-level jump landing on the right one, and
// then the Closes. It returns the first Close and the address after them.
func (l *segJoinLoop) tail(in []Instruction, pc int) (closeAt, after int, ok bool) {
	k := len(l.cursors)
	for i := k - 1; i >= 0; i-- {
		if pc >= len(in) || in[pc].Op != OpNext || in[pc].P1 != l.cursors[i] {
			return 0, 0, false
		}
		// A level's Next goes back to just after its Rewind.
		if in[pc].P2 != l.rewinds[i]+1 {
			return 0, 0, false
		}
		if i+1 < k && in[l.rewinds[i+1]].P2 != pc {
			return 0, 0, false
		}
		for at, lv := range l.ifNots {
			if lv == i && in[at].P2 != pc {
				return 0, 0, false
			}
		}
		pc++
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
	pc := l.body
	var args []segJoinArg
	loaded := map[int]segJoinArg{}
	for pc < len(in) && in[pc].Op != OpAggStep {
		switch in[pc].Op {
		case OpColumn, OpRowid:
			lv, c, ok := segJoinColRead(in[pc], l.levelOf)
			if !ok {
				return false
			}
			loaded[segJoinColDest(in[pc])] = segJoinArg{reg: segJoinColDest(in[pc]), level: lv, col: c}
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
	closeAt, pc, ok := l.tail(in, pc+1)
	if !ok {
		return false
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
	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, args: args, agg: plan, seg: step.P2, sorted: l.sorted}
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
	var loads []segJoinArg
	var copies []Instruction
	writer := map[int]segJoinArg{} // register -> the column it holds
	seqReg, rec := -1, -1
	var mk Instruction
	pc := l.body
body:
	for ; pc < len(in); pc++ {
		ins := in[pc]
		switch ins.Op {
		case OpColumn, OpRowid:
			lv, c, ok := segJoinColRead(ins, l.levelOf)
			if !ok {
				return false
			}
			a := segJoinArg{reg: segJoinColDest(ins), level: lv, col: c}
			loads = append(loads, a)
			writer[a.reg] = a
			continue
		case OpSCopy:
			if src, ok := writer[ins.P1]; ok {
				writer[ins.P2] = segJoinArg{reg: ins.P2, level: src.level, col: src.col}
			} else if pc+1 < len(in) && in[pc+1].Op == OpAdd && in[pc+1].P2 == ins.P1 && in[pc+1].P3 == ins.P1 && seqReg < 0 {
				seqReg = ins.P2 // the sequence number, then the counter's increment
			} else {
				return false
			}
		case OpAdd:
			if seqReg < 0 || in[pc-1].Op != OpSCopy || in[pc-1].P2 != seqReg {
				return false
			}
		case OpClearSubtype:
		case OpMakeRecord:
			if rec >= 0 {
				return false
			}
			rec, mk = ins.P3, ins
		case OpGosub, OpSorterInsert:
		default:
			return false
		}
		copies = append(copies, ins)
		if ins.Op == OpGosub || ins.Op == OpSorterInsert {
			break body
		}
	}
	if pc >= len(in) || rec < 0 || seqReg < 0 {
		return false
	}
	hand := in[pc]
	closeAt, _, ok := l.tail(in, pc+1)
	if !ok {
		return false
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

	// A level visited in an index's order makes another row the group's
	// first. That shows only where something reads it -- a bare column, a
	// subquery over the group's row -- or the order of the groups themselves,
	// which a streamed drain emits as they come and a sorted one sorts.
	if l.sorted && (hand.Op == OpGosub || !aggPlanReadsOnlyLoweredColsExceptOrder(plan)) {
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
	// A register the drain fills from the row, traced back to its column
	// through the straight run of reads and copies just before the step,
	// which every way into the step passes through.
	drainCol := func(reg int) (segJoinArg, bool) {
		for i := stepAt - 1; i > closeAt && (in[i].Op == OpRecordColumn || in[i].Op == OpSCopy); i-- {
			ins := in[i]
			switch {
			case ins.Op == OpSCopy && ins.P2 == reg:
				reg = ins.P1
			case ins.Op == OpRecordColumn && ins.P3 == reg:
				if ins.P1 != rowRec {
					return segJoinArg{}, false
				}
				r, ok := regOf(ins.P2)
				if !ok {
					return segJoinArg{}, false
				}
				w, ok := writer[r]
				return w, ok
			}
		}
		return segJoinArg{}, false
	}
	var args []segJoinArg
	for _, it := range aggPlanTemplates(plan) {
		if it.kind == aggCountStar {
			continue
		}
		reg := it.rowRegs[aggExprArg] - 1
		a, ok := drainCol(reg)
		if !ok {
			return false
		}
		a.reg = reg
		args = append(args, a)
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

	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, args: args, agg: plan, seg: step.P2,
		grouped: true, key: key, loads: loads, sorted: l.sorted}
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

// segJoinIsRecheck reports whether a join's recheck starts at pc: two column
// reads and their equality's skip.
func segJoinIsRecheck(in []Instruction, pc int) bool {
	read := func(op OpCode) bool { return op == OpColumn || op == OpRowid }
	return read(in[pc].Op) && read(in[pc+1].Op) && in[pc+2].Op == OpEq && in[pc+3].Op == OpIfNot
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
	writer := map[int]segJoinArg{}
	keyRec, keyReg := -1, -1
	pc := l.body
	for ; pc < len(in) && in[pc].Op != OpHashAggStep; pc++ {
		ins := in[pc]
		switch ins.Op {
		case OpColumn, OpRowid:
			lv, c, ok := segJoinColRead(ins, l.levelOf)
			if !ok {
				return false
			}
			r := segJoinColDest(ins)
			writer[r] = segJoinArg{reg: r, level: lv, col: c}
		case OpSCopy:
			src, ok := writer[ins.P1]
			if !ok {
				return false
			}
			writer[ins.P2] = segJoinArg{reg: ins.P2, level: src.level, col: src.col}
		case OpMakeRecord:
			if keyRec >= 0 || ins.P2 != 1 {
				return false
			}
			keyRec, keyReg = ins.P3, ins.P1
		default:
			return false
		}
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
	var args []segJoinArg
	for _, it := range aggPlanTemplates(plan) {
		if it.kind == aggCountStar {
			continue
		}
		reg := it.rowRegs[aggExprArg] - 1
		a, ok := writer[reg]
		if !ok {
			return false
		}
		args = append(args, a)
	}
	closeAt, after, ok := l.tail(in, pc+1)
	if !ok || after >= len(in) || in[after].Op != OpHashAggSort {
		return false
	}
	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, args: args, agg: plan, seg: step.P2,
		grouped: true, key: key, sorted: l.sorted}
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
