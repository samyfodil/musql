package engine

import (
	"math"
	"slices"
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
// its segments (no log, no session rows), every column it reads is a clean
// INTEGER block, every join is an equality between such a column and another
// table's rowid, and the sums cannot overflow in any order. The loop then
// runs as it always has.

// segJoinHits counts statements the join kernel answered, for tests.
var segJoinHits atomic.Int64

// SegJoinHitsForTest reads and clears the counter.
func SegJoinHitsForTest() int64 { return segJoinHits.Swap(0) }

// segJoinPlan is what OpSegJoinAgg carries: the loop's levels and the columns
// they read, as the recogniser traced them out of the program.
type segJoinPlan struct {
	cursors []int         // per level, outermost first
	edges   []segJoinEdge // the equalities joining the levels
	args    []segJoinArg  // the aggregate argument registers and their columns
	agg     *aggPlan
	seg     int // the AggStep segment selector (OpAggStep's P2)
}

// segJoinEdge is "level a's column aCol = level b's column bCol", a column of
// -1 being the rowid.
type segJoinEdge struct {
	a, aCol, b, bCol int
}

// segJoinArg is an aggregate argument: the register the loop loads it into
// and the column it loads.
type segJoinArg struct {
	reg, level, col int
}

// segJoinTable is one table's rows as the kernel reads them: flattened across
// segments, with a rowid-to-row lookup.
type segJoinTable struct {
	segs   []*segment
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
// of its rows: rows pending in the log, a write session's own rows, a
// generated column, a row short of the table's width.
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
		t.n += s.nRows
	}
	t.rowids = make([]int64, 0, t.n)
	for _, s := range segs {
		for i := range s.nRows {
			t.rowids = append(t.rowids, int64(s.Rowid(i)))
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

// index builds the rowid lookup.
func (t *segJoinTable) index() {
	if t.dense != nil || t.sparse != nil || t.n == 0 {
		return
	}
	lo, hi := t.rowids[0], t.rowids[0]
	for _, r := range t.rowids {
		lo, hi = min(lo, r), max(hi, r)
	}
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

// segJoinAggregate answers the plan's aggregate, or reports false having
// changed nothing. An error is the step's own, which the loop would have met.
func (m *vdbe) segJoinAggregate(plan *segJoinPlan) (bool, error) {
	if !plan.agg.rowFree || len(plan.cursors) > 1<<10 {
		return false, nil
	}
	k := len(plan.cursors)
	tabs := make([]*segJoinTable, k)
	for i, c := range plan.cursors {
		if c >= len(m.cursors) {
			return false, nil
		}
		t, ok := segJoinTableOf(m.cursors[c])
		if !ok {
			return false, nil
		}
		tabs[i] = t
	}
	isRowid := func(level, col int) bool { return col < 0 || col == tabs[level].ipk }

	// Orient each equality: the side that is its table's rowid is looked up
	// from the other. Every level but one -- the fact -- must be looked up
	// exactly once, which makes the levels a tree whose every joined row is one
	// fact row and the rows its keys name.
	parent := make([]int, k) // the level a level is looked up from, -1 for the fact
	parentCol := make([]int, k)
	for i := range parent {
		parent[i] = -2
	}
	for _, e := range plan.edges {
		ra, rb := isRowid(e.a, e.aCol), isRowid(e.b, e.bCol)
		switch {
		case rb && !ra:
			if parent[e.b] != -2 {
				return false, nil
			}
			parent[e.b], parentCol[e.b] = e.a, e.aCol
		case ra && !rb:
			if parent[e.a] != -2 {
				return false, nil
			}
			parent[e.a], parentCol[e.a] = e.b, e.bCol
		default:
			return false, nil
		}
	}
	fact := -1
	for i := range parent {
		if parent[i] == -2 {
			if fact >= 0 {
				return false, nil
			}
			fact, parent[i] = i, -1
		}
	}
	if fact < 0 {
		return false, nil
	}
	// Levels in an order that has each one's parent first.
	order := []int{fact}
	for len(order) < k {
		grew := false
		for i := range parent {
			if parent[i] >= 0 && !slices.Contains(order, i) && slices.Contains(order, parent[i]) {
				order = append(order, i)
				grew = true
			}
		}
		if !grew {
			return false, nil // not connected to the fact
		}
	}

	// slot[i][r] is level i's row joined to its parent's row r, or -1.
	slot := make([][]int32, k)
	for _, i := range order[1:] {
		keys, ok := tabs[parent[i]].col(parentCol[i])
		if !ok {
			return false, nil
		}
		tabs[i].index()
		s := make([]int32, len(keys))
		for r, key := range keys {
			s[r] = tabs[i].row(key)
		}
		slot[i] = s
	}
	args := make([][]int64, len(plan.args))
	for a, arg := range plan.args {
		vals, ok := tabs[arg.level].col(arg.col)
		if !ok {
			return false, nil
		}
		args[a] = vals
	}

	// Which argument each step reads, and that it is one the sums model.
	argOf := map[int]int{}
	for a, arg := range plan.args {
		argOf[arg.reg] = a
	}
	for _, it := range aggPlanTemplates(plan.agg) {
		if it.filter != nil || it.orderBy != nil || it.distinct || it.orderStrict || it.stepSeg != plan.seg {
			return false, nil
		}
		switch it.kind {
		case aggCountStar:
		case aggCount, aggSum, aggMin, aggMax:
			if _, ok := argOf[it.rowRegs[aggExprArg]-1]; !ok || it.minMaxLastWins {
				return false, nil
			}
		default:
			return false, nil
		}
	}

	// Walk the fact. at[i] is level i's row for the current fact row.
	na := len(args)
	sums := make([]int64, na)
	abs := make([]uint64, na)
	mins := make([]int64, na)
	maxs := make([]int64, na)
	for a := range na {
		mins[a], maxs[a] = math.MaxInt64, math.MinInt64
	}
	at := make([]int32, k)
	last := make([]int32, k)
	var n int64
rows:
	for f := range tabs[fact].n {
		at[fact] = int32(f)
		for _, i := range order[1:] {
			r := slot[i][at[parent[i]]]
			if r < 0 {
				continue rows
			}
			at[i] = r
		}
		n++
		for a, arg := range plan.args {
			v := args[a][at[arg.level]]
			sums[a] += v
			mins[a], maxs[a] = min(mins[a], v), max(maxs[a], v)
			u := uint64(v)
			if v < 0 {
				u = -u
			}
			if abs[a]+u < abs[a] || abs[a]+u > math.MaxInt64 {
				return false, nil
			}
			abs[a] += u
		}
		copy(last, at)
	}
	if n == 0 {
		segJoinHits.Add(1)
		return true, nil // no joined row: the accumulators stay as reset
	}

	// Step the last joined row through the shared code, then add the rest.
	lastArg := func(a int) int64 { return args[a][last[plan.args[a].level]] }
	for a, arg := range plan.args {
		m.regs[arg.reg] = Value{Typ: Int, I: lastArg(a)}
	}
	nr := plan.agg.nCols + len(plan.agg.scopes)
	if len(m.aggNullRow) < nr {
		m.aggNullRow = make([]Value, nr)
	}
	row, rowids := m.aggNullRow[:plan.agg.nCols:plan.agg.nCols], m.aggNullRow[plan.agg.nCols:nr]
	if err := m.aggStep(m.aggRowCtx(plan.agg, row, rowids), plan.seg); err != nil {
		return false, err
	}
	rest := func(it *aggItem) {
		switch it.kind {
		case aggCountStar, aggCount:
			it.cnt += n - 1
		case aggSum:
			a := argOf[it.rowRegs[aggExprArg]-1]
			it.cnt += n - 1
			it.iSum += sums[a] - lastArg(a)
		case aggMin:
			it.best = Value{Typ: Int, I: mins[argOf[it.rowRegs[aggExprArg]-1]]}
		case aggMax:
			it.best = Value{Typ: Int, I: maxs[argOf[it.rowRegs[aggExprArg]-1]]}
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
	segJoinHits.Add(1)
	return true, nil
}
