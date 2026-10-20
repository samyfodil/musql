package engine

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"
	"sync/atomic"
)

// An aggregate over an inner join, answered from the tables' column blocks.
//
// The loop the compiler emits visits every joined row through the VDBE: a
// column read per key, a seek per level, a recheck, a step. Over columnar
// tables most joins an aggregate runs are foreign keys into rowids -- each
// table but one (the fact) is reached from another through "x = rowid" -- and
// then every joined row is one row of the fact with the rows its keys name.
// So the join is a pass over the fact's blocks, each key followed through a
// rowid-to-row table built once per statement, and the aggregates are summed
// as segGroupBulk sums them: each group stepped once through the shared code
// with one row, the rest added to its accumulators.
//
// It declines, before touching any state, unless every table is the whole of
// its segments (no log, no session rows), every column it sums or joins on is
// a clean INTEGER block, every join is an equality between such a column and
// another table's rowid, and the sums cannot overflow in any order. The loop
// then runs as it always has.

// segJoinHits counts statements the join kernel answered, for tests.
var segJoinHits atomic.Int64

// SegJoinHitsForTest reads and clears the counter.
func SegJoinHitsForTest() int64 { return segJoinHits.Swap(0) }

// segJoinPlan is what OpSegJoinAgg and OpSegJoinGroups carry: the loop's
// levels and the columns they read, as the recogniser traced them out of the
// program.
type segJoinPlan struct {
	cursors []int         // per level, outermost first
	edges   []segJoinEdge // the equalities joining the levels
	args    []segJoinArg  // the aggregate argument registers and their columns
	agg     *aggPlan
	seg     int // the AggStep segment selector (OpAggStep's P2)

	// A GROUP BY's: the column its one key reads, and the registers the loop
	// body loads for the row it hands the drain (OpSegJoinNext fills them).
	grouped bool
	key     segJoinArg
	loads   []segJoinArg
	// sorted: some level visits its rows in an index's order, not rowid
	// order, so the loop's order is not one the kernel knows.
	sorted bool
}

// segJoinEdge is "level a's column aCol = level b's column bCol", a column of
// -1 being the rowid.
type segJoinEdge struct {
	a, aCol, b, bCol int
}

// segJoinArg is a column some level reads into a register, -1 the rowid.
type segJoinArg struct {
	reg, level, col int
}

// segJoinTable is one table's rows as the kernel reads them: flattened across
// segments in rowid order, with a rowid-to-row lookup.
type segJoinTable struct {
	segs   []*segment
	starts []int // each segment's first row
	n      int
	ipk    int
	tbl    *resolvedTable
	rowids []int64
	cols   map[int][]int64
	// lo/dense map a rowid to its row + 1 when the rowids are dense enough,
	// else sparse does.
	lo     int64
	dense  []int32
	sparse map[int64]int32
}

// segJoinTableOf is cur's table, or false when its segments are not the whole
// of its rows (rows pending in the log, a write session's own rows, a
// generated column, a row short of the table's width) or do not hold them in
// rowid order, which is the order a level visits them in.
func segJoinTableOf(cur *vdbeCursor) (*segJoinTable, bool) {
	if cur == nil || cur.tbl == nil || cur.pager == nil || cur.pager.segs == nil ||
		cur.tbl.withoutRowid || cur.hasGeneratedCols() {
		return nil, false
	}
	p, root := cur.pager, cur.tbl.root
	var segs []*segment
	if p.segs.isLive(root) {
		s, ok := p.segs.liveBaseSegments(root)
		if !ok {
			return nil, false
		}
		segs = s
	} else {
		if !p.segCleanFor(root) {
			return nil, false
		}
		segs = p.segs.byRoot[root]
	}
	t := &segJoinTable{segs: segs, ipk: cur.tbl.ipkIndex, tbl: cur.tbl, cols: map[int][]int64{}}
	for _, s := range segs {
		if s == nil || len(s.cols) != len(cur.tbl.cols) || !s.fullWidth {
			return nil, false
		}
		t.starts = append(t.starts, t.n)
		t.n += s.nRows
	}
	t.rowids = make([]int64, 0, t.n)
	for _, s := range segs {
		for i := range s.nRows {
			r := int64(s.Rowid(i))
			if len(t.rowids) > 0 && r <= t.rowids[len(t.rowids)-1] {
				return nil, false
			}
			t.rowids = append(t.rowids, r)
		}
	}
	return t, true
}

// col is column c of every row (c -1 or the INTEGER PRIMARY KEY: the rowid),
// or false unless every segment stores it as a clean INTEGER block. A
// REAL-affinity column declines: its integers read as REAL.
func (t *segJoinTable) col(c int) ([]int64, bool) {
	if c < 0 || c == t.ipk {
		return t.rowids, true
	}
	if v, ok := t.cols[c]; ok {
		return v, v != nil
	}
	t.cols[c] = nil
	if c >= len(t.tbl.cols) || t.tbl.cols[c].Aff == affReal {
		return nil, false
	}
	out := make([]int64, 0, t.n)
	for _, s := range t.segs {
		vals, ok := segCleanInt64Column(s, c)
		if !ok {
			return nil, false
		}
		out = append(out, vals[:s.nRows]...)
	}
	t.cols[c] = out
	return out, true
}

// value is column c (-1 the rowid) of row r, as the cursor reads it.
func (t *segJoinTable) value(r int32, c int) Value {
	if c < 0 {
		return Value{Typ: Int, I: t.rowids[r]}
	}
	si := sort.SearchInts(t.starts, int(r)+1) - 1
	s, row := t.segs[si], int(r)-t.starts[si]
	return segColumnValue(segColPlansFor(t.tbl), s, row, len(s.cols), uint64(t.rowids[r]), c)
}

// index builds the rowid lookup.
func (t *segJoinTable) index() {
	if t.dense != nil || t.sparse != nil || t.n == 0 {
		return
	}
	lo, hi := t.rowids[0], t.rowids[t.n-1]
	if span := uint64(hi - lo); span < uint64(4*t.n+1024) {
		t.lo, t.dense = lo, make([]int32, span+1)
		for i, r := range t.rowids {
			t.dense[r-lo] = int32(i + 1)
		}
		return
	}
	t.sparse = make(map[int64]int32, t.n)
	for i, r := range t.rowids {
		t.sparse[r] = int32(i + 1)
	}
}

// row is the row holding rowid r, or -1.
func (t *segJoinTable) row(r int64) int32 {
	if t.dense != nil {
		if r < t.lo || uint64(r-t.lo) >= uint64(len(t.dense)) {
			return -1
		}
		return t.dense[r-t.lo] - 1
	}
	if i, ok := t.sparse[r]; ok {
		return i - 1
	}
	return -1
}

// segJoinWalk is a join opened for walking: each level's table, and how each
// level but the fact is reached from its parent.
type segJoinWalk struct {
	tabs   []*segJoinTable
	fact   int
	order  []int     // the levels, each after its parent
	parent []int     // the level a level is looked up from, -1 for the fact
	slot   [][]int32 // slot[i][r]: level i's row joined to its parent's row r, or -1
}

// openSegJoin opens plan's join, or reports false.
func (m *vdbe) openSegJoin(plan *segJoinPlan) (*segJoinWalk, bool) {
	k := len(plan.cursors)
	w := &segJoinWalk{tabs: make([]*segJoinTable, k), parent: make([]int, k), slot: make([][]int32, k)}
	for i, c := range plan.cursors {
		if c >= len(m.cursors) {
			return nil, false
		}
		t, ok := segJoinTableOf(m.cursors[c])
		if !ok {
			return nil, false
		}
		w.tabs[i] = t
	}
	isRowid := func(level, col int) bool { return col < 0 || col == w.tabs[level].ipk }

	// Orient each equality: the side that is its table's rowid is looked up
	// from the other. Every level but one -- the fact -- must be looked up
	// exactly once, which makes the levels a tree whose every joined row is one
	// fact row and the rows its keys name.
	parentCol := make([]int, k)
	for i := range w.parent {
		w.parent[i] = -2
	}
	for _, e := range plan.edges {
		ra, rb := isRowid(e.a, e.aCol), isRowid(e.b, e.bCol)
		switch {
		case rb && !ra:
			if w.parent[e.b] != -2 {
				return nil, false
			}
			w.parent[e.b], parentCol[e.b] = e.a, e.aCol
		case ra && !rb:
			if w.parent[e.a] != -2 {
				return nil, false
			}
			w.parent[e.a], parentCol[e.a] = e.b, e.bCol
		default:
			return nil, false
		}
	}
	w.fact = -1
	for i := range w.parent {
		if w.parent[i] == -2 {
			if w.fact >= 0 {
				return nil, false
			}
			w.fact, w.parent[i] = i, -1
		}
	}
	if w.fact < 0 {
		return nil, false
	}
	w.order = []int{w.fact}
	for len(w.order) < k {
		grew := false
		for i := range w.parent {
			if w.parent[i] >= 0 && !slices.Contains(w.order, i) && slices.Contains(w.order, w.parent[i]) {
				w.order = append(w.order, i)
				grew = true
			}
		}
		if !grew {
			return nil, false // not connected to the fact
		}
	}
	for _, i := range w.order[1:] {
		keys, ok := w.tabs[w.parent[i]].col(parentCol[i])
		if !ok {
			return nil, false
		}
		w.tabs[i].index()
		s := make([]int32, len(keys))
		for r, key := range keys {
			s[r] = w.tabs[i].row(key)
		}
		w.slot[i] = s
	}
	return w, true
}

// each calls fn once per joined row, with at holding each level's row; fn
// returning false stops the walk, and each reports whether it ran to the end.
func (w *segJoinWalk) each(at []int32, fn func() bool) bool {
	rest := w.order[1:]
rows:
	for f := range w.tabs[w.fact].n {
		at[w.fact] = int32(f)
		for _, i := range rest {
			r := w.slot[i][at[w.parent[i]]]
			if r < 0 {
				continue rows
			}
			at[i] = r
		}
		if !fn() {
			return false
		}
	}
	return true
}

// segJoinSums is what a group's rows add up to, per integer argument.
type segJoinSums struct {
	n    int64
	sums []int64
	abs  []uint64 // sum of |v|: no partial sum, in any order, can pass it
	mins []int64
	maxs []int64
	tops []uint64 // the largest |v|
	ors  []uint64 // every |v| or-ed: its trailing zeros are the smallest unit
}

func newSegJoinSums(na int) *segJoinSums {
	s := &segJoinSums{sums: make([]int64, na), abs: make([]uint64, na), mins: make([]int64, na), maxs: make([]int64, na),
		tops: make([]uint64, na), ors: make([]uint64, na)}
	for a := range na {
		s.mins[a], s.maxs[a] = math.MaxInt64, math.MinInt64
	}
	return s
}

// add takes one row's integer arguments, or reports false when a sum could
// overflow.
func (s *segJoinSums) add(args *segJoinArgs, at []int32) bool {
	s.n++
	for a, vals := range args.ints {
		if vals == nil {
			continue
		}
		v := vals[at[args.levels[a]]]
		s.sums[a] += v
		s.mins[a], s.maxs[a] = min(s.mins[a], v), max(s.maxs[a], v)
		u := uint64(v)
		if v < 0 {
			u = -u
		}
		if s.abs[a]+u < s.abs[a] || s.abs[a]+u > math.MaxInt64 {
			return false
		}
		s.abs[a] += u
		s.tops[a], s.ors[a] = max(s.tops[a], u), s.ors[a]|u
	}
	return true
}

// segJoinArgs is each aggregate argument's column: INTEGER, summed as the
// rows go by, or REAL, whose values a group replays in the loop's order
// because a floating-point sum depends on it.
type segJoinArgs struct {
	ints   [][]int64   // nil for a REAL argument
	flts   [][]float64 // nil for an INTEGER one
	levels []int
	argOf  map[int]int // the step's argument register -> its index
	floats bool
}

// segJoinArgsOf reads plan's arguments, or reports false when an aggregate is
// one the kernel does not model.
func segJoinArgsOf(plan *segJoinPlan, w *segJoinWalk) (*segJoinArgs, bool) {
	n := len(plan.args)
	args := &segJoinArgs{ints: make([][]int64, n), flts: make([][]float64, n), levels: make([]int, n), argOf: map[int]int{}}
	for a, arg := range plan.args {
		args.levels[a] = arg.level
		args.argOf[arg.reg] = a
		if vals, ok := w.tabs[arg.level].col(arg.col); ok {
			args.ints[a] = vals
			continue
		}
		vals, ok := w.tabs[arg.level].fcol(arg.col)
		if !ok {
			return nil, false
		}
		args.flts[a], args.floats = vals, true
	}
	if args.floats && plan.sorted {
		return nil, false // a REAL sum replays the loop's order, unknown here
	}
	for _, it := range aggPlanTemplates(plan.agg) {
		sumKind := it.kind == aggSum || it.kind == aggTotal || it.kind == aggAvg
		// An order-strict sum checks at finalize that no order could change
		// it, from a window over its values that rest keeps up to date.
		if it.filter != nil || it.orderBy != nil || it.distinct || it.orderStrict && !sumKind || it.stepSeg != plan.seg {
			return nil, false
		}
		switch it.kind {
		case aggCountStar:
		case aggCount, aggSum, aggTotal, aggAvg, aggMin, aggMax:
			a, ok := args.argOf[it.rowRegs[aggExprArg]-1]
			if !ok || it.minMaxLastWins || args.flts[a] != nil && (it.kind == aggMin || it.kind == aggMax) {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return args, true
}

// fcol is REAL column c of every row, as the cursor reads it, or false unless
// every segment stores it as a clean REAL block, or as clean integers of a
// REAL-affinity column (which read as REAL).
func (t *segJoinTable) fcol(c int) ([]float64, bool) {
	if c < 0 || c == t.ipk || c >= len(t.tbl.cols) {
		return nil, false
	}
	out := make([]float64, 0, t.n)
	for _, s := range t.segs {
		if vals, ok := s.Float64Column(c); ok {
			out = append(out, vals[:s.nRows]...)
			continue
		}
		ints, ok := segCleanInt64Column(s, c)
		if !ok || t.tbl.cols[c].Aff != affReal {
			return nil, false
		}
		for _, v := range ints[:s.nRows] {
			out = append(out, float64(v))
		}
	}
	return out, true
}

// segJoinGroups is an aggregate's groups -- one for a whole-join aggregate --
// as the kernel hands them to the program: in the order the loop first meets
// them, each with the joined row the loop would have met first.
type segJoinGroups struct {
	w      *segJoinWalk
	plan   *segJoinPlan
	groups []segJoinGroup
	next   int
	args   *segJoinArgs
}

type segJoinGroup struct {
	sums  *segJoinSums
	first []int32 // each level's row in the group's first joined row
	// rows is every joined row of the group, k levels each, in the loop's
	// order: kept when a REAL argument is replayed.
	rows []int32
}

// loopBefore reports whether joined row a comes before b in the loop's order:
// the outermost level's row first, each level visiting its rows in rowid
// order, which is their order in its table.
func loopBefore(a, b []int32) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// segJoinGroupsOf builds plan's groups, or reports false. A plan with no
// GROUP BY has one group, or none when nothing joins.
func (m *vdbe) segJoinGroupsOf(plan *segJoinPlan) (*segJoinGroups, bool) {
	w, ok := m.openSegJoin(plan)
	if !ok {
		return nil, false
	}
	args, ok := segJoinArgsOf(plan, w)
	if !ok {
		return nil, false
	}
	var keys []int64
	if plan.grouped {
		if keys, ok = w.tabs[plan.key.level].col(plan.key.col); !ok {
			return nil, false
		}
	}
	byKey := map[int64]int{}
	var groups []segJoinGroup
	k := len(plan.cursors)
	at := make([]int32, k)
	if !w.each(at, func() bool {
		var key int64
		if keys != nil {
			key = keys[at[plan.key.level]]
		}
		gi, seen := byKey[key]
		switch {
		case !seen:
			gi = len(groups)
			byKey[key] = gi
			groups = append(groups, segJoinGroup{sums: newSegJoinSums(len(args.ints)), first: slices.Clone(at)})
		case args.floats:
			// The replay is in the order rows arrive here, which must be the
			// loop's, so the first row is the first one kept.
			g := &groups[gi]
			if !loopBefore(g.rows[len(g.rows)-k:], at) {
				return false
			}
		default:
			if g := &groups[gi]; loopBefore(at, g.first) {
				copy(g.first, at)
			}
		}
		g := &groups[gi]
		if args.floats {
			g.rows = append(g.rows, at...)
		}
		return g.sums.add(args, at)
	}) {
		return nil, false
	}
	sort.Slice(groups, func(i, j int) bool { return loopBefore(groups[i].first, groups[j].first) })
	return &segJoinGroups{w: w, plan: plan, groups: groups, args: args}, true
}

// load puts group g's first joined row into the registers the loop body
// reads it into.
func (gs *segJoinGroups) load(m *vdbe, g *segJoinGroup) {
	for _, l := range gs.plan.loads {
		m.regs[l.reg] = gs.w.tabs[l.level].value(g.first[l.level], l.col)
	}
}

// loadArgs puts group g's first joined row's arguments into the registers the
// step reads them from.
func (gs *segJoinGroups) loadArgs(m *vdbe, g *segJoinGroup) {
	for _, arg := range gs.plan.args {
		m.regs[arg.reg] = gs.w.tabs[arg.level].value(g.first[arg.level], arg.col)
	}
}

// rest adds group g's other rows to the accumulators its first row was just
// stepped into.
func (gs *segJoinGroups) rest(m *vdbe, g *segJoinGroup) {
	args, s := gs.args, g.sums
	k := len(gs.plan.cursors)
	rest := func(it *aggItem) {
		if it.kind == aggCountStar {
			it.cnt += s.n - 1
			return
		}
		a := args.argOf[it.rowRegs[aggExprArg]-1]
		if fl := args.flts[a]; fl != nil {
			if it.kind == aggCount {
				it.cnt += s.n - 1
				return
			}
			// The rows after the first, through what step does with each.
			for r := k; r < len(g.rows); r += k {
				f := fl[g.rows[r+args.levels[a]]]
				it.cnt++
				if it.orderStrict {
					it.sumWatch(false, 0, f)
				}
				it.sumStep(false, 0, f)
			}
			return
		}
		switch it.kind {
		case aggCount:
			it.cnt += s.n - 1
		case aggSum, aggTotal, aggAvg:
			it.cnt += s.n - 1
			it.iSum += s.sums[a] - args.ints[a][g.first[args.levels[a]]]
			if it.orderStrict && s.ors[a] != 0 {
				// What sumWatch over every value would have kept: the
				// largest magnitude and the smallest power of two dividing
				// them all.
				it.sumWatch(true, int64(s.tops[a]), 0)
				it.sumWatch(true, int64(1)<<bits.TrailingZeros64(s.ors[a]), 0)
			}
		case aggMin:
			it.best = Value{Typ: Int, I: s.mins[a]}
		case aggMax:
			it.best = Value{Typ: Int, I: s.maxs[a]}
		}
	}
	accs := m.aggAccs
	for _, set := range accs.out {
		for _, it := range set {
			rest(it)
		}
	}
	for _, it := range accs.having {
		rest(it)
	}
	for _, set := range accs.order {
		for _, it := range set {
			rest(it)
		}
	}
	for _, it := range accs.magnets {
		rest(it)
	}
}

// segJoinAggregate answers the plan's whole-join aggregate, or reports false
// having changed nothing. An error is the step's own, which the loop would
// have met.
func (m *vdbe) segJoinAggregate(plan *segJoinPlan) (bool, error) {
	if !plan.agg.rowFree {
		return false, nil
	}
	gs, ok := m.segJoinGroupsOf(plan)
	if !ok {
		return false, nil
	}
	segJoinHits.Add(1)
	if len(gs.groups) == 0 {
		return true, nil // no joined row: the accumulators stay as reset
	}
	// Step the first joined row through the shared code, then add the rest.
	g := &gs.groups[0]
	gs.loadArgs(m, g)
	nr := plan.agg.nCols + len(plan.agg.scopes)
	if len(m.aggNullRow) < nr {
		m.aggNullRow = make([]Value, nr)
	}
	row, rowids := m.aggNullRow[:plan.agg.nCols:plan.agg.nCols], m.aggNullRow[plan.agg.nCols:nr]
	if err := m.aggStep(m.aggRowCtx(plan.agg, row, rowids), plan.seg); err != nil {
		return false, err
	}
	gs.rest(m, g)
	return true, nil
}

// segJoinHashAgg fills a hashed GROUP BY's buckets, or reports false having
// changed nothing. An error is a step's own.
func (m *vdbe) segJoinHashAgg(plan *segJoinPlan) (bool, error) {
	if m.hashAgg != nil && len(m.hashAgg.buckets) > 0 {
		return false, nil // not the fresh table the loop would fill
	}
	gs, ok := m.segJoinGroupsOf(plan)
	if !ok {
		return false, nil
	}
	segJoinHits.Add(1)
	row := growValues(&m.aggRowBuf, plan.agg.nCols)
	rowids := growValues(&m.aggRowidBuf, len(plan.agg.scopes))
	keyVals := make([]Value, 1)
	for i := range gs.groups {
		g := &gs.groups[i]
		gs.loadArgs(m, g)
		keyVals[0] = gs.w.tabs[plan.key.level].value(g.first[plan.key.level], plan.key.col)
		m.regs[plan.key.reg] = keyVals[0]
		if !gs.gather(g, row, rowids) {
			return false, fmt.Errorf("vdbe: OpSegJoinHash over a cursor outside its join")
		}
		if err := m.hashAggStepRow(plan.agg, keyVals, plan.seg, row, rowids); err != nil {
			return false, err
		}
		gs.rest(m, g)
	}
	return true, nil
}

// gather is gatherCursorRow over group g's first joined row: the row and the
// rowids the loop's step would have read with its cursors on it.
func (gs *segJoinGroups) gather(g *segJoinGroup, row, rowids []Value) bool {
	level := func(cursor int) int { return slices.Index(gs.plan.cursors, cursor) }
	for _, ga := range gs.plan.agg.gathers {
		lv := level(ga.cursor)
		if lv < 0 || ga.offset+ga.n > len(row) {
			return false
		}
		for c := range min(ga.n, len(gs.w.tabs[lv].tbl.cols)) {
			row[ga.offset+c] = gs.w.tabs[lv].value(g.first[lv], c)
		}
	}
	for i, cur := range gs.plan.agg.cursors {
		lv := level(cur)
		switch {
		case gs.plan.agg.scopes[i].noRowid:
			rowids[i] = Value{Typ: Null}
		case lv < 0:
			return false
		default:
			rowids[i] = Value{Typ: Int, I: gs.w.tabs[lv].rowids[g.first[lv]]}
		}
	}
	return true
}
