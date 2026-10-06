package engine

import (
	"math"
	"sync/atomic"
)

// segGroupBulkHits counts statements the bulk path answered, for tests.
var segGroupBulkHits atomic.Int64

// Block-at-a-time GROUP BY, for the shape most grouped queries have: one
// INTEGER key and count(*), count(col), sum(col), min(col) and max(col) over
// INTEGER columns.
//
// segHashAggWalk steps every row through hashAggStepRow, which hashes the key
// and runs the accumulator machinery once per row -- the time DuckDB was
// winning by 4x. Here the groups are summed straight off the []int64 blocks
// first, and then each group is stepped ONCE, through that same
// hashAggStepRow, with its LAST row: the bucket, its key and every per-group
// field the drain reads are created by the shared code exactly as before. The
// other n-1 rows are then added to the accumulators directly.
//
// That addition is exact only for what is checked below:
//   - count(*) and count(col) over a column with no NULLs add n-1;
//   - sum(col) over integers adds the rest of the integer sum, and is used
//     only when the group's sum of |v| fits in an int64, so no partial sum in
//     ANY order overflows -- sumStep's overflow latch never fires either way;
//   - min(col) and max(col) take the group's extreme. Two integers that compare
//     equal are identical, so which of a tie arrived first cannot show;
//   - no DISTINCT, FILTER, ORDER BY inside the call, order-strict
//     accumulator or anchor certificate: each of those reads the rows
//     individually.
//
// A min()/max() also raises a census (aggPlan.magnet) that picks the row a
// bare column reads. The recognizer admits only plans with no bare column and
// no HAVING (aggPlanReadsOnlyLoweredColsExceptOrder), so nothing reads that
// row; the census sites are stepped with the last row and given the extreme
// like any other min/max.
// Anything else, and any table with rows pending in the delta, returns false
// before touching state, and the row walk runs as it always has.

// segGroupBulkMaxDense is the widest key range summed into flat arrays rather
// than a map.
const segGroupBulkMaxDense = 1 << 16

type segGroupAcc struct {
	n       int64
	sums    []int64  // per argument column
	abs     []uint64 // per argument column: sum of |v|, the overflow guard
	mins    []int64  // per argument column
	maxs    []int64
	lastSeg int
	lastRow int
}

// segGroupBulk's outcomes.
const (
	bulkNotEligible = iota // nothing written: walk the rows
	bulkAnswered
	bulkFailed // buckets partly written: decline the statement
)

func (m *vdbe) segGroupBulk(segs []*segment, plan *segGroupPlan, ipk int, row, rowids, keyVals []Value) int {
	if len(plan.keyCols) != 1 || plan.agg.anchorCheck != nil {
		return bulkNotEligible
	}
	// Which argument column each count(col)/sum(col) reads, by its register.
	argOf := map[int]int{} // register (0-based) -> index into plan.argCols
	for i, r := range plan.argRegs {
		argOf[r] = i
	}
	needSum := make([]bool, len(plan.argCols))
	needMM := make([]bool, len(plan.argCols))
	for _, it := range aggPlanTemplates(plan.agg) {
		if it.filter != nil || it.orderBy != nil || it.distinct || it.orderStrict || it.stepSeg != plan.seg {
			return bulkNotEligible
		}
		switch it.kind {
		case aggCountStar:
		case aggCount, aggSum, aggMin, aggMax:
			a, ok := argOf[it.rowRegs[aggExprArg]-1]
			if !ok || it.minMaxLastWins {
				return bulkNotEligible
			}
			needSum[a] = needSum[a] || it.kind == aggSum
			needMM[a] = needMM[a] || it.kind == aggMin || it.kind == aggMax
		default:
			return bulkNotEligible
		}
	}

	// Every block must be a clean int64 one: no NULLs, no exceptions.
	type blocks struct {
		key  []int64 // nil: the key is the rowid
		args [][]int64
	}
	bs := make([]blocks, len(segs))
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	for si, s := range segs {
		rd, ok := segColReaders(s, append([]int{plan.keyCols[0]}, plan.argCols...), ipk)
		if !ok {
			return bulkNotEligible
		}
		for _, r := range rd {
			if r.kind != segColInt {
				return bulkNotEligible // the rowid as key or argument goes the row way
			}
		}
		bs[si].key = rd[0].ints
		for _, r := range rd[1:] {
			bs[si].args = append(bs[si].args, r.ints)
		}
		for _, k := range rd[0].ints[:s.nRows] {
			lo, hi = min(lo, k), max(hi, k)
		}
	}
	if lo > hi {
		return bulkAnswered // no rows: no groups, which is what the walk would leave
	}

	// Sum every group. Nothing is written to the VM until this succeeds. Each
	// segment builds its own partial groups (on its own goroutine under
	// WithWorkers); merging them in segment order keeps every group's LAST row
	// the one a single pass would have seen last.
	na := len(plan.argCols)
	dense := uint64(hi-lo) < segGroupBulkMaxDense
	add := func(groups map[int64]*segGroupAcc, slots []segGroupAcc, k int64) *segGroupAcc {
		var g *segGroupAcc
		if slots != nil {
			g = &slots[k-lo]
		} else if g = groups[k]; g == nil {
			g = &segGroupAcc{}
			groups[k] = g
		}
		if g.sums == nil {
			g.sums, g.abs = make([]int64, na), make([]uint64, na)
			g.mins, g.maxs = make([]int64, na), make([]int64, na)
			for a := range na {
				g.mins[a], g.maxs[a] = math.MaxInt64, math.MinInt64
			}
		}
		return g
	}
	type partial struct {
		slots  []segGroupAcc
		groups map[int64]*segGroupAcc
	}
	parts := make([]partial, len(segs))
	ok := segEach(len(segs), func(si int) bool {
		s, b, pt := segs[si], bs[si], &parts[si]
		if dense {
			pt.slots = make([]segGroupAcc, hi-lo+1)
		} else {
			pt.groups = map[int64]*segGroupAcc{}
		}
		if dense && segGroupDenseFlat(s, plan.argCols, b.key, b.args, needSum, needMM, lo, si, pt.slots) {
			return true
		}
		for r := 0; r < s.nRows; r++ {
			g := add(pt.groups, pt.slots, b.key[r])
			g.n++
			g.lastSeg, g.lastRow = si, r
			for a, col := range b.args {
				v := col[r]
				if needMM[a] {
					g.mins[a], g.maxs[a] = min(g.mins[a], v), max(g.maxs[a], v)
				}
				if !needSum[a] {
					continue
				}
				g.sums[a] += v // wraps only when abs does not fit, checked below
				u := uint64(v)
				if v < 0 {
					u = -u
				}
				if g.abs[a]+u < g.abs[a] || g.abs[a]+u > math.MaxInt64 {
					return false
				}
				g.abs[a] += u
			}
		}
		return true
	})
	if !ok {
		return bulkNotEligible
	}
	var slots []segGroupAcc
	groups := map[int64]*segGroupAcc{}
	if dense {
		slots = make([]segGroupAcc, hi-lo+1)
	}
	merge := func(k int64, from *segGroupAcc) bool {
		if from.n == 0 {
			return true
		}
		g := add(groups, slots, k)
		g.n += from.n
		g.lastSeg, g.lastRow = from.lastSeg, from.lastRow
		for a := range na {
			g.mins[a], g.maxs[a] = min(g.mins[a], from.mins[a]), max(g.maxs[a], from.maxs[a])
			g.sums[a] += from.sums[a]
			if g.abs[a]+from.abs[a] < g.abs[a] || g.abs[a]+from.abs[a] > math.MaxInt64 {
				return false
			}
			g.abs[a] += from.abs[a]
		}
		return true
	}
	for _, pt := range parts {
		for i := range pt.slots {
			if !merge(lo+int64(i), &pt.slots[i]) {
				return bulkNotEligible
			}
		}
		for k, g := range pt.groups {
			if !merge(k, g) {
				return bulkNotEligible
			}
		}
	}
	var sparse map[int64]*segGroupAcc
	if !dense {
		sparse = groups
	}

	// Step each group's last row through the shared code, then add the rest.
	apply := func(key int64, g *segGroupAcc) bool {
		b := bs[g.lastSeg]
		val := Value{Typ: Int, I: key}
		m.regs[plan.keyRegs[0]] = val
		if c := plan.keyCols[0]; c >= 0 && c < len(row) {
			row[c] = val
		}
		for a, col := range b.args {
			v := Value{Typ: Int, I: col[g.lastRow]}
			m.regs[plan.argRegs[a]] = v
			if c := plan.argCols[a]; c >= 0 && c < len(row) {
				row[c] = v
			}
		}
		keyVals[0] = val
		if err := m.hashAggStepRow(plan.agg, keyVals, plan.seg, row, rowids); err != nil {
			return false
		}
		rest := func(it *aggItem) {
			switch it.kind {
			case aggCountStar, aggCount:
				it.cnt += g.n - 1
			case aggSum:
				a := argOf[it.rowRegs[aggExprArg]-1]
				it.cnt += g.n - 1
				it.iSum += g.sums[a] - b.args[a][g.lastRow]
			case aggMin:
				it.best = Value{Typ: Int, I: g.mins[argOf[it.rowRegs[aggExprArg]-1]]}
			case aggMax:
				it.best = Value{Typ: Int, I: g.maxs[argOf[it.rowRegs[aggExprArg]-1]]}
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
		return true
	}
	segGroupBulkHits.Add(1)
	if dense {
		for i := range slots {
			if slots[i].n > 0 && !apply(lo+int64(i), &slots[i]) {
				return bulkFailed
			}
		}
		return bulkAnswered
	}
	for k, g := range sparse {
		if !apply(k, g) {
			return bulkFailed
		}
	}
	return bulkAnswered
}

// segGroupDenseFlat is one segment's per-group work for dense keys, written as
// one tight loop per statistic over flat per-key arrays rather than one loop
// over rows that updates a struct per row. It is used only when every summed
// column's zone map proves no sum in this segment can overflow, so the loops
// carry no check; it reports false otherwise, and the checked loop runs.
// Results land in slots exactly as the row loop leaves them.
func segGroupDenseFlat(s *segment, argCols []int, keys []int64, args [][]int64,
	needSum, needMM []bool, lo int64, si int, slots []segGroupAcc) bool {
	for a, need := range needSum {
		if !need {
			continue
		}
		z, ok := s.intZones(argCols[a])
		if !ok || !segSumCannotOverflow(s.nRows, z.min, z.max) {
			return false
		}
	}
	nr := s.nRows
	keys = keys[:nr]
	width := len(slots)
	n := make([]int64, width)
	last := make([]int32, width)
	for r, k := range keys {
		i := k - lo
		n[i]++
		last[i] = int32(r)
	}
	na := len(args)
	sums := make([][]int64, na)
	mins := make([][]int64, na)
	maxs := make([][]int64, na)
	for a, col := range args {
		col = col[:nr]
		if needSum[a] {
			sm := make([]int64, width)
			for r, k := range keys {
				sm[k-lo] += col[r]
			}
			sums[a] = sm
		}
		if needMM[a] {
			mn, mx := make([]int64, width), make([]int64, width)
			for i := range mn {
				mn[i], mx[i] = math.MaxInt64, math.MinInt64
			}
			for r, k := range keys {
				v, i := col[r], k-lo
				if v < mn[i] {
					mn[i] = v
				}
				if v > mx[i] {
					mx[i] = v
				}
			}
			mins[a], maxs[a] = mn, mx
		}
	}
	for i := range width {
		if n[i] == 0 {
			continue
		}
		g := &slots[i]
		g.n, g.lastSeg, g.lastRow = n[i], si, int(last[i])
		g.sums, g.abs = make([]int64, na), make([]uint64, na)
		g.mins, g.maxs = make([]int64, na), make([]int64, na)
		for a := range na {
			g.mins[a], g.maxs[a] = math.MaxInt64, math.MinInt64
			if sums[a] != nil {
				g.sums[a] = sums[a][i]
				// The bound the zone map proved, standing in for the exact
				// sum of |v| the merge checks against.
				z, _ := s.intZones(argCols[a])
				g.abs[a] = uint64(n[i]) * max(segAbs(z.min), segAbs(z.max))
			}
			if mins[a] != nil {
				g.mins[a], g.maxs[a] = mins[a][i], maxs[a][i]
			}
		}
	}
	return true
}

func segAbs(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}
