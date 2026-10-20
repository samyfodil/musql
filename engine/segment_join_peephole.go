package engine

import (
	"maps"
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
	// crosses is each level's terms over it and earlier levels, true or the
	// row is skipped; tags its MULTI_OR order, nil for none.
	crosses [][]*segJoinExpr
	tags    []*segJoinTags
	// head is the first instruction after the opens -- the prologue's, or the
	// first Rewind -- where a guard goes, before anything reads a table.
	head    int
	rewind0 int
	body    int // the first instruction of the body
	// outers has bit i set for a level a LEFT JOIN makes optional, whose
	// match flag is register flags[i]. nullBodies is every copy of the body
	// the loop runs for a null row -- its first instruction and the address
	// after its last -- each read by tail.
	outers     uint64
	flags      map[int]int
	nullBodies [][2]int
}

// parseSegJoinLoop reads the loop's head from the first OpenRead at open:
// the opens, the prologue, the levels and their rechecks, up to the body.
func parseSegJoinLoop(in []Instruction, open int) (*segJoinLoop, bool) {
	l := &segJoinLoop{opened: map[int]bool{}, levelOf: map[int]int{}, ifNots: map[int]int{}, flags: map[int]int{}}
	pc := open
	var ok bool
	for pc < len(in) && in[pc].Op == OpOpenRead {
		l.opened[in[pc].P1] = true
		pc++
	}
	k := len(l.opened)
	if k < 2 || k > 64 || pc-open != k {
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
	if !l.add(in[pc].P1, pc) {
		return nil, false
	}
	pc++
	if pc, ok = l.levelPreds(in, pc, len(in)); !ok {
		return nil, false
	}
	for len(l.cursors) < k {
		if pc, ok = l.level(in, pc); !ok {
			return nil, false
		}
	}
	l.body = pc
	return l, true
}

// add enters cursor, rewound at rewind, as the next level.
func (l *segJoinLoop) add(cursor, rewind int) bool {
	if !l.opened[cursor] {
		return false
	}
	if _, dup := l.levelOf[cursor]; dup {
		return false
	}
	l.levelOf[cursor] = len(l.cursors)
	l.cursors = append(l.cursors, cursor)
	l.rewinds = append(l.rewinds, rewind)
	l.tags = append(l.tags, nil)
	return true
}

// levelPreds reads the last level's own WHERE terms, right after its
// Rewind, each skipping to its Next.
func (l *segJoinLoop) levelPreds(in []Instruction, pc, limit int) (int, bool) {
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
	var crosses []*segJoinExpr
	for {
		// A run the comparisons do not read is the filters' to read.
		if more, end, ok := segParsePredBlock(in, pc, limit, cursor, next); ok {
			preds, pc = append(preds, more...), end
		}
		if f, end, ok := segJoinFilter(in, pc, limit, lv, l.levelOf, next); ok {
			filters, pc = append(filters, f), end
			continue
		}
		x, end, ok := segJoinCrossFilter(in, pc, limit, l.levelOf, lv, next)
		if !ok {
			break
		}
		crosses, pc = append(crosses, x), end
	}
	l.preds = append(l.preds, preds)
	l.filters = append(l.filters, filters)
	l.crosses = append(l.crosses, crosses)
	return pc, true
}

// level reads the next level from pc: its seek, its Rewind, its recheck and
// its own terms, and returns the address after them.
func (l *segJoinLoop) level(in []Instruction, pc int) (int, bool) {
	// The seek's key and hint, before the Rewind (and a LEFT JOIN's flag).
	rewindAt := pc + 2
	if rewindAt+1 < len(in) && in[rewindAt].Op == OpInteger && in[rewindAt].P1 == 0 {
		rewindAt++
	}
	if rewindAt < len(in) && in[pc].Op == OpColumn &&
		(in[pc+1].Op == OpSeekRowidHint || in[pc+1].Op == OpSeekIndexHint) &&
		in[pc+1].P2 == in[pc].P3 && in[pc+1].P3 == 1 && in[rewindAt].Op == OpRewind && in[rewindAt].P1 == in[pc+1].P1 {
		if _, ok := l.levelOf[in[pc].P1]; !ok {
			return 0, false // the key comes from a level not entered yet
		}
		pc += 2
	}
	// A LEFT JOIN's level clears its match flag before its Rewind.
	flag := -1
	if pc+1 < len(in) && in[pc].Op == OpInteger && in[pc].P1 == 0 && in[pc+1].Op == OpRewind {
		flag = in[pc].P2
		pc++
	}
	// A MULTI_OR level's order, tagged per outer row before its Rewind.
	tags, pc, ok := parseSegJoinTags(in, pc, l.levelOf)
	if !ok {
		return 0, false
	}
	if pc+4 >= len(in) || in[pc].Op != OpRewind || !l.add(in[pc].P1, pc) {
		return 0, false
	}
	self := l.levelOf[in[pc].P1]
	if tags != nil && (tags.cursor != in[pc].P1 || flag >= 0) {
		return 0, false
	}
	l.tags[self] = tags
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
	eqAt, preds0, filters0, crosses0 := -1, len(l.preds), len(l.filters), len(l.crosses)
	var x, y segJoinArg
	for p := pc + 1; p+1 < len(in) && eqAt < 0; p++ {
		e, sk := in[p], in[p+1]
		if e.Op != OpEq || sk.Op != OpIfNot || sk.P1 != e.P2 ||
			!(sk.P3 == 1 && sk.P2 == next || flag >= 0 && sk.P3 == 0) || !segJoinEqOK(e) {
			continue
		}
		l.preds, l.filters, l.crosses = l.preds[:preds0], l.filters[:filters0], l.crosses[:crosses0]
		end, ok := l.levelPreds(in, pc+1, p)
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
		return 0, false
	}
	if flag >= 0 && len(l.crosses[self]) != 0 {
		return 0, false // an ON term over other levels decides the NULL row
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
			return 0, false
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
			return 0, false
		}
		l.outers |= 1 << self
		l.flags[self] = flag
		pc++
	}
	return pc, true
}

// nullLevels reads the levels after LEFT JOIN level o that its null row's
// copy, from pc to end, enters again. They must read as the loop's own do --
// the same links, the same terms, the same LEFT JOINs -- with a Next chain of
// their own, null rows' copies included, that leaves at end. Every copy of
// the body in it goes to l.nullBodies.
func (l *segJoinLoop) nullLevels(in []Instruction, pc, end, o int) bool {
	k := len(l.cursors)
	c := &segJoinLoop{opened: l.opened, levelOf: map[int]int{}, ifNots: map[int]int{}, flags: map[int]int{},
		outers:  l.outers & (1<<(o+1) - 1),
		cursors: slices.Clone(l.cursors[:o+1]), rewinds: slices.Clone(l.rewinds[:o+1]),
		preds: slices.Clone(l.preds[:o+1]), filters: slices.Clone(l.filters[:o+1]), edges: slices.Clone(l.edges[:o]),
		crosses: slices.Clone(l.crosses[:o+1]), tags: slices.Clone(l.tags[:o+1])}
	for i, cur := range c.cursors {
		c.levelOf[cur] = i
	}
	for len(c.cursors) < k {
		var ok bool
		if pc, ok = c.level(in, pc); !ok || pc > end {
			return false
		}
	}
	if c.outers != l.outers {
		return false
	}
	for i := o + 1; i < k; i++ {
		e, ce := l.edges[i-1], c.edges[i-1]
		if c.cursors[i] != l.cursors[i] || !reflect.DeepEqual(l.preds[i], c.preds[i]) ||
			!slices.EqualFunc(l.filters[i], c.filters[i], segJoinSameSrc) ||
			!slices.EqualFunc(l.crosses[i], c.crosses[i], segJoinSameExpr) || !segJoinSameTags(l.tags[i], c.tags[i]) ||
			e.a != ce.a || e.b != ce.b || !segJoinSameSrc(e.aArg, ce.aArg) || !segJoinSameSrc(e.bArg, ce.bArg) {
			return false
		}
	}
	// The copy's body, up to its innermost Next, then its Next chain.
	at := -1
	for i := pc; i < end; i++ {
		if in[i].Op == OpNext && in[i].P1 == c.cursors[k-1] && in[i].P2 == c.rewinds[k-1]+1 {
			at = i
			break
		}
	}
	if at < 0 {
		return false
	}
	l.nullBodies = append(l.nullBodies, [2]int{pc, at})
	q, ok := c.chain(in, at, o+1)
	if !ok || q != end {
		return false
	}
	l.nullBodies = append(l.nullBodies, c.nullBodies...)
	return true
}

// segJoinSameTags reports whether two MULTI_OR orders are the same.
func segJoinSameTags(a, b *segJoinTags) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.cursor == b.cursor && a.order == b.order && slices.EqualFunc(a.passes, b.passes, segJoinSameExpr)
}

// segJoinSameSrc reports whether two registers' sources are the same value:
// the same column of the same level, or the same expression.
func segJoinSameSrc(x, y segJoinArg) bool {
	return x.level == y.level && x.col == y.col && x.guard == y.guard && segJoinSameExpr(x.expr, y.expr)
}

// chain checks the Next chain at pc, from the innermost level down to level
// lo, each back to its level's body, with every skip and empty-level jump
// landing on the right one, and each LEFT JOIN's null row: skipped when its
// flag is set, else a NullRow and a copy of the levels after it and the body,
// up to the next outer level's Next. It returns the address after level lo's.
func (l *segJoinLoop) chain(in []Instruction, pc, lo int) (int, bool) {
	k := len(l.cursors)
	for i := k - 1; i >= lo; i-- {
		// A level's Next goes back to just after its Rewind.
		if pc >= len(in) || in[pc].Op != OpNext || in[pc].P1 != l.cursors[i] || in[pc].P2 != l.rewinds[i]+1 {
			return 0, false
		}
		for at, lv := range l.ifNots {
			if lv == i && in[at].P2 != pc {
				return 0, false
			}
		}
		pc++
		// An empty level goes on as its last row does.
		if in[l.rewinds[i]].P2 != pc {
			return 0, false
		}
		if l.outers>>i&1 == 0 {
			continue
		}
		if pc+1 >= len(in) || in[pc].Op != OpIf || in[pc].P1 != l.flags[i] ||
			in[pc+1].Op != OpNullRow || in[pc+1].P1 != l.cursors[i] {
			return 0, false
		}
		end := in[pc].P2
		if end <= pc+2 || end >= len(in) || in[end].Op != OpNext {
			return 0, false
		}
		if i == k-1 {
			l.nullBodies = append(l.nullBodies, [2]int{pc + 2, end})
		} else if !l.nullLevels(in, pc+2, end, i) {
			return 0, false
		}
		pc = end
	}
	return pc, true
}

// tail checks the Next chain at pc (chain) and then the Closes. It returns
// the first Close and the address after them.
func (l *segJoinLoop) tail(in []Instruction, pc int) (closeAt, after int, ok bool) {
	k := len(l.cursors)
	if pc, ok = l.chain(in, pc, 0); !ok {
		return 0, 0, false
	}
	closeAt = pc
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
	body, ok := parseSegJoinSteps(in, l.body, len(in), l.levelOf, OpAggStep)
	if !ok {
		return false
	}
	pc := body.last()
	step := in[body.at[0]]
	plan, ok := step.P4.(*aggPlan)
	if !ok || step.P3 != 0 || plan == nil || plan.anchorCheck != nil ||
		len(plan.orderPlans) != 0 || !aggPlanReadsOnlyLoweredColsExceptOrder(plan) {
		return false
	}
	args, ok := segJoinStepArgs(plan, body)
	if !ok {
		return false
	}
	closeAt, pc, ok := l.tail(in, pc+1)
	if !ok {
		return false
	}
	for _, nb := range l.nullBodies {
		// A LEFT JOIN's null row: the same arguments, the same step.
		nbody, ok := parseSegJoinSteps(in, nb[0], nb[1], l.levelOf, OpAggStep)
		if !ok || nbody.last()+1 != nb[1] || !body.same(in, nbody) {
			return false
		}
		nargs, ok := segJoinStepArgs(plan, nbody)
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
	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, filters: l.filters, crosses: l.crosses, tags: l.tags, optional: l.outers, args: args, agg: plan, seg: step.P2, segs: body.segs, orders: l.orders}
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
	for _, body := range l.nullBodies {
		// A LEFT JOIN's null row goes through a copy of the body that must
		// build the same row from the same values.
		nb, end, ok := parseSegJoinRowBody(in, body[0], body[1], l.levelOf)
		if !ok || end+1 != body[1] || !b.same(nb) || !segJoinSameInsn(in[end], hand) {
			return false
		}
	}

	// The drain's steps over the row the body built: one per segment of the
	// aggregate (a HAVING's own aggregates are another), the first's loads
	// a run the trace follows, each later one's straight after the last.
	var steps []int
	for i, ins := range in {
		if ins.Op == OpAggStep {
			if ins.P3 != 1 || len(steps) > 0 && (ins.P1 != in[steps[0]].P1 || ins.P4 != in[steps[0]].P4) {
				return false
			}
			steps = append(steps, i)
		}
	}
	if len(steps) == 0 {
		return false
	}
	stepAt := steps[0]
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
	readRow := func(i int) bool {
		ins := in[i]
		if ins.Op != OpRecordColumn {
			return tr.step(i)
		}
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
		return true
	}
	snaps := map[int]map[int]segJoinArg{step.P2: maps.Clone(tr.src)}
	segs := []int{step.P2}
	for k := 1; k < len(steps); k++ {
		// Nothing jumps into the run of a later step's loads.
		for t := steps[k-1] + 1; t <= steps[k]; t++ {
			if targets[t] {
				return false
			}
		}
		for i := steps[k-1] + 1; i < steps[k]; i++ {
			if segJoinIsJump(in[i]) || !readRow(i) {
				return false
			}
		}
		seg := in[steps[k]].P2
		if _, dup := snaps[seg]; dup {
			return false
		}
		snaps[seg], segs = maps.Clone(tr.src), append(segs, seg)
	}
	args, ok := segJoinAggArgs(plan, func(it *aggItem, reg int) (segJoinArg, bool) {
		a, ok := snaps[it.stepSeg][reg]
		return a, ok
	})
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

	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, filters: l.filters, crosses: l.crosses, tags: l.tags, optional: l.outers, args: args, agg: plan, seg: step.P2,
		segs: segs, grouped: true, key: key, loads: loads, orders: l.orders}
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
	// The drain's steps, the last followed by the group's other rows: each
	// segment takes the group's first row before them.
	last := steps[len(steps)-1]
	rest := len(out)
	out[last] = Instruction{Op: OpGoto, P2: rest}
	out = append(out, in[last],
		Instruction{Op: OpSegJoinRest, P1: rowRec, P2: seqAt, P4: jp},
		Instruction{Op: OpGoto, P2: last + 1})
	prog.Insns = out
	return true
}

// segJoinStepArgs is the source of each aggregate's argument register.
func segJoinStepArgs(plan *aggPlan, b *segJoinSteps) ([]segJoinArg, bool) {
	return segJoinAggArgs(plan, func(it *aggItem, reg int) (segJoinArg, bool) {
		a, ok := b.snaps[it.stepSeg][reg]
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
func segJoinAggArgs(plan *aggPlan, lookup func(it *aggItem, reg int) (segJoinArg, bool)) ([]segJoinArg, bool) {
	var args []segJoinArg
	seen := map[int]segJoinArg{}
	for _, it := range aggPlanTemplates(plan) {
		for _, reg := range segJoinStepRegs(it) {
			a, ok := lookup(it, reg)
			if !ok {
				return nil, false
			}
			// A value written under a truth is read only by the aggregate
			// whose FILTER is that truth, which reads it under it.
			if a.guard != 0 && (it.filter == nil || it.rowRegs[aggExprFilter] != a.guard || reg == a.guard-1) {
				return nil, false
			}
			if prev, dup := seen[reg]; dup {
				// Two steps reading one register read the same value.
				if !segJoinSameSrc(prev, a) {
					return nil, false
				}
				continue
			}
			a.reg = reg
			args = append(args, a)
			seen[reg] = a
		}
	}
	return args, true
}

// segJoinSameArgs reports whether two bodies' arguments are the same
// registers holding the same values.
func segJoinSameArgs(a, b []segJoinArg) bool {
	return slices.EqualFunc(a, b, func(x, y segJoinArg) bool {
		return x.reg == y.reg && segJoinSameSrc(x, y)
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
		level = segJoinMix(level, src.level)
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

// segJoinMulti is the level of a value over several levels: computed on a
// joined row, not once per row of one table.
const segJoinMulti = -2

// segJoinMix is the level of a value over one of level and one of src, -1
// being a constant's.
func segJoinMix(level, src int) int {
	switch {
	case src == -1 || src == level:
		return level
	case level == -1:
		return src
	}
	return segJoinMulti
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
			if !ok {
				return 0, false
			}
			level, writes = segJoinMix(level, lv), []int{segJoinColDest(ins)}
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
			level = segJoinMix(level, src.level)
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
			level = segJoinMix(level, src.level)
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

// segJoinSteps is a body's aggregate steps -- one per segment of the
// aggregate, each over the same row -- and, as of each, where every register
// came from.
type segJoinSteps struct {
	at    []int // each step's address
	segs  []int
	snaps map[int]map[int]segJoinArg // segment -> register -> source
	// keyRec and keyReg are a hashed GROUP BY's key record and register.
	keyRec, keyReg int
}

// last is the last step's address.
func (b *segJoinSteps) last() int { return b.at[len(b.at)-1] }

// same reports whether o steps the same plan's same segments the same way,
// from the same values.
func (b *segJoinSteps) same(in []Instruction, o *segJoinSteps) bool {
	if len(b.at) != len(o.at) || b.keyRec != o.keyRec || b.keyReg != o.keyReg {
		return false
	}
	for i := range b.at {
		if !segJoinSameInsn(in[b.at[i]], in[o.at[i]]) {
			return false
		}
	}
	return true
}

// parseSegJoinSteps reads a body from pc: loads and expressions up to a step
// (op), then any more, each a step of the same plan over another segment --
// a HAVING's own aggregate -- with the loads it reads before it. A hashed
// GROUP BY's body also makes its key record (MakeRecord), once, before the
// first step, which every step names.
func parseSegJoinSteps(in []Instruction, pc, limit int, levelOf map[int]int, op OpCode) (*segJoinSteps, bool) {
	tr := newSegJoinTrace(in, levelOf)
	b := &segJoinSteps{snaps: map[int]map[int]segJoinArg{}, keyRec: -1, keyReg: -1}
	for ; pc < limit; pc++ {
		ins := in[pc]
		switch {
		case ins.Op == op:
			if len(b.at) > 0 && (ins.P4 != in[b.at[0]].P4 || ins.P1 != in[b.at[0]].P1 || ins.P3 != in[b.at[0]].P3) {
				return nil, false
			}
			if _, dup := b.snaps[ins.P2]; dup {
				return nil, false
			}
			b.at, b.segs = append(b.at, pc), append(b.segs, ins.P2)
			b.snaps[ins.P2] = maps.Clone(tr.src)
			continue
		case segJoinIsJump(ins):
			if len(b.at) > 0 {
				return b, true // past the steps: the Next chain
			}
			end, ok := tr.region(pc, limit)
			if !ok {
				return nil, false
			}
			pc = end - 1
			continue
		case op == OpHashAggStep && ins.Op == OpMakeRecord && len(b.at) == 0:
			if b.keyRec >= 0 || ins.P2 != 1 {
				return nil, false
			}
			b.keyRec, b.keyReg = ins.P3, ins.P1
			continue
		}
		if !tr.step(pc) {
			if len(b.at) > 0 {
				return b, true
			}
			return nil, false
		}
	}
	return b, len(b.at) > 0
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
		if okx != oky || !segJoinSameSrc(x, y) {
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
			if !ok || i == pc || ins.P2 != next || ins.P3 != 1 || src.level != lv && src.level != -1 {
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
		OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat, OpNot, OpFunction, OpNegative, OpBitNot, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight,
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
	body, ok := parseSegJoinSteps(in, l.body, len(in), l.levelOf, OpHashAggStep)
	if !ok || body.keyRec < 0 {
		return false
	}
	pc := body.last()
	keyRec, keyReg := body.keyRec, body.keyReg
	writer := body.snaps[body.segs[0]]
	if in[pc].P1 != keyRec {
		return false
	}
	step := in[body.at[0]]
	plan, ok := step.P4.(*aggPlan)
	if !ok || plan == nil || plan.anchorCheck != nil || !aggPlanReadsOnlyLoweredColsExceptOrder(plan) {
		return false
	}
	key, ok := writer[keyReg]
	if !ok {
		return false
	}
	args, ok := segJoinStepArgs(plan, body)
	if !ok {
		return false
	}
	closeAt, after, ok := l.tail(in, pc+1)
	if !ok || after >= len(in) || in[after].Op != OpHashAggSort {
		return false
	}
	for _, nb := range l.nullBodies {
		// A LEFT JOIN's null row: the same key and arguments, the same step.
		nbody, ok := parseSegJoinSteps(in, nb[0], nb[1], l.levelOf, OpHashAggStep)
		if !ok || nbody.last()+1 != nb[1] || !body.same(in, nbody) {
			return false
		}
		x, okx := writer[keyReg]
		y, oky := nbody.snaps[nbody.segs[0]][keyReg]
		if !okx || !oky || !segJoinSameSrc(x, y) {
			return false
		}
		nargs, ok := segJoinStepArgs(plan, nbody)
		if !ok || !segJoinSameArgs(nargs, args) {
			return false
		}
	}
	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, filters: l.filters, crosses: l.crosses, tags: l.tags, optional: l.outers, args: args, agg: plan, seg: step.P2, segs: body.segs,
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
	if !ok {
		return false
	}
	b, ok := parseSegJoinTopBody(in, l.body, len(in), l.levelOf, sorter)
	if !ok {
		return false
	}
	writer, loads, copies, mk, check, pc := b.writer, b.loads, b.copies, b.mk, b.check, b.end
	closeAt, _, ok := l.tail(in, pc+1)
	if !ok || copies[check].P2 != pc+1 || copies[check].P3 != mk.P1 || mk.P2 < ki.nKey {
		return false // the check skips to the innermost Next, over the key
	}
	for _, body := range l.nullBodies {
		// A LEFT JOIN's null row goes through a copy of the body that must
		// put the same values in the same record.
		nb, ok := parseSegJoinTopBody(in, body[0], body[1], l.levelOf, sorter)
		if !ok || nb.end+1 != body[1] || !segJoinSameInsn(nb.mk, mk) ||
			nb.copies[nb.check].P2 != body[1] || nb.copies[nb.check].P3 != mk.P1 {
			return false
		}
		for r := mk.P1; r < mk.P1+mk.P2; r++ {
			x, okx := writer[r]
			y, oky := nb.writer[r]
			if !okx || !oky || !segJoinSameSrc(x, y) {
				return false
			}
		}
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
	jp := &segJoinPlan{cursors: l.cursors, edges: l.edges, preds: l.preds, filters: l.filters, crosses: l.crosses, tags: l.tags, optional: l.outers, agg: nil, orders: l.orders,
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

// Operand kinds, for comparing two copies of an expression.
const (
	segOpConst = iota // compared as is: a cursor, a column, a literal
	segOpReg          // a register, renamed between the copies
	segOpJump         // an address, at the same place in each copy
)

// segJoinOperandKinds is the kind of P1, P2 and P3 of an instruction a
// segJoinTrace may put in an expression; ok is false for any other.
func segJoinOperandKinds(in Instruction) (k [3]int, ok bool) {
	c, r, j := segOpConst, segOpReg, segOpJump
	switch in.Op {
	case OpColumn:
		return [3]int{c, c, r}, true
	case OpRowid, OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable:
		return [3]int{c, r, c}, true
	case OpSCopy, OpCopy, OpNot, OpNegative, OpBitNot:
		return [3]int{r, r, c}, true
	case OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		return [3]int{r, r, r}, true
	case OpFunction:
		return [3]int{r, c, r}, true // P2 arguments from P1: renamed as a run
	case OpLike, OpGlob, OpConcat, OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder:
		return [3]int{r, r, r}, true
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		if in.P5&p5StoreP2 != 0 {
			return [3]int{r, r, r}, true
		}
		return [3]int{r, j, r}, true
	case OpIf, OpIfNot, OpIsNull, OpNotNull:
		return [3]int{r, j, c}, true
	case OpGoto:
		return [3]int{c, j, c}, true
	}
	return k, false
}

// segJoinSameExpr reports whether a and b compute the same value: the same
// instructions in the same order, their registers renamed one to one (a
// register read before the copy writes it is the same register in both),
// their jumps landing at the same place in each.
func segJoinSameExpr(a, b *segJoinExpr) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.insns) != len(b.insns) || len(a.pcs) != len(b.pcs) {
		return false
	}
	ab, ba := map[int]int{}, map[int]int{}
	reg := func(x, y int) bool {
		if m, ok := ab[x]; ok {
			return m == y
		}
		if m, ok := ba[y]; ok {
			return m == x
		}
		ab[x], ba[y] = y, x
		return true
	}
	// Where a jump lands: an instruction of the copy by its place, or past
	// the last by the same distance.
	place := func(e *segJoinExpr, to int) int {
		if i := slices.Index(e.pcs, to); i >= 0 {
			return i
		}
		return len(e.pcs) + to - e.pcs[len(e.pcs)-1]
	}
	written := map[int]bool{}
	for i := range a.insns {
		x, y := a.insns[i], b.insns[i]
		kinds, ok := segJoinOperandKinds(x)
		if !ok || x.Op != y.Op || x.P5 != y.P5 {
			return false
		}
		if x.Op == OpLike {
			// P4 an escape register, or not one.
			ex, xr := x.P4.(int)
			ey, yr := y.P4.(int)
			if xr != yr || xr && (ex >= 0) != (ey >= 0) || xr && ex >= 0 && !reg(ex, ey) {
				return false
			}
		} else if !segJoinSameInsn(Instruction{Op: x.Op, P4: x.P4}, Instruction{Op: y.Op, P4: y.P4}) {
			return false
		}
		reads, writes, _, _ := segInsnRegs(x)
		if x.Op == OpRowid {
			writes = []int{x.P2}
		}
		px, py := [3]int{x.P1, x.P2, x.P3}, [3]int{y.P1, y.P2, y.P3}
		for f, kind := range kinds {
			switch kind {
			case segOpConst:
				if px[f] != py[f] {
					return false
				}
			case segOpJump:
				if place(a, px[f]) != place(b, py[f]) {
					return false
				}
			case segOpReg:
				n := 1
				if x.Op == OpFunction && f == 0 {
					n = x.P2 // the arguments, a run of registers
				}
				for d := range n {
					rx, ry := px[f]+d, py[f]+d
					if slices.Contains(reads, rx) && !written[rx] && rx != ry {
						return false // read from outside the copy
					}
					if !reg(rx, ry) {
						return false
					}
				}
			}
		}
		for _, w := range writes {
			written[w] = true
		}
	}
	return reg(a.out, b.out)
}

// segJoinTopBody is an ORDER BY ... LIMIT loop's body: the columns it loads,
// its instructions from the loads to the SorterInsert at end (the check at
// copies[check]), the record it makes and where each register came from.
type segJoinTopBody struct {
	writer map[int]segJoinArg
	loads  []segJoinArg
	copies []Instruction
	mk     Instruction
	check  int
	end    int
}

// parseSegJoinTopBody reads the body from pc, before limit, inserting into
// sorter.
func parseSegJoinTopBody(in []Instruction, pc, limit int, levelOf map[int]int, sorter int) (segJoinTopBody, bool) {
	tr := newSegJoinTrace(in, levelOf)
	b := segJoinTopBody{writer: tr.src, check: -1}
body:
	for ; pc < limit; pc++ {
		ins := in[pc]
		switch ins.Op {
		case OpColumn, OpRowid:
			if !tr.step(pc) {
				return b, false
			}
			b.loads = append(b.loads, tr.src[segJoinColDest(ins)])
			continue
		case OpSorterCheck:
			if b.check >= 0 || ins.P1 != sorter {
				return b, false
			}
			b.check = len(b.copies)
		case OpMakeRecord:
			if b.mk.Op == OpMakeRecord || b.check < 0 {
				return b, false
			}
			b.mk = ins
		case OpSorterInsert:
			if b.mk.Op != OpMakeRecord || ins.P1 != sorter || ins.P2 != b.mk.P3 {
				return b, false
			}
			b.copies = append(b.copies, ins)
			break body
		default:
			// An expression the record holds: the body's copy computes it
			// again from the loaded columns.
			if !tr.step(pc) {
				return b, false
			}
		}
		b.copies = append(b.copies, ins)
	}
	if pc >= limit || b.check < 0 {
		return b, false
	}
	b.end = pc
	return b, true
}

// segJoinTags is a MULTI_OR level's order as the loop builds it per outer
// row: each row of cursor tagged with the first of passes it satisfies
// (len(passes) for none), then ordered by (tag, that pass's index key,
// rowid) -- order.keys -- by OpMultiOrSort.
type segJoinTags struct {
	cursor int
	passes []*segJoinExpr
	order  *multiOrOrder
}

// segJoinSafeRun reports whether in[from:to] is a run the kernel may evaluate
// on any joined row the loop meets: operations that cannot raise an error --
// the loop runs them on rows the kernel skips, where an error would be the
// statement's answer -- reading columns of the levels in levelOf up to
// maxLv, or of cursor self, with every jump forward and inside the run or to
// its end.
func segJoinSafeRun(in []Instruction, from, to int, levelOf map[int]int, maxLv, self int) bool {
	if from >= to {
		return false
	}
	for j := from; j < to; j++ {
		ins := in[j]
		switch ins.Op {
		case OpColumn, OpRowid:
			if lv, ok := levelOf[ins.P1]; !(ok && lv <= maxLv || ins.P1 == self) {
				return false
			}
		case OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable, OpCopy, OpSCopy, OpNot,
			OpConcat, OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder,
			OpNegative, OpBitNot, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIf, OpIfNot, OpIsNull, OpNotNull, OpGoto:
			if segJoinIsJump(ins) && (ins.P2 <= j || ins.P2 > to) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// segJoinRun is in[from:to] as an expression into out.
func segJoinRun(in []Instruction, from, to, out int) *segJoinExpr {
	e := &segJoinExpr{out: out, insns: slices.Clone(in[from:to])}
	for j := from; j < to; j++ {
		e.pcs = append(e.pcs, j)
	}
	return e
}

// segJoinCrossFilter reads a term at pc over level lv and the levels before
// it: a safe run (segJoinSafeRun) computing a register, then an IfNot that
// skips the row to next unless it is true. It returns the run and the
// address after the IfNot.
func segJoinCrossFilter(in []Instruction, pc, limit int, levelOf map[int]int, lv, next int) (*segJoinExpr, int, bool) {
	for i := pc + 1; i < limit; i++ {
		ins := in[i]
		if ins.Op != OpIfNot || ins.P2 != next || ins.P3 != 1 {
			continue
		}
		if !segJoinSafeRun(in, pc, i, levelOf, lv, -1) {
			return nil, 0, false
		}
		return segJoinRun(in, pc, i, ins.P1), i + 1, true
	}
	return nil, 0, false
}

// parseSegJoinTags reads, at pc, the block a MULTI_OR level runs before its
// Rewind (emitMultiOrGroups):
//
//	Rewind cur -> done
//	top: Integer 0 g; <pass 0>; If -> tag; ...; Integer n g
//	tag: MultiOrTag cur g; Next cur -> top
//	done: MultiOrSort cur
//
// It returns nil and pc when there is none, else the tags and the address
// after the block.
func parseSegJoinTags(in []Instruction, pc int, levelOf map[int]int) (*segJoinTags, int, bool) {
	if pc >= len(in) || in[pc].Op != OpRewind {
		return nil, pc, true
	}
	cur, done := in[pc].P1, in[pc].P2
	if done <= pc || done >= len(in) || in[done].Op != OpMultiOrSort || in[done].P1 != cur {
		return nil, pc, true
	}
	mo, ok := in[done].P4.(*multiOrOrder)
	tag := done - 2
	if !ok || mo == nil || len(mo.passes) == 0 || len(mo.keys) != len(mo.passes) || tag <= pc ||
		in[tag].Op != OpMultiOrTag || in[tag].P1 != cur || in[tag+1].Op != OpNext || in[tag+1].P1 != cur || in[tag+1].P2 != pc+1 {
		return nil, 0, false
	}
	if _, entered := levelOf[cur]; entered {
		return nil, 0, false
	}
	g := in[tag].P2
	t := &segJoinTags{cursor: cur, order: mo}
	q := pc + 1
	for i := 0; ; i++ {
		if q >= tag || in[q].Op != OpInteger || in[q].P1 != i || in[q].P2 != g {
			return nil, 0, false
		}
		q++
		if i == len(mo.passes) {
			break
		}
		e := q
		for e < tag && !(in[e].Op == OpIf && in[e].P2 == tag) {
			e++
		}
		// A NULL pass is not satisfied: the If does not jump on it.
		if e >= tag || in[e].P3 != 0 || !segJoinSafeRun(in, q, e, levelOf, len(levelOf)-1, cur) {
			return nil, 0, false
		}
		t.passes = append(t.passes, segJoinRun(in, q, e, in[e].P1))
		q = e + 1
	}
	if q != tag {
		return nil, 0, false
	}
	return t, done + 1, true
}
