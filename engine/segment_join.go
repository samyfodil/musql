package engine

import (
	"bytes"
	"cmp"
	"encoding/binary"
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
	// optional has bit i set for a level a LEFT JOIN makes optional: an
	// outer row nothing there matches still joins, with that level's row all
	// NULL.
	optional uint64
	args     []segJoinArg // the aggregate argument registers and their columns
	agg      *aggPlan
	seg      int // the AggStep segment selector (OpAggStep's P2)

	// A GROUP BY's: the column its one key reads, and the registers the loop
	// body loads for the row it hands the drain (OpSegJoinNext fills them).
	grouped bool
	key     segJoinArg
	loads   []segJoinArg
	// crosses is each level's terms over it and the levels before it, true
	// or the joined row is skipped; tags each level's MULTI_OR order, nil
	// for none.
	crosses [][]*segJoinExpr
	tags    []*segJoinTags
	// volatiles is a top-k record's volatile values, which the walk computes
	// on every joined row, in the loop's order, for the body's copy to read.
	volatiles []segJoinArg
	// drainCalls: the volatile calls are a GROUP BY drain's, made in its
	// order -- each group's first row by the drain itself, the others by
	// the kernel's rest after it, row by row (fullStep) -- not the scan's.
	drainCalls bool
	// fullStep: every joined row goes through the aggregate's own step with
	// its whole row (segJoinFullStep), rather than the kernel adding its
	// arguments.
	fullStep bool
	// firstRow: only bare columns read the row, from each group's first
	// joined row (segJoinFirstRow), which is stepped with it.
	firstRow bool
	// keyColl is the GROUP BY key's collation when it is not BINARY:
	// NOCASE or RTRIM.
	keyColl string
	// segs is every segment of agg the body steps, seg first; nil for seg
	// alone.
	segs []int
	// orders is the loop's AutoIndexOrders: a level visiting its rows in an
	// index's order rather than rowid order.
	orders []segJoinOrder

	// An ORDER BY ... LIMIT's: the columns of its sort key, and the bounded
	// sorter's key (sorterKeyInfo.bound rows kept).
	order []segJoinArg
	keys  *sorterKeyInfo
}

// argsInCallOrder is the arguments, those that call a volatile function in
// the order the loop makes their first such call -- by address -- and the
// rest after them.
func (p *segJoinPlan) argsInCallOrder() []segJoinArg {
	first := func(a segJoinArg) int {
		if a.expr == nil || !a.expr.volatile {
			return math.MaxInt
		}
		for k, in := range a.expr.insns {
			if segJoinVolatileInsn(in) {
				return a.expr.pcs[k]
			}
		}
		return math.MaxInt
	}
	args := slices.Clone(p.args)
	slices.SortStableFunc(args, func(x, y segJoinArg) int { return cmp.Compare(first(x), first(y)) })
	return args
}

// volatile reports whether an argument or the key calls a volatile
// function.
func (p *segJoinPlan) volatile() bool {
	for _, a := range p.args {
		if a.expr != nil && a.expr.volatile {
			return true
		}
	}
	return p.key.expr != nil && p.key.expr.volatile
}

// allSegs is every segment the body steps.
func (p *segJoinPlan) allSegs() []int {
	if p.segs == nil {
		return []int{p.seg}
	}
	return p.segs
}

// steps reports whether the body steps segment seg.
func (p *segJoinPlan) steps(seg int) bool { return slices.Contains(p.allSegs(), seg) }

// segJoinCollKey is key, one value, as values equal exactly when it equals
// another under coll: for TEXT under NOCASE, its ASCII-lowered bytes up to
// and with its first NUL, where nocaseCompare stops, and its length, which
// breaks the tie; under RTRIM, its bytes without trailing spaces. Any other
// value, and any value under BINARY, is itself.
func segJoinCollKey(key []Value, coll string) []Value {
	v := key[0]
	if coll == "" || v.Typ != Text {
		return key
	}
	b := v.S
	switch coll {
	case "NOCASE":
		if i := bytes.IndexByte(b, 0); i >= 0 {
			b = b[:i+1]
		}
		f := make([]byte, len(b))
		for i, c := range b {
			f[i] = asciiLowerByte(c)
		}
		return []Value{{Typ: Text, S: f}, {Typ: Int, I: int64(len(v.S))}}
	case "RTRIM":
		return []Value{{Typ: Text, S: rtrimTrailingSpaces(b)}}
	}
	return key
}

// segJoinOrder is an AutoIndexOrder: cursor's rows visited in key's order.
type segJoinOrder struct {
	cursor int
	key    *autoIndexKey
}

// segJoinEdge is "level a's column aCol = level b's column bCol", a column of
// -1 being the rowid.
type segJoinEdge struct {
	a, b       int
	aArg, bArg segJoinArg // each side's value: a column, or an expression over its level's
}

// segJoinArg is what a register holds: column col of a level (-1 the rowid),
// or, when expr is set, an expression over one level's columns -- or over
// constants alone, level -1.
type segJoinArg struct {
	reg, level, col int
	expr            *segJoinExpr
	// guard, when set, is the register (+1) whose truth the value is
	// written under: on a row where it is not true the register keeps
	// another row's value, so only a reader that reads it under the same
	// truth -- an aggregate whose FILTER that register is -- may use it.
	guard int
}

// segJoinExpr is the body instructions that compute register out, in the
// body's order: column reads of one level, constants, and operations over
// them whose result depends on nothing else, so one evaluation per row of
// that level's table is every evaluation the loop makes.
type segJoinExpr struct {
	insns []Instruction
	pcs   []int // each instruction's address in the program, which its jumps name
	out   int
	// volatile: it calls random() or another function whose value is not a
	// function of its arguments, so each joined row's value is computed
	// once, in the loop's order, and kept (segJoinWalk.memo).
	volatile bool
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

// sortedByValues is sortedBy over an expression's values, for one walk.
func (w *segJoinWalk) sortedByValues(a segJoinArg) (keys []int64, rows []int32, ok bool) {
	vals, _, nulls, ok := w.nullable(a, false)
	if !ok {
		return nil, nil, false
	}
	for r := range vals {
		if nulls == nil || !nulls[r] {
			rows = append(rows, int32(r))
		}
	}
	slices.SortStableFunc(rows, func(x, y int32) int { return cmp.Compare(vals[x], vals[y]) })
	keys = make([]int64, len(rows))
	for i, r := range rows {
		keys[i] = vals[r]
	}
	return keys, rows, true
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
	// optional is plan.optional: the levels whose row is -1, all NULL,
	// where nothing there matched.
	optional uint64

	m     *vdbe
	exprs map[*segJoinExpr][]Value // each expression's value per row
	// nullExprs is each expression's value on the optional level's NULL row.
	nullExprs map[*segJoinExpr]Value

	// crosses and tags are plan's; levelOf maps a cursor to its level, for
	// a run over several; tagKeys, per tagged level, each pass's index key
	// per row of the level's table. failed is set when a run could not be
	// evaluated: whatever the walk produced is then not the loop's.
	crosses [][]*segJoinExpr
	tags    []*segJoinTags
	levelOf map[int]int
	tagKeys [][]func(i, k int) Value
	vm      *vdbe
	failed  bool
	calls   map[string]Value // volatile calls' values, by address and joined row

	// order0 is the first level's rows in the order the loop visits them,
	// nil for rowid order; rank, per level, each row's place in the order
	// the loop visits that level's rows, nil for rowid order.
	order0 []int32
	rank   [][]int32
}

// segJoinScratch is a cursor like cur holding t's rows in rowid order, each
// as the cursor reads it (t.value), for the cursor's own ordering code to run
// on as it runs on the real one.
func segJoinScratch(t *segJoinTable, cur *vdbeCursor, m *vdbe) *vdbeCursor {
	ncol := len(t.tbl.cols)
	rows := make([][]Value, t.n)
	rowids := make([]uint64, t.n)
	for r := range t.n {
		row := make([]Value, ncol)
		for c := range ncol {
			row[c] = t.value(int32(r), c)
		}
		rows[r], rowids[r] = row, uint64(t.rowids[r])
	}
	return &vdbeCursor{tbl: cur.tbl, pager: cur.pager, params: m.params, rows: rows, rowids: rowids}
}

// eval is run e's value on the joined row at.
func (w *segJoinWalk) eval(e *segJoinExpr, at []int32) (Value, bool) {
	column := func(op *Instruction) Value {
		lv := w.levelOf[op.P1]
		return w.tabs[lv].value(at[lv], op.P2)
	}
	rowid := func(op *Instruction) Value {
		lv := w.levelOf[op.P1]
		if at[lv] < 0 {
			return Value{}
		}
		return Value{Typ: Int, I: w.tabs[lv].rowids[at[lv]]}
	}
	// A volatile call is made once per joined row: the first time, which
	// the walk makes the loop's order, and its value kept by the call's
	// address for any later evaluation -- and for the drain's own copy of
	// the call (OpSegJoinVolatile).
	volatile := func(k int, op *Instruction) bool {
		key := segJoinCallKey(e.pcs[k], at)
		if v, ok := w.calls[key]; ok {
			w.vm.regs[op.P3] = v
			return true
		}
		if _, ok := w.vm.segEvalInsn(op, column); !ok {
			return false
		}
		if w.calls == nil {
			w.calls = map[string]Value{}
		}
		w.calls[key] = copyValue(w.vm.regs[op.P3])
		return true
	}
	v, ok := segJoinEvalRow(w.vm, e, column, rowid, volatile)
	if !ok {
		w.failed = true
	}
	return v, ok
}

// segJoinCallKey is the key of the call at address pc on joined row at.
func segJoinCallKey(pc int, at []int32) string {
	key := make([]byte, 0, 8+4*len(at))
	key = binary.LittleEndian.AppendUint64(key, uint64(pc))
	for _, r := range at {
		key = binary.LittleEndian.AppendUint32(key, uint32(r))
	}
	return string(key)
}

// segJoinVolatileCall is OpSegJoinVolatile's: the call it stands for, at
// address pc, in plan's body copy.
type segJoinVolatileCall struct {
	plan *segJoinPlan
	pc   int
	fn   Instruction
}

// called is the value the walk's call at address pc got on joined row at.
func (w *segJoinWalk) called(pc int, at []int32) (Value, bool) {
	v, ok := w.calls[segJoinCallKey(pc, at)]
	return v, ok
}

// keep reports whether the joined row at, through level i, passes level i's
// terms over several levels.
func (w *segJoinWalk) keep(i int, at []int32) bool {
	if w.crosses == nil {
		return true
	}
	for _, e := range w.crosses[i] {
		v, ok := w.eval(e, at)
		if !ok || !isTruthy(v) {
			return false
		}
	}
	return true
}

// tagOf is the pass level i's row at[i] files under for the outer row in at:
// the first it satisfies, else one past the last.
func (w *segJoinWalk) tagOf(i int, at []int32) int {
	tg := w.tags[i]
	for g, e := range tg.passes {
		v, ok := w.eval(e, at)
		if !ok {
			return len(tg.passes)
		}
		if v.Typ != Null && isTruthy(v) {
			return g
		}
	}
	return len(tg.passes)
}

// tagCmp orders level i's rows x and y under the outer row in at as
// multiOrSortTagged does: by pass, then that pass's key, then rowid.
func (w *segJoinWalk) tagCmp(i int, at []int32, x, y int32) int {
	tg, t := w.tags[i], w.tabs[i]
	save := at[i]
	at[i] = x
	gx := w.tagOf(i, at)
	at[i] = y
	gy := w.tagOf(i, at)
	at[i] = save
	if gx != gy {
		return gx - gy
	}
	byRowid := cmp.Compare(t.rowids[x], t.rowids[y])
	if gx >= len(tg.order.keys) || tg.order.keys[gx] == nil {
		return byRowid
	}
	key, kv := tg.order.keys[gx], w.tagKeys[i][gx]
	for k := range key.cols {
		c := compareValuesCollatedEnc(kv(int(x), k), kv(int(y), k), key.colls[k], w.m.encoding())
		if c == 0 {
			continue
		}
		if k < len(key.desc) && key.desc[k] {
			return -c
		}
		return c
	}
	return byRowid
}

// segJoinOrderEncoding is the encoding OpAutoIndexOrder compares cur's text
// under: a write cursor's own (applyWriteOrder), else the statement's.
func segJoinOrderEncoding(cur *vdbeCursor, m *vdbe) TextEncoding {
	if cur.rowStore == nil {
		return m.encoding()
	}
	if cur.pager != nil {
		return cur.pager.encoding()
	}
	return UTF8
}

// segJoinCursorOrder is the order autoIndexOrderCursor puts t's rows in,
// under key, as positions in t: it runs on a cursor like cur holding t's rows
// in rowid order, each as the cursor reads it (t.value), as the cursor holds
// them when it is ordered.
func segJoinCursorOrder(t *segJoinTable, cur *vdbeCursor, key *autoIndexKey, m *vdbe) ([]int, bool) {
	if t.tbl == nil || key.multiOr != nil && key.multiOr.passes != nil {
		return nil, false
	}
	sc := segJoinScratch(t, cur, m)
	autoIndexOrderCursor(sc, key, segJoinOrderEncoding(cur, m))
	t.index()
	perm := make([]int, t.n)
	for i, id := range sc.rowids {
		r := t.row(int64(id))
		if r < 0 {
			return nil, false
		}
		perm[i] = int(r)
	}
	return perm, true
}

// opt reports whether level i is optional.
func (w *segJoinWalk) opt(i int) bool { return w.optional>>i&1 != 0 }

// before reports whether joined row a comes before b in the loop's order:
// level by level, each row by its place in the order the loop visits that
// level's rows.
func (w *segJoinWalk) before(a, b []int32) bool {
	for i := range a {
		if a[i] != b[i] {
			if w.tags != nil && w.tags[i] != nil && a[i] >= 0 && b[i] >= 0 {
				return w.tagCmp(i, slices.Clone(a), a[i], b[i]) < 0
			}
			if w.rank != nil && w.rank[i] != nil && a[i] >= 0 && b[i] >= 0 {
				return w.rank[i][a[i]] < w.rank[i][b[i]]
			}
			return a[i] < b[i]
		}
	}
	return false
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
	if a.level == segJoinMulti {
		return nil, false // computed on a joined row, by value
	}
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
		v, ok := segJoinEvalRow(&vm, a.expr, column, func(*Instruction) Value { return t.value(row, -1) }, nil)
		if !ok {
			return nil, false
		}
		out[r] = v
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

// segJoinEvalRow runs e once on vm's registers, its columns read by column
// and its rowids by rowid, and returns its value.
//
// volatile, when not nil, makes the expression's k'th instruction, a call of
// a volatile function, and reports whether it did; nil makes every call.
func segJoinEvalRow(vm *vdbe, e *segJoinExpr, column, rowid func(*Instruction) Value, volatile func(k int, op *Instruction) bool) (Value, bool) {
	for k := 0; k < len(e.insns); {
		op := &e.insns[k]
		switch {
		case volatile != nil && segJoinVolatileInsn(*op):
			if !volatile(k, op) {
				return Value{}, false
			}
		case op.Op == OpRowid:
			vm.regs[op.P2] = rowid(op)
		case op.Op == OpGoto:
			k = e.at(op.P2)
			continue
		default:
			taken, ok := vm.segEvalInsn(op, column)
			if !ok {
				return Value{}, false
			}
			if taken {
				k = e.at(op.P2)
				continue
			}
		}
		k++
	}
	// The value may point into the private registers' scratch: kept, it
	// must be its own.
	return copyValue(vm.regs[e.out]), true
}

// nullRowValue is a's value on the optional level's NULL row, where every
// column and the rowid read NULL -- not NULL itself for every expression
// (coalesce).
func (w *segJoinWalk) nullRowValue(a segJoinArg) (Value, bool) {
	if v, ok := w.nullExprs[a.expr]; ok {
		return v, true
	}
	vm := *w.m
	vm.regs = make([]Value, len(w.m.regs))
	vm.fnArgBuf, vm.likePlan = nil, nil
	none := func(*Instruction) Value { return Value{} }
	v, ok := segJoinEvalRow(&vm, a.expr, none, none, nil)
	if !ok {
		return Value{}, false
	}
	if w.nullExprs == nil {
		w.nullExprs = map[*segJoinExpr]Value{}
	}
	w.nullExprs[a.expr] = v
	return v, true
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
	if a.level == segJoinMulti {
		v, _ := w.eval(a.expr, at) // a failure marks the walk failed
		return v
	}
	lv := w.lv(a)
	if a.expr == nil {
		return w.tabs[lv].value(at[lv], a.col)
	}
	if at[lv] < 0 {
		v, _ := w.nullRowValue(a)
		return v
	}
	vals, ok := w.exprValues(a)
	if !ok {
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
	w.crosses, w.tags, w.levelOf = plan.crosses, plan.tags, map[int]int{}
	for i, c := range plan.cursors {
		w.levelOf[c] = i
	}
	vm := *m
	vm.regs = make([]Value, len(m.regs))
	vm.fnArgBuf, vm.likePlan = nil, nil
	w.vm = &vm
	for i, tg := range plan.tags {
		if tg == nil {
			continue
		}
		// The first level has no outer row to tag it by.
		if i == 0 || tg.cursor != plan.cursors[i] {
			return nil, false
		}
		sc := segJoinScratch(w.tabs[i], m.cursors[tg.cursor], m)
		if w.tagKeys == nil {
			w.tagKeys = make([][]func(i, k int) Value, k)
		}
		w.tagKeys[i] = make([]func(i, k int) Value, len(tg.order.keys))
		for g, key := range tg.order.keys {
			if key == nil {
				continue
			}
			// OpMultiOrSort fails on a key it cannot compute for some row.
			at, ok := autoIndexKeyValues(sc, key)
			if !ok || len(key.colls) < len(key.cols) {
				return nil, false
			}
			w.tagKeys[i][g] = at
		}
	}
	// The index orders the loop applies -- as OpAutoIndexOrder does: not to a
	// cursor it streams or re-seeks -- the first level's by the same sort,
	// over the same values.
	for _, o := range plan.orders {
		lv := slices.Index(plan.cursors, o.cursor)
		cur := m.cursors[o.cursor]
		if lv < 0 || cur == nil || cur.rowStore == nil && (cursorHasNoBtree(cur) || cur.streamable || cur.seekReseek) {
			continue
		}
		key := o.key
		t := w.tabs[lv]
		var perm []int
		if key.multiOr != nil || key.exprs != nil {
			// An order by an expression or by a MULTI_OR's sub-scans: the
			// cursor's own code orders a cursor holding the table's rows.
			var ok bool
			if perm, ok = segJoinCursorOrder(t, cur, key, m); !ok {
				return nil, false
			}
		} else {
			if len(key.cols) == 0 || len(key.colls) < len(key.cols) {
				return nil, false
			}
			perm = make([]int, t.n)
			for i := range perm {
				perm[i] = i
			}
			autoIndexSortValues(perm, key, func(i, k int) Value { return t.value(int32(i), key.cols[k]) }, segJoinOrderEncoding(cur, m))
		}
		rank := make([]int32, t.n)
		for i, r := range perm {
			rank[r] = int32(i)
		}
		if w.rank == nil {
			w.rank = make([][]int32, k)
		}
		w.rank[lv] = rank
		if lv != 0 {
			continue
		}
		w.order0 = make([]int32, t.n)
		for i, r := range perm {
			w.order0[i] = int32(r)
		}
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
				e, n = segJoinEdge{a: x.b, aArg: x.bArg, b: x.a, bArg: x.aArg}, n+1
			}
		}
		if n != 1 {
			return nil, false
		}
		// The earlier level's side: integers, NULLs marked.
		keys, _, nulls, ok := w.nullable(e.aArg, false)
		if !ok {
			return nil, false
		}
		l := segJoinLink{from: e.a}
		t := w.tabs[i]
		if b := e.bArg; b.expr == nil && (b.col < 0 || b.col == t.ipk) {
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
			var sk []int64
			var rows []int32
			if b.expr == nil {
				if sk, rows, ok = t.sortedBy(b.col); !ok {
					return nil, false
				}
			} else if sk, rows, ok = w.sortedByValues(b); !ok {
				return nil, false
			}
			if w.rank != nil && w.rank[i] != nil {
				// The rows sharing a key, in the order the loop visits them.
				rank := w.rank[i]
				rows = slices.Clone(rows)
				for lo := 0; lo < len(sk); {
					hi := lo + 1
					for hi < len(sk) && sk[hi] == sk[lo] {
						hi++
					}
					slices.SortFunc(rows[lo:hi], func(x, y int32) int { return cmp.Compare(rank[x], rank[y]) })
					lo = hi
				}
			}
			l.keys, l.nulls, l.sorted, l.rows = keys, nulls, sk, rows
		}
		w.link[i] = l
	}
	return w, true
}

// flat reports whether every level after the first is found by a rowid.
func (w *segJoinWalk) flat() bool {
	for i := 1; i < len(w.link); i++ {
		if w.link[i].one == nil {
			return false
		}
	}
	return true
}

// each calls fn once per joined row, in the loop's order, with at holding
// each level's row (-1 for the optional level's NULL row); fn returning false
// stops the walk, and each reports whether it ran to the end.
func (w *segJoinWalk) each(at []int32, fn func() bool) bool {
	k := len(w.tabs)
	if w.flat() {
		// Every later level found by a rowid: at most one row each, so the
		// nest is a pass over the first level's rows.
	rows:
		for j := range w.tabs[0].n {
			r := int32(j)
			if w.order0 != nil {
				r = w.order0[j]
			}
			if p := w.pass[0]; p != nil && !p[r] {
				continue
			}
			at[0] = r
			if !w.keep(0, at) {
				continue
			}
			for i := 1; i < k; i++ {
				l := &w.link[i]
				found := int32(-1)
				if from := at[l.from]; from >= 0 {
					found = l.one[from]
				}
				if found >= 0 && w.crosses != nil && len(w.crosses[i]) != 0 {
					if at[i] = found; !w.keep(i, at) {
						found = -1
					}
				}
				if found < 0 || w.pass[i] != nil && !w.pass[i][found] {
					if !w.opt(i) {
						continue rows
					}
					found = -1 // nothing matched: the row is all NULL
				}
				at[i] = found
			}
			if !fn() {
				return false
			}
		}
		return true
	}
	var visit func(i int) bool
	visit = func(i int) bool {
		if i == k {
			return fn()
		}
		pass := w.pass[i]
		if i == 0 {
			for j := range w.tabs[0].n {
				r := int32(j)
				if w.order0 != nil {
					r = w.order0[j]
				}
				if pass == nil || pass[r] {
					at[0] = r
					if w.keep(0, at) && !visit(1) {
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
					if at[i] = r; w.keep(i, at) {
						matched = true
						if !visit(i + 1) {
							return false
						}
					}
				}
			} else if l.nulls == nil || !l.nulls[from] {
				// A NULL key equals nothing; any other, the rows holding it.
				key := l.keys[from]
				lo, _ := slices.BinarySearch(l.sorted, key)
				hi := lo
				for hi < len(l.sorted) && l.sorted[hi] == key {
					hi++
				}
				rows := l.rows[lo:hi]
				if w.tags != nil && w.tags[i] != nil {
					// A MULTI_OR level: its rows in the order the loop puts
					// them in for this outer row.
					rows = slices.Clone(rows)
					slices.SortStableFunc(rows, func(x, y int32) int { return w.tagCmp(i, at, x, y) })
				}
				for _, r := range rows {
					if pass == nil || pass[r] {
						if at[i] = r; !w.keep(i, at) {
							continue
						}
						matched = true
						if !visit(i + 1) {
							return false
						}
					}
				}
			}
		}
		if !matched && w.opt(i) {
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
	// distinct is the arguments a DISTINCT count or sum of integers reads.
	distinct []int
	plan     *segJoinPlan
	w        *segJoinWalk
	floats   bool
	// keep: a group keeps its joined rows, which a REAL argument replays in
	// order and a DISTINCT one dedupes.
	keep bool
}

// segJoinArgsOf reads plan's arguments, or reports false when an aggregate is
// one the kernel does not model.
//
// replayInts reads every INTEGER argument one row at a time too, its
// aggregate replaying its rows in the loop's order: what a sum that may
// overflow takes, to raise the loop's error or turn REAL where it does.
func segJoinArgsOf(plan *segJoinPlan, w *segJoinWalk, replayInts bool) (*segJoinArgs, bool) {
	n := len(plan.args)
	args := &segJoinArgs{ints: make([][]int64, n), flts: make([][]float64, n), nulls: make([][]bool, n),
		levels: make([]int, n), argOf: map[int]int{}, plan: plan, w: w}
	for a, arg := range plan.args {
		args.levels[a] = w.lv(arg)
		args.argOf[arg.reg] = a
		// An expression over an optional level's NULL row need not be NULL
		// (coalesce). The vectors below take that row's value to be NULL;
		// when it is not, the argument is read one row at a time instead.
		if arg.expr != nil && w.opt(w.lv(arg)) {
			v, ok := w.nullRowValue(arg)
			if !ok {
				return nil, false
			}
			if v.Typ != Null {
				continue
			}
		}
		if vals, _, nulls, ok := w.nullable(arg, false); ok {
			if !replayInts {
				args.ints[a], args.nulls[a] = vals, nulls
			}
		} else if _, vals, nulls, ok := w.nullable(arg, true); ok {
			args.flts[a], args.nulls[a], args.floats = vals, nulls, true
		}
		// Otherwise its values are read one at a time, by an aggregate that
		// replays its rows.
	}
	for _, it := range aggPlanTemplates(plan.agg) {
		if !plan.steps(it.stepSeg) {
			return nil, false
		}
		for _, r := range segJoinStepRegs(it) {
			if _, ok := args.argOf[r]; !ok {
				return nil, false
			}
		}
		if it.kind == aggCountStar && it.filter == nil {
			continue
		}
		if args.replays(it) {
			args.keep = true
		} else if it.distinct {
			args.distinct = append(args.distinct, args.argOf[it.rowRegs[aggExprArg]-1])
		}
	}
	args.keep = args.keep || args.floats || plan.fullStep || plan.drainCalls
	// A DISTINCT over integers needs only which of its level's rows took
	// part, not how often or in what order -- when there is one group. Many
	// keep their rows.
	if len(args.distinct) > 0 && plan.grouped {
		args.keep = true
	}
	return args, true
}

// replays reports whether aggregate it takes its group's rows after the
// first through its own step, one at a time, rather than the sums: every
// kind but count, sum, total, avg, min and max, and those over arguments the
// sums do not model -- not INTEGER or REAL throughout, a DISTINCT over REALs,
// a min or max over REALs or one whose ties go to the last -- or order-strict
// beyond what a sum's window shows; and any with a FILTER or an ORDER BY of
// its own.
func (args *segJoinArgs) replays(it *aggItem) bool {
	if it.filter != nil || it.orderBy != nil {
		return true // the step decides per row, or buffers the rows
	}
	switch it.kind {
	case aggCountStar:
		return false
	case aggCount, aggSum, aggTotal, aggAvg, aggMin, aggMax:
	default:
		return true
	}
	a := args.argOf[it.rowRegs[aggExprArg]-1]
	sumKind := it.kind == aggSum || it.kind == aggTotal || it.kind == aggAvg
	minMax := it.kind == aggMin || it.kind == aggMax
	switch {
	case it.minMaxLastWins, it.orderStrict && !sumKind:
		return true
	case args.ints[a] == nil && args.flts[a] == nil:
		return true
	case args.flts[a] != nil && (minMax || it.distinct):
		return true
	case it.distinct && !sumKind && it.kind != aggCount:
		return true
	}
	return false
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
	// gcols is each of the aggregate's gathers' level and columns, read
	// once for gatherAt.
	gcols []segJoinGatherCols
}

type segJoinGatherCols struct {
	level int
	cols  [][]Value
}

type segJoinGroup struct {
	sums  *segJoinSums
	first []int32 // each level's row in the group's first joined row
	// rows is every joined row of the group, k levels each, in the loop's
	// order: kept when a REAL argument is replayed.
	rows []int32
	// took, per level a DISTINCT argument is read from, marks the level's
	// rows some joined row of the group held: kept instead of rows by a
	// whole-join aggregate.
	took [][]bool
}

// segJoinGroupsOf builds plan's groups, or reports false. A plan with no
// GROUP BY has one group, or none when nothing joins.
func (m *vdbe) segJoinGroupsOf(plan *segJoinPlan) (gs *segJoinGroups, ok bool) {
	gs, ok, overflow := m.segJoinGroupsWith(plan, false)
	if overflow && !plan.volatile() {
		// A second walk would call a volatile expression again.
		// A sum that could overflow: summed again by its own step.
		gs, ok, _ = m.segJoinGroupsWith(plan, true)
	}
	return gs, ok
}

// segJoinGroupsWith is segJoinGroupsOf with every INTEGER argument summed
// as the rows go by, or with replayInts, replayed (segJoinArgsOf). overflow
// reports that a sum could overflow, the groups unbuilt.
func (m *vdbe) segJoinGroupsWith(plan *segJoinPlan, replayInts bool) (gs *segJoinGroups, ok, overflow bool) {
	defer func() {
		// A run the walk could not evaluate: what it produced is not the
		// loop's.
		if ok && gs != nil && gs.w != nil && gs.w.failed {
			gs, ok = nil, false
		}
	}()
	w, ok := m.openSegJoin(plan)
	if !ok {
		return nil, false, false
	}
	args, ok := segJoinArgsOf(plan, w, replayInts)
	if !ok {
		return nil, false, false
	}
	// The groups, by an INTEGER key's value, or by any other key's canonical
	// bytes: the encoding hashed GROUP BY keys by, equal exactly when the
	// keys compare equal under BINARY (the recognisers admit no other
	// collation).
	var keys []int64
	byValue := false
	keyLv := w.lv(plan.key)
	if plan.grouped {
		if plan.key.expr != nil && w.opt(keyLv) {
			// Read by value, which evaluates the NULL row.
			if _, ok := w.nullRowValue(plan.key); !ok {
				return nil, false, false
			}
		}
		// A key on the optional level is NULL in its NULL rows, which only the
		// canonical bytes can say.
		if keys, ok = w.ints(plan.key); !ok || w.opt(keyLv) || plan.keyColl != "" {
			keys, byValue = nil, true
		}
		// NOCASE folds bytes, which for UTF-16 text is not its letters.
		if plan.keyColl != "" && m.encoding() != UTF8 {
			return nil, false, false
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
			buf = hashAggKeyBytes(buf[:0], segJoinCollKey(one, plan.keyColl))
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
			if !w.before(g.rows[len(g.rows)-k:], at) {
				return false
			}
		default:
			if g := &groups[gi]; w.before(at, g.first) {
				copy(g.first, at)
			}
		}
		g := &groups[gi]
		if args.keep {
			g.rows = append(g.rows, at...)
		} else if len(args.distinct) > 0 {
			if g.took == nil {
				g.took = make([][]bool, k)
				for _, a := range args.distinct {
					if lv := args.levels[a]; g.took[lv] == nil {
						g.took[lv] = make([]bool, w.tabs[lv].n)
					}
				}
			}
			for _, a := range args.distinct {
				if lv := args.levels[a]; at[lv] >= 0 {
					g.took[lv][at[lv]] = true
				}
			}
		}
		// A volatile argument's value, computed now, in the loop's order --
		// unless the loop computes it in a GROUP BY's drain, in the drain's
		// order: the group's first row there, its others in rest.
		if !plan.drainCalls {
			for _, a := range plan.argsInCallOrder() {
				w.value(a, at)
			}
		}
		if !g.sums.add(args, at) {
			overflow = true
			return false
		}
		return true
	}) {
		return nil, false, overflow
	}
	sort.Slice(groups, func(i, j int) bool { return w.before(groups[i].first, groups[j].first) })
	return &segJoinGroups{w: w, plan: plan, groups: groups, args: args}, true, false
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
func (gs *segJoinGroups) rest(m *vdbe, g *segJoinGroup) error {
	if gs.plan.fullStep || gs.plan.drainCalls {
		return gs.restFull(m, g)
	}
	args, s := gs.args, g.sums
	k := len(gs.plan.cursors)
	// An aggregate that replays: the group's rows after the first, in the
	// loop's order, each with the registers its step reads set to that row's
	// values, through the step itself.
	var replayErr error
	var window []Value
	replay := func(it *aggItem) {
		if window == nil {
			window = make([]Value, len(m.regs))
		}
		nr := gs.plan.agg.nCols + len(gs.plan.agg.scopes)
		if len(m.aggNullRow) < nr {
			m.aggNullRow = make([]Value, nr)
		}
		ctx := m.aggRowCtx(gs.plan.agg, m.aggNullRow[:gs.plan.agg.nCols:gs.plan.agg.nCols], m.aggNullRow[gs.plan.agg.nCols:nr])
		skipped := false
		for r := 0; r < len(g.rows); r += k {
			at := g.rows[r : r+k]
			if !skipped && slices.Equal(at, g.first) {
				skipped = true // the row already stepped
				continue
			}
			for _, reg := range segJoinStepRegs(it) {
				window[reg] = gs.w.value(gs.plan.args[args.argOf[reg]], at)
			}
			if err := it.step(ctx, window); err != nil && replayErr == nil {
				replayErr = err
				return
			}
		}
	}
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
		if args.replays(it) {
			replay(it)
			return
		}
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
			if g.took != nil && g.took[lv] != nil {
				took := g.took[lv]
				// Each value of a row that took part: the first row's is
				// already seen.
				for r, t := range took {
					if !t || null(a, int32(r)) {
						continue
					}
					if v := ints[r]; !has(v) {
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
				}
				return
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
	return replayErr
}

// restFull steps group g's rows after the first, in the loop's order,
// through the aggregate's own step, each with its arguments in their
// registers and its whole row: as the loop steps them.
func (gs *segJoinGroups) restFull(m *vdbe, g *segJoinGroup) error {
	k := len(gs.plan.cursors)
	nr := gs.plan.agg.nCols + len(gs.plan.agg.scopes)
	buf := make([]Value, nr)
	row, rowids := buf[:gs.plan.agg.nCols:gs.plan.agg.nCols], buf[gs.plan.agg.nCols:nr]
	skipped := false
	for r := 0; r < len(g.rows); r += k {
		at := g.rows[r : r+k]
		if !skipped && slices.Equal(at, g.first) {
			skipped = true // the row already stepped
			continue
		}
		if err := gs.stepRow(m, at, row, rowids); err != nil {
			return err
		}
	}
	return nil
}

// stepRow steps joined row at through every segment the body steps, with
// its arguments in their registers and its whole row gathered into row and
// rowids.
func (gs *segJoinGroups) stepRow(m *vdbe, at []int32, row, rowids []Value) error {
	for _, a := range gs.plan.argsInCallOrder() {
		m.regs[a.reg] = gs.w.value(a, at)
	}
	if !gs.gatherAt(at, row, rowids) {
		return fmt.Errorf("vdbe: the join kernel over a cursor outside its join")
	}
	for _, seg := range gs.plan.allSegs() {
		if err := m.aggStep(m.aggRowCtx(gs.plan.agg, row, rowids), seg); err != nil {
			return err
		}
	}
	return nil
}

// segJoinAggregate answers the plan's whole-join aggregate, or reports false
// having changed nothing. An error is the step's own, which the loop would
// have met.
func (m *vdbe) segJoinAggregate(plan *segJoinPlan) (bool, error) {
	// Nothing reads the row the step is handed (the recogniser checked:
	// aggPlanReadsOnlyLoweredColsExceptOrder), so it is a NULL one.
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
	if plan.fullStep || plan.firstRow {
		nr := plan.agg.nCols + len(plan.agg.scopes)
		buf := make([]Value, nr)
		if err := gs.stepRow(m, g.first, buf[:plan.agg.nCols:plan.agg.nCols], buf[plan.agg.nCols:nr]); err != nil {
			return false, err
		}
		if err := gs.rest(m, g); err != nil {
			return false, err
		}
		return true, nil
	}
	gs.loadArgs(m, g)
	nr := plan.agg.nCols + len(plan.agg.scopes)
	if len(m.aggNullRow) < nr {
		m.aggNullRow = make([]Value, nr)
	}
	row, rowids := m.aggNullRow[:plan.agg.nCols:plan.agg.nCols], m.aggNullRow[plan.agg.nCols:nr]
	for _, seg := range plan.allSegs() {
		if err := m.aggStep(m.aggRowCtx(plan.agg, row, rowids), seg); err != nil {
			return false, err
		}
	}
	if err := gs.rest(m, g); err != nil {
		return false, err
	}
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
		for _, seg := range plan.allSegs() {
			if err := m.hashAggStepRow(plan.agg, keyVals, seg, row, rowids); err != nil {
				return false, err
			}
		}
		if err := gs.rest(m, g); err != nil {
			return false, err
		}
	}
	return true, nil
}

// gather is gatherCursorRow over group g's first joined row: the row and the
// rowids the loop's step would have read with its cursors on it.
func (gs *segJoinGroups) gather(g *segJoinGroup, row, rowids []Value) bool {
	return gs.gatherAt(g.first, row, rowids)
}

// gatherAt is gatherCursorRow over joined row at.
func (gs *segJoinGroups) gatherAt(at []int32, row, rowids []Value) bool {
	level := func(cursor int) int { return slices.Index(gs.plan.cursors, cursor) }
	if gs.gcols == nil {
		gs.gcols = make([]segJoinGatherCols, len(gs.plan.agg.gathers))
		for i, ga := range gs.plan.agg.gathers {
			lv := level(ga.cursor)
			if lv < 0 || ga.offset+ga.n > len(row) {
				gs.gcols = nil
				return false
			}
			cols := make([][]Value, min(ga.n, len(gs.w.tabs[lv].tbl.cols)))
			for c := range cols {
				cols[c] = gs.w.tabs[lv].values(c)
			}
			gs.gcols[i] = segJoinGatherCols{level: lv, cols: cols}
		}
	}
	for i, ga := range gs.plan.agg.gathers {
		gc := gs.gcols[i]
		r := at[gc.level]
		for c, col := range gc.cols {
			if r < 0 {
				row[ga.offset+c] = Value{} // an optional level's NULL row
			} else {
				row[ga.offset+c] = col[r]
			}
		}
	}
	for i, cur := range gs.plan.agg.cursors {
		lv := level(cur)
		switch {
		case gs.plan.agg.scopes[i].noRowid:
			rowids[i] = Value{Typ: Null}
		case lv < 0:
			return false
		case at[lv] < 0:
			rowids[i] = Value{} // the optional level's NULL row
		default:
			rowids[i] = Value{Typ: Int, I: gs.w.tabs[lv].rowids[at[lv]]}
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
func (m *vdbe) segJoinTopOf(plan *segJoinPlan) (gs *segJoinGroups, ok bool) {
	defer func() {
		// A run the walk could not evaluate: what it produced is not the
		// loop's.
		if ok && gs != nil && gs.w != nil && gs.w.failed {
			gs, ok = nil, false
		}
	}()
	ki := plan.keys
	if ki == nil || ki.bound <= 0 || ki.nKey != len(plan.order) {
		return nil, false
	}
	w, ok := m.openSegJoin(plan)
	if !ok {
		return nil, false
	}
	// Every key computable on every row, the NULL row included: value
	// reads NULL for one that is not.
	for _, o := range plan.order {
		if o.expr == nil || o.level == segJoinMulti {
			continue // a joined row's value fails the walk if it fails
		}
		if _, ok := w.exprValues(o); !ok {
			return nil, false
		}
		if _, ok := w.nullRowValue(o); !ok && w.opt(w.lv(o)) {
			return nil, false
		}
	}
	if len(plan.volatiles) > 0 {
		// A walk over every joined row first, making each volatile call in
		// the loop's order; the walks after read what they got.
		vol := (&segJoinPlan{args: plan.volatiles}).argsInCallOrder()
		at := make([]int32, len(plan.cursors))
		w.each(at, func() bool {
			for _, a := range vol {
				w.value(a, at)
			}
			return true
		})
	} else if gs, ok := segJoinTopNumeric(w, plan); ok {
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
	gs = &segJoinGroups{w: w, plan: plan}
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
	sort.Slice(gs.groups, func(i, j int) bool { return w.before(gs.groups[i].first, gs.groups[j].first) })
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
	if !ok || w.opt(w.lv(o)) {
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
	sort.Slice(gs.groups, func(i, j int) bool { return w.before(gs.groups[i].first, gs.groups[j].first) })
	return gs, true
}
