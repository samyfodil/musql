package engine

import (
	"cmp"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"
	"sync"
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
	cursors []int           // per level, outermost first
	edges   []segJoinEdge   // the equalities joining the levels
	preds   [][]segPlanPred // per level, its own WHERE terms
	filters [][]segJoinArg  // per level, its other terms: true or the row is skipped
	// optional is the level a LEFT JOIN makes optional -- the innermost --
	// or -1: an outer row nothing there matches still joins, with that
	// level's row all NULL.
	optional int
	args     []segJoinArg // the aggregate argument registers and their columns
	agg      *aggPlan
	seg      int // the AggStep segment selector (OpAggStep's P2)

	// A GROUP BY's: the column its one key reads, and the registers the loop
	// body loads for the row it hands the drain (OpSegJoinNext fills them).
	grouped bool
	key     segJoinArg
	loads   []segJoinArg
	// sorted: some level visits its rows in an index's order, not rowid
	// order, so the loop's order is not one the kernel knows.
	sorted bool

	// An ORDER BY ... LIMIT's: the columns of its sort key, and the bounded
	// sorter's key (sorterKeyInfo.bound rows kept).
	order []segJoinArg
	keys  *sorterKeyInfo
}

// segJoinEdge is "level a's column aCol = level b's column bCol", a column of
// -1 being the rowid.
type segJoinEdge struct {
	a, aCol, b, bCol int
}

// segJoinArg is what a register holds: column col of a level (-1 the rowid),
// or, when expr is set, an expression over one level's columns -- or over
// constants alone, level -1.
type segJoinArg struct {
	reg, level, col int
	expr            *segJoinExpr
}

// segJoinExpr is the body instructions that compute register out, in the
// body's order: column reads of one level, constants, and operations over
// them whose result depends on nothing else, so one evaluation per row of
// that level's table is every evaluation the loop makes.
type segJoinExpr struct {
	insns []Instruction
	pcs   []int // each instruction's address in the program, which its jumps name
	out   int
}

// at is where a jump to address pc goes on in the expression: its first
// instruction at or after pc, which the program would run next of those the
// expression holds, or past the last.
func (e *segJoinExpr) at(pc int) int {
	i, _ := slices.BinarySearch(e.pcs, pc)
	return i
}

// segJoinTable is one table's rows as the kernel reads them, in rowid order,
// as layered as the table is: its segments' rows, under the rows the log
// replaced or added, under a write session's own (a patch). A row is a
// segment row (pos >= 0, its index across the segments' rows) or a patch row
// (pos -1-k), read as stored and fixed up per value as the cursor fixes it.
// Its columns and rowid lookup are built when first asked for. Every
// statement that reads the table while its rows stay the same shares one
// (segJoinTableOf), so it is read-only once built but for those lazy parts,
// which mu guards.
type segJoinTable struct {
	stamp  segJoinStamp
	tbl    *resolvedTable
	ipk    int
	n      int
	rowids []int64
	pos    []int32 // nil: row r is segment row r
	segs   []*segment
	starts []int // each segment's first row
	patch  [][]Value

	// extendable: read from a write session's store whose sorted overlay
	// held overN rowids, at its reshapes count -- what extend checks.
	extendable bool
	overN      int
	reshapes   uint64

	mu     sync.Mutex
	cols   map[int][]int64
	kcols  map[int]segJoinKeyCol
	vcols  map[int][]Value
	sorted map[int]segJoinSorted
	exprs  map[segJoinExprKey][]Value
	fcols  map[int][]float64
	// lo/dense map a rowid to its row + 1 when the rowids are dense enough,
	// else sparse does.
	indexed bool
	lo      int64
	dense   []int32
	sparse  map[int64]int32
}

// segJoinStamp is what a table's rows are as of: the source and log state for
// committed rows, and a write session's store and its write counters for its
// own. Equal stamps mean the same rows.
type segJoinStamp struct {
	root    uint32
	src     *segSource
	delta   *segDeltaState
	batches int
	store   *rowStore
	writes  uint64
	swaps   uint32
}

// segJoinTableOf is cur's table, or false when the kernel cannot read it: a
// WITHOUT ROWID table, a generated column, rows a scan does not yield in
// rowid order.
func segJoinTableOf(cur *vdbeCursor) (*segJoinTable, bool) {
	if cur == nil || cur.tbl == nil || cur.pager == nil || cur.pager.segs == nil ||
		cur.tbl.withoutRowid || cur.hasGeneratedCols() {
		return nil, false
	}
	p, root := cur.pager, cur.tbl.root
	src := p.segs
	stamp := segJoinStamp{root: root, src: src}
	if src.delta != nil {
		stamp.delta, stamp.batches = src.delta, src.delta.batches
	}
	if src.isLive(root) {
		store, have, err := src.liveRows(root)
		if err != nil || !have || store == nil {
			return nil, false
		}
		stamp.store, stamp.writes, stamp.swaps = store, store.writes, store.swaps
		if b := store.seg; b != nil {
			stamp.src, stamp.delta, stamp.batches = b.memoKey()
		}
		segJoinCacheMu.Lock()
		t := store.joinTable
		segJoinCacheMu.Unlock()
		if t != nil && t.stamp == stamp && t.sameShape(cur.tbl) {
			return t, true
		}
		if t != nil && t.sameShape(cur.tbl) {
			if nt, ok := t.extend(store, stamp); ok {
				segJoinCacheMu.Lock()
				store.joinTable = nt
				segJoinCacheMu.Unlock()
				return nt, true
			}
		}
		t, ok := buildSegJoinTable(cur, stamp)
		if ok {
			segJoinCacheMu.Lock()
			store.joinTable = t
			segJoinCacheMu.Unlock()
		}
		return t, ok
	}
	segJoinCacheMu.Lock()
	t := src.joinTables[root]
	segJoinCacheMu.Unlock()
	if t != nil && t.stamp == stamp && t.sameShape(cur.tbl) {
		return t, true
	}
	t, ok := buildSegJoinTable(cur, stamp)
	if ok {
		segJoinCacheMu.Lock()
		if src.joinTables == nil {
			src.joinTables = map[uint32]*segJoinTable{}
		}
		src.joinTables[root] = t
		segJoinCacheMu.Unlock()
	}
	return t, ok
}

// segJoinCacheMu guards the tables segJoinTableOf keeps.
var segJoinCacheMu sync.Mutex

// sameShape reports whether tbl reads rows as the table t was read for does:
// each statement resolves its own, so they are compared by what a read
// depends on.
func (t *segJoinTable) sameShape(tbl *resolvedTable) bool {
	a := t.tbl
	if a == tbl {
		return true
	}
	if a.root != tbl.root || a.ipkIndex != tbl.ipkIndex || a.withoutRowid != tbl.withoutRowid || len(a.cols) != len(tbl.cols) {
		return false
	}
	for i := range a.cols {
		x, y := &a.cols[i], &tbl.cols[i]
		if x.Aff != y.Aff || x.HasDefault != y.HasDefault || x.DefaultKnown != y.DefaultKnown ||
			x.IsGenerated() != y.IsGenerated() || !valuesIdentical(x.DefaultValue, y.DefaultValue) {
			return false
		}
	}
	return true
}

// buildSegJoinTable reads cur's table in the layers a scan of it merges
// (ScanTable): the committed segments less the rows the log names, the log's
// rows, and a write session's store over both.
func buildSegJoinTable(cur *vdbeCursor, stamp segJoinStamp) (*segJoinTable, bool) {
	t := &segJoinTable{stamp: stamp, ipk: cur.tbl.ipkIndex, tbl: cur.tbl,
		cols: map[int][]int64{}, fcols: map[int][]float64{}}
	ncols := len(cur.tbl.cols)
	store := stamp.store
	src, root := cur.pager.segs, cur.tbl.root
	if store != nil {
		if b := store.seg; b == nil {
			src = nil // the session's rows are all there is
		} else if b.rp == nil || b.rp.segs == nil || b.rp.segs.isLive(b.root) {
			return nil, false
		} else {
			src, root = b.rp.segs, b.root
		}
	}
	// The committed layer: segments and the log's rows over them.
	var skips [][]int
	var dlive map[int64][]Value
	if src != nil && (store == nil || !store.baseGone) {
		t.segs = src.byRoot[root]
		var ok bool
		if skips, dlive, ok = src.segDeltaSkips(root, t.segs); !ok {
			return nil, false
		}
	}
	for _, s := range t.segs {
		if s == nil || len(s.cols) != ncols || !s.fullWidth {
			return nil, false
		}
		t.starts = append(t.starts, t.n)
		t.n += s.nRows
	}
	// The patch: the log's rows, then the session's over them, less the rows
	// the session removed, in rowid order.
	var keys []uint64
	if len(dlive) > 0 {
		keys = make([]uint64, 0, len(dlive))
		for rid := range dlive {
			keys = append(keys, uint64(rid))
		}
		radixSortRowids(keys)
	}
	var gone []int64
	patchRow := func(rid int64) []Value { return dlive[rid] }
	if store != nil {
		if len(store.m) > 0 {
			keys = mergeRowids(keys, store.overlaySorted())
		}
		for rid, g := range store.gone {
			if g {
				gone = append(gone, int64(rid))
			}
		}
		slices.Sort(gone)
		if len(gone) > 0 {
			keys = slices.DeleteFunc(keys, func(rid uint64) bool {
				_, found := slices.BinarySearch(gone, int64(rid))
				return found
			})
		}
		patchRow = func(rid int64) []Value {
			if row, ok := store.m[uint64(rid)]; ok {
				return store.resolve(row)
			}
			return dlive[rid]
		}
	}

	// Merge, in rowid order. A segment row the log or the session replaced or
	// removed gives way.
	t.rowids = make([]int64, 0, t.n+len(keys))
	if len(keys) > 0 || len(gone) > 0 || slices.ContainsFunc(skips, func(s []int) bool { return len(s) > 0 }) {
		t.pos = make([]int32, 0, cap(t.rowids))
	}
	pi, gi := 0, 0
	addPatch := func(rid int64) bool {
		row := patchRow(rid)
		if len(row) < ncols {
			// Written before an ALTER TABLE ADD COLUMN: widened, as the
			// cursor widens it.
			row = cur.normalizeFrom(true, uint64(rid), row)
		}
		t.rowids = append(t.rowids, rid)
		t.pos = append(t.pos, int32(-1-len(t.patch)))
		t.patch = append(t.patch, row)
		return len(t.patch) < math.MaxInt32
	}
	for si, s := range t.segs {
		skip := skips[si]
		sk := 0
		for i := range s.nRows {
			if sk < len(skip) && skip[sk] == i {
				sk++
				continue
			}
			rid := int64(s.Rowid(i))
			for pi < len(keys) && int64(keys[pi]) < rid {
				if !addPatch(int64(keys[pi])) {
					return nil, false
				}
				pi++
			}
			if pi < len(keys) && int64(keys[pi]) == rid {
				continue // the patch row goes out in its turn
			}
			for gi < len(gone) && gone[gi] < rid {
				gi++
			}
			if gi < len(gone) && gone[gi] == rid {
				continue
			}
			t.rowids = append(t.rowids, rid)
			if t.pos != nil {
				t.pos = append(t.pos, int32(t.starts[si]+i))
			}
		}
	}
	for ; pi < len(keys); pi++ {
		if !addPatch(int64(keys[pi])) {
			return nil, false
		}
	}
	t.n = len(t.rowids)
	if store != nil && store.overValid && len(store.over) == len(store.m) {
		t.overN, t.reshapes, t.extendable = len(store.over), store.reshapes, true
	}
	for i := 1; i < t.n; i++ {
		if t.rowids[i] <= t.rowids[i-1] {
			return nil, false
		}
	}
	return t, true
}

// extend is t with the rows store appended since t was read, or false when
// anything else may have changed: a write that was not an append, a different
// base, a rowid not past every row t holds.
func (t *segJoinTable) extend(store *rowStore, stamp segJoinStamp) (*segJoinTable, bool) {
	was := t.stamp
	was.writes = stamp.writes
	if !t.extendable || was != stamp || store.reshapes != t.reshapes || !store.overValid ||
		len(store.over) != len(store.m) || len(store.over) < t.overN {
		return nil, false
	}
	added := store.over[t.overN:]
	if len(added) == 0 || t.n > 0 && !rowidLess(uint64(t.rowids[t.n-1]), added[0]) ||
		t.n+len(added) >= math.MaxInt32 {
		return nil, false
	}
	ncols := len(t.tbl.cols)
	nt := &segJoinTable{stamp: stamp, tbl: t.tbl, ipk: t.ipk, segs: t.segs, starts: t.starts,
		extendable: true, overN: len(store.over), reshapes: store.reshapes,
		cols: map[int][]int64{}, fcols: map[int][]float64{}}
	nt.rowids = append(t.rowids[:t.n:t.n], make([]int64, 0, len(added))...)
	nt.patch = t.patch[:len(t.patch):len(t.patch)]
	if t.pos != nil {
		nt.pos = t.pos[:t.n:t.n]
	} else {
		nt.pos = make([]int32, t.n, t.n+len(added))
		for i := range nt.pos {
			nt.pos[i] = int32(i)
		}
	}
	for _, rid := range added {
		row := store.resolve(store.m[rid])
		if len(row) < ncols {
			return nil, false
		}
		nt.rowids = append(nt.rowids, int64(rid))
		nt.pos = append(nt.pos, int32(-1-len(nt.patch)))
		nt.patch = append(nt.patch, row)
	}
	nt.n = len(nt.rowids)
	// The columns t had read, each extended by the new rows.
	t.mu.Lock()
	defer t.mu.Unlock()
	for c, vals := range t.cols {
		if vals == nil {
			nt.cols[c] = nil
			continue
		}
		out := append(vals[:t.n:t.n], make([]int64, 0, len(added))...)
		for r := t.n; r < nt.n; r++ {
			v := nt.stored(-1-nt.pos[r], c, nt.rowids[r])
			if v.Typ != Int {
				out = nil
				break
			}
			out = append(out, v.I)
		}
		nt.cols[c] = out
	}
	for c, vals := range t.fcols {
		if vals == nil {
			nt.fcols[c] = nil
			continue
		}
		out := append(vals[:t.n:t.n], make([]float64, 0, len(added))...)
		for r := t.n; r < nt.n; r++ {
			v := nt.stored(-1-nt.pos[r], c, nt.rowids[r])
			if v.Typ != Float {
				out = nil
				break
			}
			out = append(out, v.F)
		}
		nt.fcols[c] = out
	}
	return nt, true
}

// mergeRowids is a and b, each ascending, merged ascending with no rowid
// twice.
func mergeRowids(a, b []uint64) []uint64 {
	out := make([]uint64, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j == len(b) || i < len(a) && rowidLess(a[i], b[j]):
			out = append(out, a[i])
			i++
		case i == len(a) || rowidLess(b[j], a[i]):
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i, j = i+1, j+1
		}
	}
	return out
}

// stored is column c of patch row k as the cursor reads it: the rowid in an
// INTEGER PRIMARY KEY stored as NULL, a REAL-affinity column's integer as
// REAL.
func (t *segJoinTable) stored(k int32, c int, rowid int64) Value {
	row := t.patch[k]
	if c >= len(row) {
		return Value{}
	}
	v := row[c]
	switch {
	case c == t.ipk && v.Typ == Null:
		return Value{Typ: Int, I: rowid}
	case v.Typ == Int && t.tbl.cols[c].Aff == affReal:
		return Value{Typ: Float, F: float64(v.I)}
	}
	return v
}

// col is column c of every row (c -1 or the INTEGER PRIMARY KEY: the rowid),
// or false unless every row holds an INTEGER there. A REAL-affinity column
// declines: its integers read as REAL.
func (t *segJoinTable) col(c int) ([]int64, bool) {
	if c < 0 || c == t.ipk {
		return t.rowids, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if v, ok := t.cols[c]; ok {
		return v, v != nil
	}
	t.cols[c] = nil
	if c >= len(t.tbl.cols) || t.tbl.cols[c].Aff == affReal {
		return nil, false
	}
	var base []int64
	if len(t.segs) > 0 {
		base = make([]int64, 0, t.starts[len(t.starts)-1]+t.segs[len(t.segs)-1].nRows)
		for _, s := range t.segs {
			vals, ok := segCleanInt64Column(s, c)
			if !ok {
				return nil, false
			}
			base = append(base, vals[:s.nRows]...)
		}
	}
	if t.pos == nil {
		t.cols[c] = base
		return base, true
	}
	out := make([]int64, t.n)
	for r, p := range t.pos {
		if p >= 0 {
			out[r] = base[p]
			continue
		}
		v := t.stored(-1-p, c, t.rowids[r])
		if v.Typ != Int {
			return nil, false
		}
		out[r] = v.I
	}
	t.cols[c] = out
	return out, true
}

// keyCol is col for a join's key: integers, NULLs allowed and marked in
// nulls (nil when there are none), anything else declining.
func (t *segJoinTable) keyCol(c int) (vals []int64, nulls []bool, ok bool) {
	if v, ok := t.col(c); ok {
		return v, nil, true
	}
	if c < 0 || c == t.ipk || c >= len(t.tbl.cols) || t.tbl.cols[c].Aff == affReal {
		return nil, nil, false
	}
	t.mu.Lock()
	k, seen := t.kcols[c]
	t.mu.Unlock()
	if seen {
		return k.vals, k.nulls, k.vals != nil
	}
	vals, nulls = make([]int64, t.n), make([]bool, t.n)
	for r := range vals {
		switch v := t.value(int32(r), c); v.Typ {
		case Int:
			vals[r] = v.I
		case Null:
			nulls[r] = true
		default:
			vals, nulls = nil, nil
		}
		if vals == nil {
			break
		}
	}
	t.mu.Lock()
	if t.kcols == nil {
		t.kcols = map[int]segJoinKeyCol{}
	}
	t.kcols[c] = segJoinKeyCol{vals, nulls}
	t.mu.Unlock()
	return vals, nulls, vals != nil
}

// segJoinKeyCol is a key column keyCol read: nil vals when it declined.
type segJoinKeyCol struct {
	vals  []int64
	nulls []bool
}

// fcol is REAL column c of every row, as the cursor reads it, or false unless
// every row holds a REAL there: a REAL, or an integer of a REAL-affinity
// column (which reads as REAL).
func (t *segJoinTable) fcol(c int) ([]float64, bool) {
	if c < 0 || c == t.ipk || c >= len(t.tbl.cols) {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if v, ok := t.fcols[c]; ok {
		return v, v != nil
	}
	t.fcols[c] = nil
	var base []float64
	for _, s := range t.segs {
		if vals, ok := s.Float64Column(c); ok {
			base = append(base, vals[:s.nRows]...)
			continue
		}
		ints, ok := segCleanInt64Column(s, c)
		if !ok || t.tbl.cols[c].Aff != affReal {
			return nil, false
		}
		for _, v := range ints[:s.nRows] {
			base = append(base, float64(v))
		}
	}
	if t.pos == nil {
		t.fcols[c] = base
		return base, true
	}
	out := make([]float64, t.n)
	for r, p := range t.pos {
		if p >= 0 {
			out[r] = base[p]
			continue
		}
		v := t.stored(-1-p, c, t.rowids[r])
		if v.Typ != Float {
			return nil, false
		}
		out[r] = v.F
	}
	t.fcols[c] = out
	return out, true
}

// value is column c (-1 the rowid) of row r, as the cursor reads it.
func (t *segJoinTable) value(r int32, c int) Value {
	if r < 0 {
		return Value{} // an optional level's NULL row
	}
	if c < 0 {
		return Value{Typ: Int, I: t.rowids[r]}
	}
	p := r
	if t.pos != nil {
		p = t.pos[r]
	}
	if p < 0 {
		return t.stored(-1-p, c, t.rowids[r])
	}
	si := sort.SearchInts(t.starts, int(p)+1) - 1
	s, row := t.segs[si], int(p)-t.starts[si]
	return segColumnValue(segColPlansFor(t.tbl), s, row, len(s.cols), uint64(t.rowids[r]), c)
}

// values is column c of every row as the cursor reads it, kept with the
// table's other readings.
func (t *segJoinTable) values(c int) []Value {
	t.mu.Lock()
	v, ok := t.vcols[c]
	t.mu.Unlock()
	if ok {
		return v
	}
	v = make([]Value, t.n)
	for r := range v {
		v[r] = t.value(int32(r), c)
	}
	t.mu.Lock()
	if t.vcols == nil {
		t.vcols = map[int][]Value{}
	}
	t.vcols[c] = v
	t.mu.Unlock()
	return v
}

// segJoinExprKey is what an expression's values over a table depend on
// besides its rows: the expression, and the LIKE case setting.
type segJoinExprKey struct {
	expr          *segJoinExpr
	caseSensitive bool
}

// sortedBy is the rows whose column c holds an integer, in order of that
// value and then of row, with the values: what finds the rows equal to a
// key. Kept with the table's other readings.
func (t *segJoinTable) sortedBy(c int) (keys []int64, rows []int32, ok bool) {
	t.mu.Lock()
	sb, seen := t.sorted[c]
	t.mu.Unlock()
	if seen {
		return sb.keys, sb.rows, sb.keys != nil
	}
	vals, nulls, ok := t.keyCol(c)
	if ok {
		for r := range vals {
			if nulls == nil || !nulls[r] {
				rows = append(rows, int32(r))
			}
		}
		slices.SortStableFunc(rows, func(a, b int32) int { return cmp.Compare(vals[a], vals[b]) })
		keys = make([]int64, len(rows))
		for i, r := range rows {
			keys[i] = vals[r]
		}
		if keys == nil {
			keys = []int64{}
		}
	}
	t.mu.Lock()
	if t.sorted == nil {
		t.sorted = map[int]segJoinSorted{}
	}
	t.sorted[c] = segJoinSorted{keys, rows}
	t.mu.Unlock()
	return keys, rows, ok
}

// segJoinSorted is a column sortedBy read: nil keys when it declined.
type segJoinSorted struct {
	keys []int64
	rows []int32
}

// index builds the rowid lookup.
func (t *segJoinTable) index() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.indexed || t.n == 0 {
		return
	}
	t.indexed = true
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

// row is the row holding rowid r, or -1. index must have run.
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

// segJoinWalk is a join opened for walking in the loop's own order: the
// first level's rows in turn, and under each, each later level's rows that
// its equality with an earlier level finds, in rowid order -- the order the
// VDBE's nested loop visits them in.
type segJoinWalk struct {
	tabs []*segJoinTable
	link []segJoinLink // per level after the first
	// pass[i][r] is whether level i's row r satisfies the level's own WHERE
	// terms; nil when it has none.
	pass [][]bool
	// optional is plan.optional: a level whose row is -1, all NULL, where
	// nothing there matched.
	optional int

	m     *vdbe
	exprs map[*segJoinExpr][]Value // each expression's value per row
}

// segJoinLink is how a level's rows are found from an earlier level's
// (from) row: by the rowid its key names (one, per row of from, the row or
// -1), or by every row whose join column equals the key (rows[lo[r]:hi[r]],
// in rowid order).
type segJoinLink struct {
	from int
	one  []int32
	// The equal rows: the from row's key (keys, nulls), looked up when the
	// walk reaches it among the level's rows sorted by the join column
	// (sorted, rows).
	keys   []int64
	nulls  []bool
	sorted []int64
	rows   []int32
}

// lv is the level a's value is per row of: a constant's, the first's.
func (w *segJoinWalk) lv(a segJoinArg) int {
	if a.level < 0 {
		return 0
	}
	return a.level
}

// exprValues is expression a computed for every row of its level's table, by
// running its instructions with the VDBE's own code on a private copy of the
// registers, or false when one cannot be: an error, which the loop will
// raise, or an opcode an expression may not hold. A constant is computed
// once.
func (w *segJoinWalk) exprValues(a segJoinArg) ([]Value, bool) {
	if v, ok := w.exprs[a.expr]; ok {
		return v, v != nil
	}
	w.exprs[a.expr] = nil
	t := w.tabs[w.lv(a)]
	// Kept with the table while its rows stay the same -- the program, and
	// so the expression, is the plan cache's -- unless it reads a parameter,
	// which the next run may bind to another value. A LIKE's answer follows
	// case_sensitive_like, which is part of the key.
	keep := !slices.ContainsFunc(a.expr.insns, func(in Instruction) bool { return in.Op == OpVariable })
	key := segJoinExprKey{a.expr, w.m.likeCaseSensitive()}
	if keep {
		t.mu.Lock()
		v, ok := t.exprs[key]
		t.mu.Unlock()
		if ok {
			w.exprs[a.expr] = v
			return v, v != nil
		}
	}
	vm := *w.m
	vm.regs = make([]Value, len(w.m.regs))
	vm.fnArgBuf, vm.likePlan = nil, nil
	n := t.n
	if a.level < 0 {
		n = 1
	}
	var row int32
	// Each column the expression reads, read once for every row.
	cols := map[int][]Value{}
	for _, op := range a.expr.insns {
		if op.Op == OpColumn && cols[op.P2] == nil {
			cols[op.P2] = t.values(op.P2)
		}
	}
	column := func(op *Instruction) Value { return cols[op.P2][row] }
	out := make([]Value, n)
	for r := range n {
		row = int32(r)
		for k := 0; k < len(a.expr.insns); {
			op := &a.expr.insns[k]
			switch {
			case op.Op == OpRowid:
				vm.regs[op.P2] = t.value(row, -1)
			case op.Op == OpGoto:
				k = a.expr.at(op.P2)
				continue
			default:
				taken, ok := vm.segEvalInsn(op, column)
				if !ok {
					return nil, false
				}
				if taken {
					k = a.expr.at(op.P2)
					continue
				}
			}
			k++
		}
		out[r] = vm.regs[a.expr.out]
		// The value may point into the private registers' scratch: kept,
		// it must be its own.
		out[r] = copyValue(out[r])
	}
	if a.level < 0 {
		one := out[0]
		out = make([]Value, t.n)
		for i := range out {
			out[i] = one
		}
	}
	w.exprs[a.expr] = out
	if keep {
		t.mu.Lock()
		if t.exprs == nil {
			t.exprs = map[segJoinExprKey][]Value{}
		}
		t.exprs[key] = out
		t.mu.Unlock()
	}
	return out, true
}

// ints is a's value per row of its level as integers, or false unless every
// one is an INTEGER.
func (w *segJoinWalk) ints(a segJoinArg) ([]int64, bool) {
	if a.expr == nil {
		return w.tabs[a.level].col(a.col)
	}
	vals, ok := w.exprValues(a)
	if !ok {
		return nil, false
	}
	out := make([]int64, len(vals))
	for i, v := range vals {
		if v.Typ != Int {
			return nil, false
		}
		out[i] = v.I
	}
	return out, true
}

// nullable is a's value per row as integers (or, with real, REALs) with
// its NULLs marked, or false unless every value is one or the other.
func (w *segJoinWalk) nullable(a segJoinArg, real bool) (ints []int64, flts []float64, nulls []bool, ok bool) {
	if real {
		if v, ok := w.floats(a); ok {
			return nil, v, nil, true
		}
	} else if v, ok := w.ints(a); ok {
		return v, nil, nil, true
	}
	var vals []Value
	if a.expr != nil {
		if vals, ok = w.exprValues(a); !ok {
			return nil, nil, nil, false
		}
	} else {
		vals = w.tabs[a.level].values(a.col)
	}
	nulls = make([]bool, len(vals))
	if real {
		flts = make([]float64, len(vals))
	} else {
		ints = make([]int64, len(vals))
	}
	for r, v := range vals {
		switch {
		case v.Typ == Null:
			nulls[r] = true
		case real && v.Typ == Float:
			flts[r] = v.F
		case !real && v.Typ == Int:
			ints[r] = v.I
		default:
			return nil, nil, nil, false
		}
	}
	return ints, flts, nulls, true
}

// floats is ints for REAL values.
func (w *segJoinWalk) floats(a segJoinArg) ([]float64, bool) {
	if a.expr == nil {
		return w.tabs[a.level].fcol(a.col)
	}
	vals, ok := w.exprValues(a)
	if !ok {
		return nil, false
	}
	out := make([]float64, len(vals))
	for i, v := range vals {
		if v.Typ != Float {
			return nil, false
		}
		out[i] = v.F
	}
	return out, true
}

// value is a's value in joined row at.
func (w *segJoinWalk) value(a segJoinArg, at []int32) Value {
	lv := w.lv(a)
	if a.expr == nil {
		return w.tabs[lv].value(at[lv], a.col)
	}
	vals, ok := w.exprValues(a)
	if !ok || at[lv] < 0 {
		return Value{}
	}
	return vals[at[lv]]
}

// openSegJoin opens plan's join, or reports false.
func (m *vdbe) openSegJoin(plan *segJoinPlan) (*segJoinWalk, bool) {
	k := len(plan.cursors)
	w := &segJoinWalk{tabs: make([]*segJoinTable, k), link: make([]segJoinLink, k),
		m: m, exprs: map[*segJoinExpr][]Value{}, optional: plan.optional}
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
	// A LEFT JOIN's optional level is the innermost: a NULL row reaches no
	// level after it.
	if o := plan.optional; o >= 0 && o != k-1 {
		return nil, false
	}
	w.pass = make([][]bool, k)
	for i, preds := range plan.preds {
		if len(preds) == 0 {
			continue
		}
		// The bounds, resolved now that the parameters are bound, through the
		// comparison's affinity as the loop applies it.
		resolved, ok := m.segPlanPreds(&segFilterPlan{preds: preds})
		if !ok {
			return nil, false
		}
		w.pass[i] = w.tabs[i].passing(preds, resolved)
	}
	for i, fs := range plan.filters {
		for _, f := range fs {
			if w.pass[i] == nil {
				w.pass[i] = make([]bool, w.tabs[i].n)
				for r := range w.pass[i] {
					w.pass[i][r] = true
				}
			}
			// The term's value per row, true as the loop's IfNot reads it:
			// not NULL, and true.
			var vals []Value
			if f.expr != nil {
				f.level = i
				var ok bool
				if vals, ok = w.exprValues(f); !ok {
					return nil, false
				}
			} else {
				vals = w.tabs[i].values(f.col)
			}
			for r, v := range vals {
				if w.pass[i][r] && (v.Typ == Null || !isTruthy(v)) {
					w.pass[i][r] = false
				}
			}
		}
	}
	// Each later level is found from an earlier one by its recheck's
	// equality, exactly one.
	for i := 1; i < k; i++ {
		var e segJoinEdge
		n := 0
		for _, x := range plan.edges {
			if x.b == i && x.a < i {
				e, n = x, n+1
			} else if x.a == i && x.b < i {
				e, n = segJoinEdge{a: x.b, aCol: x.bCol, b: x.a, bCol: x.aCol}, n+1
			}
		}
		if n != 1 {
			return nil, false
		}
		keys, nulls, ok := w.tabs[e.a].keyCol(e.aCol)
		if !ok {
			return nil, false
		}
		l := segJoinLink{from: e.a}
		t := w.tabs[i]
		if e.bCol < 0 || e.bCol == t.ipk {
			t.index()
			l.one = make([]int32, len(keys))
			for r, key := range keys {
				// A NULL key equals no rowid: the loop's equality is not true.
				if nulls != nil && nulls[r] {
					l.one[r] = -1
					continue
				}
				l.one[r] = t.row(key)
			}
		} else {
			sk, rows, ok := t.sortedBy(e.bCol)
			if !ok {
				return nil, false
			}
			l.keys, l.nulls, l.sorted, l.rows = keys, nulls, sk, rows
		}
		w.link[i] = l
	}
	return w, true
}

// each calls fn once per joined row, in the loop's order, with at holding
// each level's row (-1 for the optional level's NULL row); fn returning false
// stops the walk, and each reports whether it ran to the end.
func (w *segJoinWalk) each(at []int32, fn func() bool) bool {
	k := len(w.tabs)
	var visit func(i int) bool
	visit = func(i int) bool {
		if i == k {
			return fn()
		}
		pass := w.pass[i]
		if i == 0 {
			for r := range w.tabs[0].n {
				if pass == nil || pass[r] {
					at[0] = int32(r)
					if !visit(1) {
						return false
					}
				}
			}
			return true
		}
		l := &w.link[i]
		matched := false
		if from := at[l.from]; from >= 0 {
			if l.one != nil {
				if r := l.one[from]; r >= 0 && (pass == nil || pass[r]) {
					matched, at[i] = true, r
					if !visit(i + 1) {
						return false
					}
				}
			} else if l.nulls == nil || !l.nulls[from] {
				// A NULL key equals nothing; any other, the rows holding it.
				key := l.keys[from]
				lo, _ := slices.BinarySearch(l.sorted, key)
				for j := lo; j < len(l.sorted) && l.sorted[j] == key; j++ {
					if r := l.rows[j]; pass == nil || pass[r] {
						matched, at[i] = true, r
						if !visit(i + 1) {
							return false
						}
					}
				}
			}
		}
		if !matched && i == w.optional {
			at[i] = -1 // nothing matched: the row is all NULL
			return visit(i + 1)
		}
		return true
	}
	return visit(0)
}

// passing is, per row, whether it satisfies every one of preds, whose
// bounds are resolved: the column's value through the comparison's affinity,
// as the loop applies it to both sides, compared as the loop compares. A NULL
// satisfies nothing.
func (t *segJoinTable) passing(preds []segPlanPred, resolved []segPred) []bool {
	pass := make([]bool, t.n)
	for r := range pass {
		pass[r] = true
	}
	for j, pr := range resolved {
		aff := preds[j].Aff
		if ints, ok := t.col(pr.Col); ok && pr.Val.Typ == Int && (aff == affNone || aff == affNumeric) {
			for r, v := range ints {
				if pass[r] {
					pass[r] = pr.Op.satisfied(cmp.Compare(v, pr.Val.I))
				}
			}
			continue
		}
		for r := range pass {
			if !pass[r] {
				continue
			}
			v := t.value(int32(r), pr.Col)
			if aff != affNone && !affinityIsIdentity(v.Typ, aff) {
				v = applyAffinityToValue(v, aff)
			}
			pass[r] = v.Typ != Null && pr.Op.satisfied(compareValues(v, pr.Val))
		}
	}
	return pass
}

// segJoinSums is what a group's rows add up to, per integer argument.
type segJoinSums struct {
	n    int64
	nn   []int64 // per argument, the rows where it is not NULL
	sums []int64
	abs  []uint64 // sum of |v|: no partial sum, in any order, can pass it
	mins []int64
	maxs []int64
	tops []uint64 // the largest |v|
	ors  []uint64 // every |v| or-ed: its trailing zeros are the smallest unit
}

func newSegJoinSums(na int) *segJoinSums {
	s := &segJoinSums{nn: make([]int64, na), sums: make([]int64, na), abs: make([]uint64, na), mins: make([]int64, na), maxs: make([]int64, na),
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
		r := at[args.levels[a]]
		if r < 0 || args.nulls[a] != nil && args.nulls[a][r] {
			continue // an optional level's NULL row, or a NULL
		}
		s.nn[a]++
		if vals == nil {
			continue
		}
		v := vals[r]
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
	nulls  [][]bool    // the rows where an argument is NULL; nil for none
	levels []int
	argOf  map[int]int // the step's argument register -> its index
	floats bool
	// keep: a group keeps its joined rows, which a REAL argument replays in
	// order and a DISTINCT one dedupes.
	keep bool
}

// segJoinArgsOf reads plan's arguments, or reports false when an aggregate is
// one the kernel does not model.
func segJoinArgsOf(plan *segJoinPlan, w *segJoinWalk) (*segJoinArgs, bool) {
	n := len(plan.args)
	args := &segJoinArgs{ints: make([][]int64, n), flts: make([][]float64, n), nulls: make([][]bool, n), levels: make([]int, n), argOf: map[int]int{}}
	for a, arg := range plan.args {
		// An expression over the optional level's NULL row need not be NULL
		// (coalesce): such an argument declines.
		if arg.expr != nil && w.lv(arg) == w.optional {
			return nil, false
		}
		args.levels[a] = w.lv(arg)
		args.argOf[arg.reg] = a
		if vals, _, nulls, ok := w.nullable(arg, false); ok {
			args.ints[a], args.nulls[a] = vals, nulls
			continue
		}
		_, vals, nulls, ok := w.nullable(arg, true)
		if !ok {
			return nil, false
		}
		args.flts[a], args.nulls[a], args.floats = vals, nulls, true
	}
	if args.floats && plan.sorted {
		return nil, false // a REAL sum replays the loop's order, unknown here
	}
	args.keep = args.keep || args.floats
	for _, it := range aggPlanTemplates(plan.agg) {
		sumKind := it.kind == aggSum || it.kind == aggTotal || it.kind == aggAvg
		// An order-strict sum checks at finalize that no order could change
		// it, from a window over its values that rest keeps up to date. A
		// DISTINCT count or sum takes INTEGER arguments, which are equal only
		// when identical.
		if it.filter != nil || it.orderBy != nil || it.orderStrict && !sumKind || it.stepSeg != plan.seg {
			return nil, false
		}
		if it.distinct {
			a, ok := args.argOf[it.rowRegs[aggExprArg]-1]
			if !ok || args.ints[a] == nil || !sumKind && it.kind != aggCount {
				return nil, false
			}
			args.keep = true
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
	// The groups, by an INTEGER key's value, or by any other key's canonical
	// bytes: the encoding hashed GROUP BY keys by, equal exactly when the
	// keys compare equal under BINARY (the recognisers admit no other
	// collation).
	var keys []int64
	byValue := false
	keyLv := w.lv(plan.key)
	if plan.grouped {
		if plan.key.expr != nil && keyLv == w.optional {
			return nil, false // see segJoinArgsOf
		}
		// A key on the optional level is NULL in its NULL rows, which only the
		// canonical bytes can say.
		if keys, ok = w.ints(plan.key); !ok || keyLv == w.optional {
			keys, byValue = nil, true
		}
	}
	byKey := map[int64]int{}
	// An INTEGER key whose values span little more than the table's rows
	// finds its group in an array instead.
	var dense []int32
	var lo int64
	if len(keys) > 0 {
		lo = slices.Min(keys)
		if span := uint64(slices.Max(keys) - lo); span < uint64(4*len(keys)+1024) {
			dense = make([]int32, span+1)
		}
	}
	byBytes := map[string]int{}
	var buf []byte
	one := make([]Value, 1)
	var groups []segJoinGroup
	k := len(plan.cursors)
	at := make([]int32, k)
	if !w.each(at, func() bool {
		var gi int
		var seen bool
		if byValue {
			one[0] = w.value(plan.key, at)
			buf = hashAggKeyBytes(buf[:0], one)
			gi, seen = byBytes[string(buf)]
			if !seen {
				byBytes[string(buf)] = len(groups)
			}
		} else if keys == nil {
			gi, seen = 0, len(groups) > 0 // no GROUP BY: one group
		} else if key := keys[at[keyLv]]; dense != nil {
			gi, seen = int(dense[key-lo])-1, dense[key-lo] != 0
			if !seen {
				dense[key-lo] = int32(len(groups) + 1)
			}
		} else {
			gi, seen = byKey[key]
			if !seen {
				byKey[key] = len(groups)
			}
		}
		switch {
		case !seen:
			gi = len(groups)
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
		if args.keep {
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
		m.regs[l.reg] = gs.w.value(l, g.first)
	}
}

// loadArgs puts group g's first joined row's arguments into the registers the
// step reads them from.
func (gs *segJoinGroups) loadArgs(m *vdbe, g *segJoinGroup) {
	for _, arg := range gs.plan.args {
		m.regs[arg.reg] = gs.w.value(arg, g.first)
	}
}

// rest adds group g's other rows to the accumulators its first row was just
// stepped into.
func (gs *segJoinGroups) rest(m *vdbe, g *segJoinGroup) {
	args, s := gs.args, g.sums
	k := len(gs.plan.cursors)
	// firstNN is 1 when the row already stepped holds argument a, 0 when it
	// is that row's NULL.
	// null is whether argument a is NULL in level row r: the optional
	// level's NULL row, or a NULL value.
	null := func(a int, r int32) bool {
		return r < 0 || args.nulls[a] != nil && args.nulls[a][r]
	}
	// firstNN is 1 when the row already stepped holds argument a, 0 when it
	// is NULL there.
	firstNN := func(a int) int64 {
		if null(a, g.first[args.levels[a]]) {
			return 0
		}
		return 1
	}
	rest := func(it *aggItem) {
		if it.kind == aggCountStar {
			it.cnt += s.n - 1
			return
		}
		a := args.argOf[it.rowRegs[aggExprArg]-1]
		if it.distinct {
			// Each value the group's other rows bring that it has not seen,
			// as step takes it: kept, counted, summed.
			ints, lv := args.ints[a], args.levels[a]
			// Values the group spans narrowly are marked in a bitmap.
			lo, hi := s.mins[a], s.maxs[a]
			var bits []uint64
			var seen map[int64]bool
			if span := uint64(hi - lo); hi >= lo && span < uint64(64*len(g.rows)+4096) {
				bits = make([]uint64, span/64+1)
			} else {
				seen = make(map[int64]bool, len(it.seen))
			}
			has := func(v int64) bool {
				if bits != nil {
					i := uint64(v - lo)
					return bits[i/64]&(1<<(i%64)) != 0
				}
				return seen[v]
			}
			mark := func(v int64) {
				if bits != nil {
					i := uint64(v - lo)
					bits[i/64] |= 1 << (i % 64)
					return
				}
				seen[v] = true
			}
			for _, v := range it.seen {
				mark(v.I)
			}
			var first int64
			if !null(a, g.first[lv]) {
				first = ints[g.first[lv]]
			}
			skipped := false
			for r := 0; r < len(g.rows); r += k {
				if null(a, g.rows[r+lv]) {
					continue // NULL: no value
				}
				v := ints[g.rows[r+lv]]
				if !skipped && v == first && slices.Equal(g.rows[r:r+k], g.first) {
					skipped = true // the row already stepped
					continue
				}
				if has(v) {
					continue
				}
				mark(v)
				it.seen = append(it.seen, Value{Typ: Int, I: v})
				it.cnt++
				if it.kind != aggCount {
					if it.orderStrict {
						it.sumWatch(true, v, 0)
					}
					it.sumStep(true, v, float64(v))
				}
			}
			return
		}
		if fl := args.flts[a]; fl != nil {
			if it.kind == aggCount {
				it.cnt += s.nn[a] - firstNN(a)
				return
			}
			// The rows after the first, through what step does with each.
			for r := k; r < len(g.rows); r += k {
				if null(a, g.rows[r+args.levels[a]]) {
					continue // NULL: no value
				}
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
			it.cnt += s.nn[a] - firstNN(a)
		case aggSum, aggTotal, aggAvg:
			it.cnt += s.nn[a] - firstNN(a)
			if fr := g.first[args.levels[a]]; !null(a, fr) {
				it.iSum += s.sums[a] - args.ints[a][fr]
			} else {
				it.iSum += s.sums[a]
			}
			if it.orderStrict && s.ors[a] != 0 {
				// What sumWatch over every value would have kept: the
				// largest magnitude and the smallest power of two dividing
				// them all.
				it.sumWatch(true, int64(s.tops[a]), 0)
				it.sumWatch(true, int64(1)<<bits.TrailingZeros64(s.ors[a]), 0)
			}
		case aggMin, aggMax:
			if s.nn[a] == 0 {
				return
			}
			it.best, it.haveBest = Value{Typ: Int, I: s.mins[a]}, true
			if it.kind == aggMax {
				it.best.I = s.maxs[a]
			}
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
		keyVals[0] = gs.w.value(plan.key, g.first)
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
		case g.first[lv] < 0:
			rowids[i] = Value{} // the optional level's NULL row
		default:
			rowids[i] = Value{Typ: Int, I: gs.w.tabs[lv].rowids[g.first[lv]]}
		}
	}
	return true
}

// segJoinTopOf is an ORDER BY ... LIMIT's candidates: the joined rows whose
// sort key is no later than the bound-th smallest, ties included, in the
// loop's order. Handed to the bounded sorter in that order, they leave it
// holding exactly the rows every joined row would have: the rows it keeps
// are these, and among equal keys it keeps the earliest inserted, whose
// order is the loop's. Each is a group of one row, which OpSegJoinNext loads.
func (m *vdbe) segJoinTopOf(plan *segJoinPlan) (*segJoinGroups, bool) {
	ki := plan.keys
	if ki == nil || ki.bound <= 0 || ki.nKey != len(plan.order) || plan.sorted || plan.optional >= 0 {
		return nil, false
	}
	w, ok := m.openSegJoin(plan)
	if !ok {
		return nil, false
	}
	if gs, ok := segJoinTopNumeric(w, plan); ok {
		return gs, true
	}
	cmpKeys := &vdbeSorter{keyInfo: *ki, enc: m.encoding()}
	key := func(at []int32, into []Value) []Value {
		into = into[:0]
		for _, o := range plan.order {
			into = append(into, w.value(o, at))
		}
		return into
	}
	// The bound-th smallest key: a max-heap of the smallest seen.
	k := len(plan.cursors)
	at := make([]int32, k)
	var heap [][]Value
	less := func(a, b []Value) bool {
		l, _ := cmpKeys.keyLess(a, b)
		return l
	}
	var buf []Value
	w.each(at, func() bool {
		buf = key(at, buf)
		if len(heap) < ki.bound {
			heap = append(heap, slices.Clone(buf))
			for i := len(heap) - 1; i > 0; {
				p := (i - 1) / 2
				if !less(heap[p], heap[i]) {
					break
				}
				heap[p], heap[i] = heap[i], heap[p]
				i = p
			}
			return true
		}
		if !less(buf, heap[0]) {
			return true
		}
		copy(heap[0], buf)
		for i := 0; ; {
			c := 2*i + 1
			if c >= len(heap) {
				break
			}
			if c+1 < len(heap) && less(heap[c], heap[c+1]) {
				c++
			}
			if !less(heap[i], heap[c]) {
				break
			}
			heap[i], heap[c] = heap[c], heap[i]
			i = c
		}
		return true
	})
	gs := &segJoinGroups{w: w, plan: plan}
	if len(heap) == 0 {
		return gs, true
	}
	limit := heap[0]
	w.each(at, func() bool {
		buf = key(at, buf)
		if !less(limit, buf) {
			gs.groups = append(gs.groups, segJoinGroup{first: slices.Clone(at)})
		}
		return true
	})
	sort.Slice(gs.groups, func(i, j int) bool { return loopBefore(gs.groups[i].first, gs.groups[j].first) })
	return gs, true
}

// segJoinTopNumeric is segJoinTopOf for one sort key over a clean INTEGER
// column, which orders as its values do under any collation and holds no
// NULL: one walk, the bound-th smallest found in a heap of integers.
func segJoinTopNumeric(w *segJoinWalk, plan *segJoinPlan) (*segJoinGroups, bool) {
	ki := plan.keys
	if ki.nKey != 1 {
		return nil, false
	}
	o := plan.order[0]
	vals, ok := w.ints(o)
	if !ok {
		return nil, false
	}
	// Ordered descending, the smallest key is the largest value.
	desc := ki.desc[0]
	before := func(a, b int64) bool {
		if desc {
			return a > b
		}
		return a < b
	}
	k := len(plan.cursors)
	at := make([]int32, k)
	var keys []int64
	var rows []int32
	w.each(at, func() bool {
		keys = append(keys, vals[at[w.lv(o)]])
		rows = append(rows, at...)
		return true
	})
	gs := &segJoinGroups{w: w, plan: plan}
	if len(keys) == 0 {
		return gs, true
	}
	// A max-heap, under before, of the bound smallest keys.
	heap := make([]int64, 0, min(ki.bound, len(keys)))
	for _, v := range keys {
		if len(heap) < ki.bound {
			heap = append(heap, v)
			for i := len(heap) - 1; i > 0; {
				p := (i - 1) / 2
				if !before(heap[p], heap[i]) {
					break
				}
				heap[p], heap[i] = heap[i], heap[p]
				i = p
			}
			continue
		}
		if !before(v, heap[0]) {
			continue
		}
		heap[0] = v
		for i := 0; ; {
			c := 2*i + 1
			if c >= len(heap) {
				break
			}
			if c+1 < len(heap) && before(heap[c], heap[c+1]) {
				c++
			}
			if !before(heap[i], heap[c]) {
				break
			}
			heap[i], heap[c] = heap[c], heap[i]
			i = c
		}
	}
	limit := heap[0]
	for i, v := range keys {
		if !before(limit, v) {
			gs.groups = append(gs.groups, segJoinGroup{first: rows[i*k : i*k+k : i*k+k]})
		}
	}
	sort.Slice(gs.groups, func(i, j int) bool { return loopBefore(gs.groups[i].first, gs.groups[j].first) })
	return gs, true
}
