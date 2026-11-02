package engine

import (
	"math"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// Running a compiled loop body over the columnar blocks.
//
// One mapped kernel per lowered program, cached for the process like the fixed
// ones: a program is a few hundred bytes and the same statement recompiles to
// the identical IR every time.

var segProgCache sync.Map // segProgKey -> *jit.Code

// segProgKey is a lowered body and whether it was emitted fused
// (WithoutFusion): the same body emits differently either way.
type segProgKey struct {
	low   *segLowered
	fused bool
}

// segProgKernel returns the machine code for this lowered body, emitting it on
// first use. nil when the platform or the program declines.
func segProgKernel(low *segLowered) *jit.Code {
	key := segProgKey{low, jit.Fusing()}
	if c, ok := segProgCache.Load(key); ok {
		if c == nil {
			return nil
		}
		return c.(*jit.Code)
	}
	code, err := jit.EmitProgramFlagged(low.insns, len(low.cols), low.fusable)
	if err != nil {
		segProgCache.Store(key, (*jit.Code)(nil))
		return nil
	}
	k, err := jit.Map(code)
	if err != nil {
		segProgCache.Store(key, (*jit.Code)(nil))
		return nil
	}
	actual, _ := segProgCache.LoadOrStore(key, k)
	if got := actual.(*jit.Code); got != k {
		k.Close()
		return got
	}
	return k
}

// segRunProgramAll answers every aggregate the statement computes from the
// segments, or reports false to decline -- in which case the loop the
// recogniser left in the program runs.
//
// One lowered program per aggregate, all sharing the same body and differing
// only in the accumulate at the end, so N aggregates are N passes over the same
// int64 blocks. Two passes over a column beat one pass through the VDBE by
// enough that a shared-accumulator IR is not worth the complexity: the whole
// point of "SELECT min(v), max(v) FROM t" being here is that it measured 4.49x
// SLOWER than C SQLite while every single-aggregate form measured faster.
func (m *vdbe) segRunProgramAll(p *ReadOnlyPager, tbl *resolvedTable, plan *segProgPlan) ([]Value, bool) {
	rootPage, ipkCol := tbl.root, tbl.ipkIndex
	if plan == nil || plan.src == nil {
		return nil, false
	}
	kinds, ok := m.segKindsOf(p, tbl, plan.src)
	if !ok {
		return nil, false
	}
	lows := plan.forKinds(kinds)
	if len(lows) == 0 {
		return nil, false
	}
	for _, low := range lows {
		if low != nil && (lowReadsVirtual(tbl.cols, low) || lowReadsVirtual(tbl.cols, low.textVar)) {
			return nil, false
		}
	}
	out := make([]Value, len(lows))
	for i, low := range lows {
		v, ok := m.segRunOne(p, tbl, rootPage, low, ipkCol)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// segKindsOf is which of the columns src loads hold REALs in every segment of
// the table now, and which of its parameters are bound to REALs: nil when
// none do. A column that is REAL in some segments and INTEGER in others
// declines, since one program reads it one way.
func (m *vdbe) segKindsOf(p *ReadOnlyPager, tbl *resolvedTable, src *segProgSource) (*segKinds, bool) {
	var kinds *segKinds
	if p != nil && p.segs != nil {
		segs := p.segs.byRoot[tbl.root]
		for _, c := range src.cols {
			if c == tbl.ipkIndex && c >= 0 {
				continue
			}
			isReal, isInt := false, false
			for _, s := range segs {
				if c >= len(s.cols) {
					continue
				}
				switch s.cols[c].phys {
				case PhysFloat64:
					isReal = true
				case PhysInt64:
					isInt = true
				}
			}
			if isReal && isInt {
				return nil, false
			}
			if isReal {
				if kinds == nil {
					kinds = &segKinds{realCol: map[int]bool{}, realParam: map[int]bool{}}
				}
				kinds.realCol[c] = true
			}
		}
	}
	for _, idx := range src.params {
		if idx >= 1 && idx <= len(m.params) && m.params[idx-1].Typ == Float {
			if f := m.params[idx-1].F; f != f {
				return nil, false // a NaN, which a REAL register holds as NULL
			}
			if kinds == nil {
				kinds = &segKinds{realCol: map[int]bool{}, realParam: map[int]bool{}}
			}
			kinds.realParam[idx] = true
		}
	}
	return kinds, true
}

// lowReadsVirtual reports whether low reads a VIRTUAL generated column. Its
// stored cell is not its value: a row written before the column was added has
// none, and a rewrite keeps none. Only the VDBE computes it.
func lowReadsVirtual(cols []columnInfo, low *segLowered) bool {
	return low != nil && readsVirtual(cols, low.cols)
}

// readsVirtual reports whether any of read is a VIRTUAL generated column.
func readsVirtual(cols []columnInfo, read []int) bool {
	for _, c := range read {
		if c >= 0 && c < len(cols) && cols[c].IsGenerated() && !cols[c].GeneratedStored {
			return true
		}
	}
	return false
}

// segProgRegs is low's register file with the statement's bound parameters
// in place, or false when one is not a value the program can hold.
func (m *vdbe) segProgRegs(low *segLowered) ([]int64, bool) {
	nRegs := low.nRegs
	if low.textVar != nil {
		nRegs = max(nRegs, low.textVar.nRegs)
	}
	regs := make([]int64, nRegs+2)
	// Bound parameters go into the registers the body reads them from. A
	// non-integer bound declines: every value in a compiled program is an
	// int64, and coercing here is exactly where SQLite's cross-class ordering
	// would be got wrong.
	for i, reg := range low.consts {
		idx := low.cvIndex[i]
		if idx < 1 || idx > len(m.params) || reg >= len(regs) {
			return nil, false
		}
		v := m.params[idx-1]
		if low.real[reg] {
			if v.Typ != Float {
				return nil, false
			}
			regs[reg] = int64(math.Float64bits(v.F))
			continue
		}
		if v.Typ != Int {
			return nil, false
		}
		regs[reg] = v.I
	}
	// An INTEGER a REAL comparison reads as a double must be one exactly.
	for _, idx := range low.exactParams {
		if idx < 1 || idx > len(m.params) {
			return nil, false
		}
		if v := m.params[idx-1]; v.Typ != Int || v.I > 1<<53 || v.I < -(1<<53) {
			return nil, false
		}
	}
	return regs, low.seedSorted(regs, m.params)
}

func (m *vdbe) segRunOne(p *ReadOnlyPager, tbl *resolvedTable, rootPage uint32, low *segLowered, ipkCol int) (Value, bool) {
	if p == nil || p.segs == nil || low == nil {
		return Value{}, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok {
		return Value{}, false
	}
	// A row-major delta beside the segments makes a raw block read wrong
	// (segCleanFor). It is merged instead (segDeltaSplit): the kernel runs over
	// the runs of segment rows the delta does not name, and once more over the
	// delta's own live rows laid out as blocks. Every aggregate here is an
	// integer one combined exactly across runs, so the split cannot move an
	// answer.
	var split *segDeltaSplit
	if !p.segCleanFor(rootPage) {
		// A REAL sum is rounded in row order, which the split does not keep:
		// it runs the delta's rows after every segment's.
		if low.fAcc || low.distinctReal() {
			return Value{}, false
		}
		if split, ok = p.segs.deltaSplitFor(rootPage, segs, low, ipkCol); !ok {
			return Value{}, false
		}
	}
	kern := segProgKernel(low)
	if kern == nil {
		return Value{}, false
	}
	regs, ok := m.segProgRegs(low)
	if !ok {
		return Value{}, false
	}

	// Each segment runs on its own copy of the register file (on its own
	// goroutine under WithWorkers); the results are then combined in segment
	// order, exactly as a single pass combines them.
	type segResult = segRunResult
	nSlots := len(segs)
	if split != nil {
		nSlots++ // the delta's own rows, after every segment
	}
	results := make([]segResult, nSlots)
	// A DISTINCT aggregate's values go into one set when the runs take turns,
	// and into a set per run, joined at the end, when they run at once. REALs
	// are taken in row order, the runs taking turns.
	if low.distinctReal() {
		rd := &segRealDistinct{}
		for i := range results {
			results[i].real = rd
		}
	} else if low.distinct {
		var shared *int64Set
		if segWorkers <= 1 || len(low.blocks) > 0 {
			shared = &int64Set{}
		}
		for i := range results {
			results[i].set = shared
			if shared == nil {
				results[i].set = &int64Set{}
			}
		}
	}
	// A REAL accumulator is carried from each run to the next, in row order.
	var facc [2]float64
	switch low.agg {
	case aggMin:
		facc[0] = math.Inf(1)
	case aggMax:
		facc[0] = math.Inf(-1)
	}
	// Service blocks share one window across the statement, in row order, as
	// the interpreter shares its registers: their segments run one at a time.
	var svc *segSvcRunner
	each := segEach
	if len(low.blocks) > 0 || low.fAcc || low.distinctReal() {
		if len(low.blocks) > 0 {
			svc = newSegSvcRunner(m, tbl)
		}
		each = func(n int, work func(int) bool) bool {
			for i := range n {
				if !work(i) {
					return false
				}
			}
			return true
		}
	}
	ok = each(nSlots, func(si int) bool {
		res := &results[si]
		if si == len(segs) {
			return split.runDelta(kern, low, regs, res)
		}
		s := segs[si]
		// The text variant over a segment whose text columns are clean TEXT;
		// the service-only body otherwise.
		low, kern := low, kern
		var heaps [jit.MaxProgCols][]byte
		if tv := low.textVar; tv != nil && (!tv.textLike || tv.textFold == !m.likeCaseSensitive()) {
			if tk := segProgKernel(tv); tk != nil {
				clean := true
				for i, isText := range tv.textCol {
					if !isText {
						continue
					}
					_, heap, okT := segVecColumn(s, tv.cols[i])
					if !okT {
						clean = false
						break
					}
					heaps[i] = heap
				}
				if clean {
					low, kern = tv, tk
				}
			}
		}
		blocks := make([][]int64, len(low.cols))
		isRowid := make([]bool, len(low.cols))
		for i, c := range low.cols {
			if i < len(low.realCol) && low.realCol[i] {
				f, okF := s.Float64Column(c)
				if !okF || len(f) < s.nRows {
					return false
				}
				blocks[i] = unsafe.Slice((*int64)(unsafe.Pointer(unsafe.SliceData(f))), len(f))
				continue
			}
			if i < len(low.textCol) && low.textCol[i] {
				// A text slot holds the column's cells; its heap goes in
				// ProgArgs.Heap.
				cells, _, _ := segVecColumn(s, c)
				blocks[i] = unsafe.Slice((*int64)(unsafe.Pointer(unsafe.SliceData(cells))), len(cells))
				continue
			}
			b, r, okCol := segProgColumns(s, low.cols[i:i+1], low.nullCol[i:i+1], ipkCol)
			if !okCol {
				return false
			}
			blocks[i], isRowid[i] = b[0], r[0]
		}
		// A rowid-alias column has no block of its own -- the values live in
		// the segment's rowid block -- and the program reads a flat int64
		// array, so one is materialized here. It is the only allocation on
		// this path and only for a query that names the INTEGER PRIMARY KEY.
		for i := range blocks {
			if !isRowid[i] {
				continue
			}
			buf := make([]int64, s.nRows)
			for r := 0; r < s.nRows; r++ {
				buf[r] = int64(s.Rowid(r))
			}
			blocks[i] = buf
		}
		if s.nRows == 0 {
			res.empty = true
			return true
		}
		var skip []int // positions of the rows the delta names, ascending
		if split != nil {
			skip = split.skipIn(s)
		}
		// min()/max() of a whole column, with no predicate: the zone map
		// already holds the segment's extreme, so no row is read -- unless the
		// delta replaced a row, whose value the zone map may still hold.
		if len(skip) == 0 && !low.distinct {
			if extreme, ok := segZoneExtreme(s, low); ok {
				res.out, res.rows = extreme, int64(s.nRows)
				return true
			}
		}
		for _, b := range blocks {
			if len(b) < s.nRows {
				return false
			}
		}
		// The segment's rows in runs between the skipped ones, each its own
		// kernel call on its own copy of the registers, combined as segments
		// are (segCombine).
		start := 0
		for k := 0; k <= len(skip); k++ {
			end := s.nRows
			if k < len(skip) {
				end = skip[k]
			}
			if end > start {
				local := slices.Clone(regs)
				var out, ovf int64
				args := &jit.ProgArgs{N: int64(end - start), Regs: &local[0], Out: &out, Overflow: &ovf}
				// Only a program with a REAL accumulator carries one, and its
				// segments run one at a time.
				if low.fAcc {
					args.FAcc = facc
				}
				for i, b := range blocks {
					args.Col[i] = &b[start]
					if h := heaps[i]; len(h) > 0 {
						args.Heap[i], args.HeapLen[i] = &h[0], int64(len(h))
					}
				}
				var vals []int64
				if low.distinct {
					vals = make([]int64, end-start)
					args.Sel = &vals[0]
				}
				if !segCallKernel(kern, args, svc, low.blocks, s, local) {
					return false
				}
				if low.fAcc {
					facc = args.FAcc
				}
				if low.distinct {
					res.ovf = res.ovf || ovf != 0
					res.addDistinct(vals[:out])
					start = end + 1
					continue
				}
				part := segResult{out: out, ovf: ovf != 0}
				if low.agg != aggCountStar && low.agg != aggCount {
					part.rows = local[low.rowsReg]
				}
				if !segCombine(low.agg, res, part) {
					return false
				}
			}
			start = end + 1
		}
		if len(skip) == s.nRows {
			res.empty = true
		}
		return true
	})
	if !ok {
		return Value{}, false
	}
	return segProgAnswer(low, results, facc)
}

// segProgAnswer folds the runs' results, in row order, into the aggregate's
// value.
func segProgAnswer(low *segLowered, results []segRunResult, facc [2]float64) (Value, bool) {
	if low.distinct && len(results) > 0 && results[0].real != nil {
		return results[0].real.answer(low.agg, results)
	}
	if low.distinct {
		return segDistinctAnswer(low.agg, results)
	}
	var total int64
	matched := int64(0)
	for _, res := range results {
		if res.empty {
			continue
		}
		if res.ovf {
			// sum() overflowed int64. SQLite switches to floating point there
			// and this cannot, so the whole statement goes back to the loop.
			return Value{}, false
		}
		out := res.out
		switch low.agg {
		case aggCountStar, aggCount:
			total += out
			matched += out
		case aggMin, aggMax:
			// Combine ACROSS segments: each call answered over its own rows and
			// started from its own seed, so a segment that matched nothing must
			// not vote.
			if res.rows == 0 {
				continue
			}
			if matched == 0 || (low.agg == aggMin && out < total) ||
				(low.agg == aggMax && out > total) {
				total = out
			}
			matched += res.rows
		default:
			sum := total + out
			if (out > 0 && sum < total) || (out < 0 && sum > total) {
				return Value{}, false
			}
			total = sum
			matched += res.rows
		}
	}
	if low.fAcc {
		return segFloatAnswer(low.agg, matched, facc), true
	}
	// func.c's finalizers, for the all-integer non-NULL case this path is the
	// only one that reaches: sumFinalize (2003) returns the int64 or NULL at
	// cnt==0, avgFinalize (2020) returns (double)iSum/(double)cnt or NULL at
	// cnt==0, totalFinalize (2034) returns (double)iSum and 0.0 at no rows at
	// all -- which is the one that does NOT go NULL.
	switch low.agg {
	case aggMin, aggMax:
		// minmaxFinalize (func.c:2107) returns NULL for no rows, like sum.
		if matched == 0 {
			return Value{Typ: Null}, true
		}
		return Value{Typ: Int, I: total}, true
	case aggSum:
		if matched == 0 {
			return Value{Typ: Null}, true
		}
		return Value{Typ: Int, I: total}, true
	case aggAvg:
		if matched == 0 {
			return Value{Typ: Null}, true
		}
		return Value{Typ: Float, F: float64(total) / float64(matched)}, true
	case aggTotal:
		return Value{Typ: Float, F: float64(total)}, true
	}
	return Value{Typ: Int, I: total}, true
}

// segZoneExtremeHits counts segments answered from a zone map, for tests.
var segZoneExtremeHits atomic.Int64

// segZoneExtreme answers a predicate-free min(col) or max(col) over one segment
// from its zone map. The body must hold nothing but loads of its one column and
// copies between registers, with no bound parameters, so every register it
// writes -- the accumulated one included -- holds that column's value.
func segZoneExtreme(s *segment, low *segLowered) (int64, bool) {
	n := len(low.insns)
	if (low.agg != aggMin && low.agg != aggMax) || len(low.cols) != 1 || low.nullCol[0] ||
		len(low.consts) != 0 || len(low.sorted) != 0 || n < 2 {
		return 0, false
	}
	holds := map[int]bool{} // registers holding the column's value
	for _, in := range low.insns[:n-1] {
		switch {
		case in.Op == jit.POpLoadCol && in.B == 0:
			holds[in.A] = true
		case in.Op == jit.POpLoadReg && holds[in.B]:
			holds[in.A] = true
		default:
			return 0, false
		}
	}
	acc := low.insns[n-1]
	if (acc.Op != jit.POpAccMin && acc.Op != jit.POpAccMax) || !holds[acc.A] {
		return 0, false
	}
	z, ok := s.intZones(low.cols[0])
	if !ok {
		return 0, false
	}
	segZoneExtremeHits.Add(1)
	if low.agg == aggMin {
		return z.min, true
	}
	return z.max, true
}

// segRunResult is one kernel run's answer: a segment's, a run of a segment's
// rows, or the delta's rows. rows is the count matched, for the aggregates
// whose out is not itself a count; set holds a DISTINCT aggregate's values.
type segRunResult struct {
	out, rows int64
	ovf       bool
	empty     bool
	set       *int64Set
	real      *segRealDistinct
}

// addDistinct takes the values a DISTINCT aggregate's run handed back.
func (r *segRunResult) addDistinct(vals []int64) {
	if r.real != nil {
		r.real.add(vals)
		return
	}
	r.set.addAll(vals)
}

// distinctReal reports whether low hands back REALs for a DISTINCT
// aggregate, which segRealDistinct takes in row order.
func (l *segLowered) distinctReal() bool { return l.distinct && l.real[l.sumReg] }

// segRealDistinct is a DISTINCT aggregate over REALs: the values the runs
// hand back, taken in row order, each the first time it is seen (-0.0 and
// 0.0 are one value, a NULL none), and summed as sumStep sums them.
type segRealDistinct struct {
	seen int64Set
	it   aggItem
}

func (d *segRealDistinct) add(vals []int64) {
	for i, b := range vals {
		f := math.Float64frombits(uint64(b))
		if f != f || i > 0 && b == vals[i-1] {
			continue
		}
		key := b
		if f == 0 {
			key = 0
		}
		if d.seen.insert(key) {
			d.it.cnt++
			d.it.kbnStep(f)
		}
	}
}

// answer is the aggregate over the values taken, or false when a run
// overflowed.
func (d *segRealDistinct) answer(agg aggKind, results []segRunResult) (Value, bool) {
	for _, r := range results {
		if r.ovf {
			return Value{}, false
		}
	}
	if agg == aggCount {
		return Value{Typ: Int, I: d.it.cnt}, true
	}
	return segFloatAnswer(agg, d.it.cnt, [2]float64{d.it.rSum, d.it.rErr}), true
}

// segDistinctAnswer finishes a DISTINCT count, sum, total or avg from the
// values each run handed back: their union, in which a value counts once.
// The sum is exact in any order only while the values' magnitudes sum within
// an int64: C adds them in first-occurrence order and raises "integer
// overflow" if any partial sum overflows (sumFinalize), so past that the
// statement goes back to the loop, which adds them as C does.
func segDistinctAnswer(agg aggKind, results []segRunResult) (Value, bool) {
	var all *int64Set
	for i := range results {
		r := &results[i]
		if r.ovf {
			return Value{}, false
		}
		switch {
		case r.set == nil || r.set == all:
		case all == nil:
			all = r.set
		default:
			all.union(r.set)
		}
	}
	if all == nil {
		all = &int64Set{}
	}
	var sum, mag int64
	ok := all.each(func(v int64) bool {
		if v == math.MinInt64 {
			return false
		}
		a := v
		if a < 0 {
			a = -a
		}
		if mag > math.MaxInt64-a {
			return false
		}
		mag += a
		sum += v
		return true
	})
	if !ok {
		return Value{}, false
	}
	n := all.len()
	switch agg {
	case aggCount:
		return Value{Typ: Int, I: int64(n)}, true
	case aggSum:
		if n == 0 {
			return Value{Typ: Null}, true
		}
		return Value{Typ: Int, I: sum}, true
	case aggAvg:
		if n == 0 {
			return Value{Typ: Null}, true
		}
		return Value{Typ: Float, F: float64(sum) / float64(n)}, true
	case aggTotal:
		return Value{Typ: Float, F: float64(sum)}, true
	}
	return Value{}, false
}

// segCombine folds part into acc as segRunOne's final loop folds segments:
// counts and sums add (a sum that overflows declines), min and max keep the
// extreme of the runs that matched anything.
func segCombine(agg aggKind, acc *segRunResult, part segRunResult) bool {
	if part.ovf {
		acc.ovf = true
		return true
	}
	switch agg {
	case aggCountStar, aggCount:
		acc.out += part.out
	case aggMin, aggMax:
		if part.rows == 0 {
			return true
		}
		if acc.rows == 0 || (agg == aggMin && part.out < acc.out) || (agg == aggMax && part.out > acc.out) {
			acc.out = part.out
		}
		acc.rows += part.rows
	default:
		sum := acc.out + part.out
		if (part.out > 0 && sum < acc.out) || (part.out < 0 && sum > acc.out) {
			acc.ovf = true
			return true
		}
		acc.out = sum
		acc.rows += part.rows
	}
	return true
}

// segDeltaSplitMax bounds the delta rows a split merges: past it a statement
// takes the row loop, as before, rather than many tiny kernel runs.
const segDeltaSplitMax = 4096

// segDeltaSplit is a table's delta, arranged for segRunOne: the rowids it names
// (superseded or removed), whose segment rows the kernel skips, and its live
// rows as int64 blocks in the lowered program's slot order.
type segDeltaSplit struct {
	named  map[int64]bool
	blocks [][]int64
	n      int
}

// deltaSplitFor arranges root's delta for low, or declines: a program with
// text slots or service blocks, a delta past segDeltaSplitMax, or a live row
// whose value for a slot is not what a clean int64 block would hold there.
func (src *segSource) deltaSplitFor(rootPage uint32, segs []*segment, low *segLowered, ipkCol int) (*segDeltaSplit, bool) {
	if src.delta == nil {
		return nil, false
	}
	ti, known := src.tableOf[rootPage]
	if !known || low.textVar != nil || len(low.blocks) > 0 {
		return nil, false
	}
	for _, t := range low.textCol {
		if t {
			return nil, false
		}
	}
	live, dead := src.delta.live[ti], src.delta.dead[ti]
	if len(live)+len(dead) > segDeltaSplitMax {
		return nil, false
	}
	sp := &segDeltaSplit{named: make(map[int64]bool, len(live)+len(dead)), blocks: make([][]int64, len(low.cols))}
	for rid := range dead {
		sp.named[rid] = true
	}
	rids := make([]int64, 0, len(live))
	for rid := range live {
		sp.named[rid] = true
		rids = append(rids, rid)
	}
	slices.Sort(rids) // ascending, as a scan meets them; the aggregates do not care, a reader of this might
	for i := range sp.blocks {
		sp.blocks[i] = make([]int64, len(rids))
	}
	for r, rid := range rids {
		vals := live[rid]
		for i, c := range low.cols {
			asNull := i < len(low.nullCol) && low.nullCol[i]
			isIPK := c == ipkCol && ipkCol >= 0
			switch {
			case asNull && isIPK:
				sp.blocks[i][r] = 0 // the rowid is never NULL
			case asNull:
				if c >= len(vals) {
					return nil, false
				}
				if vals[c].Typ == Null {
					sp.blocks[i][r] = 1
				}
			case isIPK:
				sp.blocks[i][r] = rid
			default:
				if c >= len(vals) || vals[c].Typ != Int {
					return nil, false
				}
				sp.blocks[i][r] = vals[c].I
			}
		}
	}
	sp.n = len(rids)
	return sp, true
}

// skipIn returns the positions in s of the rows the delta names, ascending.
// A segment's rowids are sorted, so each is a binary search.
func (sp *segDeltaSplit) skipIn(s *segment) []int {
	if len(sp.named) == 0 || s.nRows == 0 {
		return nil
	}
	lo, hi := int64(s.Rowid(0)), int64(s.Rowid(s.nRows-1))
	var out []int
	for rid := range sp.named {
		if rid < lo || rid > hi {
			continue
		}
		i := sort.Search(s.nRows, func(i int) bool { return int64(s.Rowid(i)) >= rid })
		if i < s.nRows && int64(s.Rowid(i)) == rid {
			out = append(out, i)
		}
	}
	slices.Sort(out)
	return out
}

// runDelta runs the kernel over the delta's live rows.
func (sp *segDeltaSplit) runDelta(kern *jit.Code, low *segLowered, regs []int64, res *segRunResult) bool {
	if sp.n == 0 {
		res.empty = true
		return true
	}
	local := slices.Clone(regs)
	var out, ovf int64
	args := &jit.ProgArgs{N: int64(sp.n), Regs: &local[0], Out: &out, Overflow: &ovf}
	for i, b := range sp.blocks {
		args.Col[i] = &b[0]
	}
	var vals []int64
	if low.distinct {
		vals = make([]int64, sp.n)
		args.Sel = &vals[0]
	}
	if !segCallKernel(kern, args, nil, nil, nil, local) {
		return false
	}
	if low.distinct {
		res.addDistinct(vals[:out])
	}
	res.out, res.ovf = out, ovf != 0
	if low.agg != aggCountStar && low.agg != aggCount {
		res.rows = local[low.rowsReg]
	}
	return true
}
