package engine

import (
	"maps"
	"slices"
	"sync"
)

// A table's rows, never loaded whole. A session reads rows on demand
// from the mapped segment file.
// come and go through the page cache, and only what a transaction writes is
// pinned (the journal and the dirty pages).
//
// So a table's rowStore keeps the same split. The COMMITTED rows are the
// segment file plus its delta, read on demand through the session's committed
// pager (segRows): a point lookup is a binary search over a segment's rowid
// block, a scan is the rowid-ordered merge scanSegments already does. Only what
// this session WROTE is in memory -- the overlay: rowStore.m holds every row put
// since the base was taken, and rowStore.gone every base rowid dropped since.
// A row read is the overlay's when it has one, nothing when it is gone, the
// base's otherwise; every mutator writes the overlay alone, so the undo log, the
// equality and unique indexes and the max-rowid caches work unchanged.
//
// The base is replaced -- and the overlay emptied -- only where the committed
// state is re-read: a rewrite (rewriteFile unloads every file table, and the next
// use takes a fresh view) and a reopen. Between them the overlay keeps winning
// over a base that lags the delta this session appended, which is what makes it
// correct across commits without re-reading the file per commit.

// segRows is one table's committed rows: the segments and delta behind rp.
type segRows struct {
	rp   *ReadOnlyPager
	root uint32
	tbl  *tableMeta

	// memo keeps rows get decoded, by rowid, for the next get of the same one:
	// an UPDATE reads each row it writes twice -- its cursor, then the store
	// again just before writing (opUpdateRow, for what a BEFORE trigger may
	// have changed) -- and decoding a row from its segment was most of the
	// cost of the second read. Committed rows change only with the source or
	// its delta (a commit of this session replays its batch into the delta in
	// place), so the memo is emptied whenever either has moved since it was
	// filled. Made only once a store has done memoAfter gets, so a table read
	// a few rows at a time costs nothing.
	memoMu      sync.Mutex
	memo        []segRowMemo
	memoGets    int
	memoSrc     *segSource
	memoDelta   *segDeltaState
	memoBatches int
}

// segRowMemo is one memo slot.
type segRowMemo struct {
	rid uint64
	row []Value
}

const (
	memoSlots = 1 << 14
	memoAfter = 256
)

// memoKey is what the memo's rows were decoded from.
func (b *segRows) memoKey() (*segSource, *segDeltaState, int) {
	src := b.rp.segs
	if src == nil || src.delta == nil {
		return src, nil, 0
	}
	return src, src.delta, src.delta.batches
}

// newSegRowStore is the row store over a table's committed rows.
func newSegRowStore(rp *ReadOnlyPager, tbl *tableMeta) *rowStore {
	return newSegRowStoreAt(rp, tbl.rootPage, tbl)
}

// newSegRowStoreAt is newSegRowStore for a table stored under root in rp, which
// for a TEMP table is its place in the temp file, not the session's number.
func newSegRowStoreAt(rp *ReadOnlyPager, root uint32, tbl *tableMeta) *rowStore {
	return &rowStore{seg: &segRows{rp: rp, root: root, tbl: tbl}, m: map[uint64][]Value{}}
}

// get is rid's committed row, normalized exactly as a table load normalizes it
// (fullStoredRow: full width, generated columns computed).
func (b *segRows) get(rid uint64) ([]Value, bool) {
	b.memoMu.Lock()
	src, delta, batches := b.memoKey()
	if b.memo != nil && (src != b.memoSrc || delta != b.memoDelta || batches != b.memoBatches) {
		clear(b.memo)
	}
	b.memoSrc, b.memoDelta, b.memoBatches = src, delta, batches
	if b.memo != nil {
		if e := &b.memo[rid%memoSlots]; e.row != nil && e.rid == rid {
			row := e.row
			b.memoMu.Unlock()
			return row, true
		}
	}
	b.memoMu.Unlock()
	vals, found, served := b.rp.SeekRowidSegments(b.root, int64(rid))
	if !served || !found {
		return nil, false
	}
	row, err := fullStoredRow(b.tbl, rid, vals)
	if err != nil {
		return nil, false
	}
	b.memoMu.Lock()
	if b.memo == nil {
		if b.memoGets++; b.memoGets > memoAfter {
			b.memo = make([]segRowMemo, memoSlots)
		}
	}
	if b.memo != nil && src == b.memoSrc && delta == b.memoDelta && batches == b.memoBatches {
		b.memo[rid%memoSlots] = segRowMemo{rid, row}
	}
	b.memoMu.Unlock()
	return row, true
}

// segRun reads the committed rows of a run of rowids, in the order a seek's
// candidates come: what get answers for each, with the segment found by moving
// forward rather than searched per row and the rows carved from shared chunks
// rather than allocated one at a time. Rowids out of order restart the walk.
// Anything get would answer from elsewhere -- a session's live rows, a table
// not loaded yet, segments not in ascending disjoint order -- goes to get.
type segRun struct {
	b     *segRows
	src   *segSource
	segs  []*segment
	ti    int
	delta bool
	ok    bool
	si    int
	prev  int64
	arena []Value
	// at is the position in segs[si] the last rowid was found at or would
	// be: the rowids come ascending, so the next one is searched from there.
	at int
}

func (b *segRows) run() segRun {
	r := segRun{b: b}
	src := b.rp.segs
	if src == nil || src.unloaded[b.root] {
		return r
	}
	if _, live := src.live[b.root]; live {
		return r
	}
	segs, ok := src.byRoot[b.root]
	if !ok {
		return r
	}
	for i, sg := range segs {
		if sg.nRows == 0 || i > 0 && int64(segs[i-1].Rowid(segs[i-1].nRows-1)) >= int64(sg.Rowid(0)) {
			return r
		}
	}
	r.src, r.segs, r.ok = src, segs, true
	if src.delta != nil {
		if ti, known := src.tableOf[b.root]; known && !src.delta.emptyFor(ti) {
			r.ti, r.delta = ti, true
		}
	}
	return r
}

func (r *segRun) get(rid uint64) ([]Value, bool) {
	if !r.ok {
		return r.b.get(rid)
	}
	at := int64(rid)
	if r.delta {
		// As seekRowid: the log's PUT beats the segment's copy, a KILL removes it.
		if v, live := r.src.delta.live[r.ti][at]; live {
			row, err := fullStoredRow(r.b.tbl, rid, v)
			return row, err == nil
		}
		if r.src.delta.dead[r.ti][at] {
			return nil, false
		}
	}
	if at < r.prev {
		r.si, r.at = 0, 0
	}
	r.prev = at
	for r.si < len(r.segs) && at > int64(r.segs[r.si].Rowid(r.segs[r.si].nRows-1)) {
		r.si, r.at = r.si+1, 0
	}
	if r.si == len(r.segs) {
		return nil, false
	}
	sg := r.segs[r.si]
	i, hit := sg.findRowidFrom(at, r.at)
	r.at = i
	if !hit {
		return nil, false
	}
	nCols := len(sg.cols)
	w := sg.Width(i)
	if w > nCols {
		w = nCols
	}
	if len(r.arena) < w {
		r.arena = make([]Value, max(512*nCols, w))
	}
	row := r.arena[:w:w]
	r.arena = r.arena[w:]
	for c := range w {
		row[c] = sg.Value(c, i)
	}
	full, err := fullStoredRow(r.b.tbl, rid, row)
	return full, err == nil
}

// has reports whether rid has a committed row, without decoding it.
func (b *segRows) has(rid uint64) bool {
	src := b.rp.segs
	if src == nil {
		return false
	}
	if src.delta != nil {
		if ti, known := src.tableOf[b.root]; known && !src.delta.emptyFor(ti) {
			if _, live := src.delta.live[ti][int64(rid)]; live {
				return true
			}
			if src.delta.dead[ti][int64(rid)] {
				return false
			}
		}
	}
	for _, sg := range src.byRoot[b.root] {
		if _, hit := sg.findRowid(int64(rid)); hit {
			return true
		}
	}
	return false
}

// walk yields every committed row in ascending signed-rowid order.
func (b *segRows) walk(yield func(uint64, []Value) bool) {
	src := b.rp.segs
	if src == nil {
		return
	}
	seq, handled, err := src.scanSegments(b.root)
	if err != nil || !handled {
		return
	}
	for rid, vals := range seq {
		row, rerr := fullStoredRow(b.tbl, rid, vals)
		if rerr != nil {
			return
		}
		if !yield(rid, row) {
			return
		}
	}
}

// walkFiltered is walk restricted to the segment rows f selects (segSelectRows)
// -- a SUPERSET of the rows the statement's WHERE keeps, which still runs on
// every row handed back -- plus every row the delta holds, since the filter
// reads only the segments' column blocks. A segment the filter cannot judge is
// walked whole. Only the rows handed back are decoded.
func (b *segRows) walkFiltered(f *segRowFilter, params []Value, yield func(uint64, []Value) bool) {
	src := b.rp.segs
	if src == nil {
		return
	}
	segs, ok := src.byRoot[b.root]
	if !ok {
		return
	}
	dRows, dead, _ := src.overlayFor(b.root)
	di := 0
	emit := func(rid uint64, vals []Value) bool {
		row, err := fullStoredRow(b.tbl, rid, vals)
		return err == nil && yield(rid, row)
	}
	// The selection buffer is a row per segment row: pooled, since a write
	// statement walks here once and a one-row UPDATE allocated 800 KB for it.
	bp := segSelPool.Get().(*[]int64)
	buf := (*bp)[:0]
	defer func() { *bp = buf[:0]; segSelPool.Put(bp) }()
	for _, sg := range segs {
		sel, judged := segSelectRows(sg, f, b.tbl.ipkIndex, params, buf)
		n := sg.nRows
		if judged {
			buf, n = sel, len(sel)
			segRowsSelected += int64(n)
		}
		nCols := len(sg.cols)
		for k := 0; k < n; k++ {
			i := k
			if judged {
				i = int(sel[k])
			}
			rowid := int64(sg.Rowid(i))
			for di < len(dRows) && dRows[di].rowid < rowid {
				if !emit(uint64(dRows[di].rowid), dRows[di].vals) {
					return
				}
				di++
			}
			if di < len(dRows) && dRows[di].rowid == rowid {
				if !emit(uint64(rowid), dRows[di].vals) {
					return
				}
				di++
				continue
			}
			if dead[rowid] {
				continue
			}
			w := min(sg.Width(i), nCols)
			row := make([]Value, w)
			for c := 0; c < w; c++ {
				row[c] = sg.Value(c, i)
			}
			if !emit(uint64(rowid), row) {
				return
			}
		}
	}
	for ; di < len(dRows); di++ {
		if !emit(uint64(dRows[di].rowid), dRows[di].vals) {
			return
		}
	}
}

// walkRowids yields every committed rowid in ascending signed order, decoding no
// row: the segments' rowid blocks merged with the delta's puts, its kills
// skipped -- scanSegments' merge, keys only.
func (b *segRows) walkRowids(yield func(uint64) bool) {
	src := b.rp.segs
	if src == nil {
		return
	}
	segs := src.byRoot[b.root]
	dRows, dead, _ := src.overlayFor(b.root)
	di := 0
	for _, sg := range segs {
		for i := 0; i < sg.nRows; i++ {
			rid := int64(sg.Rowid(i))
			for di < len(dRows) && dRows[di].rowid < rid {
				if !yield(uint64(dRows[di].rowid)) {
					return
				}
				di++
			}
			if di < len(dRows) && dRows[di].rowid == rid {
				if !yield(uint64(rid)) {
					return
				}
				di++
				continue
			}
			if dead[rid] {
				continue
			}
			if !yield(uint64(rid)) {
				return
			}
		}
	}
	for ; di < len(dRows); di++ {
		if !yield(uint64(dRows[di].rowid)) {
			return
		}
	}
}

// segGet is get over the overlay and the base.
// segHas is whether segGet would find rid, without reading the row.
func (s *rowStore) segHas(rid uint64) bool {
	if _, ok := s.m[rid]; ok {
		return true
	}
	if s.baseGone || s.gone[rid] {
		return false
	}
	return s.seg.has(rid)
}

func (s *rowStore) segGet(rid uint64) ([]Value, bool) {
	if vals, ok := s.m[rid]; ok {
		return s.resolve(vals), true
	}
	if s.baseGone || s.gone[rid] {
		return nil, false
	}
	return s.seg.get(rid)
}

// segLen counts the visible rows: one walk of the base's keys the first time,
// then kept by put and drop.
func (s *rowStore) segLen() int {
	if !s.nKnown {
		n := 0
		if !s.baseGone {
			s.seg.walkRowids(func(rid uint64) bool {
				if _, over := s.m[rid]; !over && !s.gone[rid] {
					n++
				}
				return true
			})
		}
		s.n, s.nKnown = n+len(s.m), true
	}
	return s.n
}

// segAll yields every visible row, base first, then the overlay. A row the body
// puts is not yielded a second time: the overlay's keys are taken before the
// walk, and the base walk skips any rowid the overlay holds by then.
func (s *rowStore) segAll(yield func(uint64, []Value) bool) {
	over := slices.Collect(maps.Keys(s.m))
	if !s.baseGone {
		stop := false
		s.seg.walk(func(rid uint64, vals []Value) bool {
			if _, o := s.m[rid]; o || s.gone[rid] || s.baseGone {
				return true
			}
			if !yield(rid, vals) {
				stop = true
				return false
			}
			return true
		})
		if stop {
			return
		}
	}
	for _, rid := range over {
		if vals, ok := s.m[rid]; ok {
			if !yield(rid, s.resolve(vals)) {
				return
			}
		}
	}
}

// segEachSorted yields every visible row in ascending signed-rowid order: the
// base walk and the overlay's sorted keys, merged, the overlay winning a tie.
func (s *rowStore) segEachSorted(yield func(uint64, []Value) bool) {
	s.segEachSortedOver(s.seg.walk, yield)
}

// segEachSortedRaw is segEachSorted handing out the overlay's rows as m holds
// them, a spilled row as its sentinel.
func (s *rowStore) segEachSortedRaw(yield func(uint64, []Value) bool) {
	s.segEachSortedOverWith(s.seg.walk, func(v []Value) []Value { return v }, yield)
}

// segEachSortedOver is segEachSorted over the base rows walk hands it, which
// may be a subset (segRows.walkFiltered): every overlay row still goes out.
func (s *rowStore) segEachSortedOver(walk func(func(uint64, []Value) bool), yield func(uint64, []Value) bool) {
	s.segEachSortedOverWith(walk, s.resolve, yield)
}

// segEachSortedOverWith is segEachSortedOver reading each overlay row through
// resolve.
func (s *rowStore) segEachSortedOverWith(walk func(func(uint64, []Value) bool), resolve func([]Value) []Value, yield func(uint64, []Value) bool) {
	over := s.overlaySorted()
	oi := 0
	stop := false
	if !s.baseGone {
		walk(func(rid uint64, vals []Value) bool {
			for oi < len(over) && rowidLess(over[oi], rid) {
				if v, ok := s.m[over[oi]]; ok && !yield(over[oi], resolve(v)) {
					stop = true
					return false
				}
				oi++
			}
			if oi < len(over) && over[oi] == rid {
				return true // the overlay's copy goes out in its turn
			}
			if s.gone[rid] {
				return true
			}
			if !yield(rid, vals) {
				stop = true
				return false
			}
			return true
		})
	}
	if stop {
		return
	}
	for ; oi < len(over); oi++ {
		if v, ok := s.m[over[oi]]; ok && !yield(over[oi], resolve(v)) {
			return
		}
	}
}

// overlaySorted is the overlay's keys in ascending signed order, memoized
// (rowStore.over). The caller must not modify the slice.
func (s *rowStore) overlaySorted() []uint64 {
	if !s.overValid || len(s.over) != len(s.m) {
		over := slices.Collect(maps.Keys(s.m))
		radixSortRowids(over)
		s.over, s.overValid = over, true
	}
	return s.over
}

// segSortedRowids is every visible rowid in ascending signed order.
func (s *rowStore) segSortedRowids() []uint64 {
	out := make([]uint64, 0, s.segLen())
	over := s.overlaySorted()
	oi := 0
	if !s.baseGone {
		s.seg.walkRowids(func(rid uint64) bool {
			for oi < len(over) && rowidLess(over[oi], rid) {
				out = append(out, over[oi])
				oi++
			}
			if oi < len(over) && over[oi] == rid {
				return true
			}
			if !s.gone[rid] {
				out = append(out, rid)
			}
			return true
		})
	}
	return append(out, over[oi:]...)
}

// segMaxRowid is the largest visible rowid.
func (s *rowStore) segMaxRowid() (uint64, bool) {
	var max uint64
	found := false
	for rid := range s.m {
		if !found || rowidLess(max, rid) {
			max, found = rid, true
		}
	}
	if !s.baseGone {
		// From the top down: the first base rowid not deleted is the base's
		// largest, so this stops there instead of walking every row.
		s.seg.walkRowidsDesc(func(rid uint64) bool {
			if s.gone[rid] {
				return true
			}
			if !found || rowidLess(max, rid) {
				max, found = rid, true
			}
			return false
		})
	}
	return max, found
}

// segSelPool holds walkFiltered's selection buffers.
var segSelPool = sync.Pool{New: func() any { return new([]int64) }}

// walkRowidsDesc is walkRowids from the other end: every live rowid of the
// base, in descending order, stopping when yield returns false.
func (b *segRows) walkRowidsDesc(yield func(uint64) bool) {
	src := b.rp.segs
	if src == nil {
		return
	}
	segs := src.byRoot[b.root]
	dRows, dead, _ := src.overlayFor(b.root)
	di := len(dRows) - 1
	for si := len(segs) - 1; si >= 0; si-- {
		sg := segs[si]
		for i := sg.nRows - 1; i >= 0; i-- {
			rid := int64(sg.Rowid(i))
			for di >= 0 && dRows[di].rowid > rid {
				if !yield(uint64(dRows[di].rowid)) {
					return
				}
				di--
			}
			if di >= 0 && dRows[di].rowid == rid {
				if !yield(uint64(rid)) {
					return
				}
				di--
				continue
			}
			if dead[rid] {
				continue
			}
			if !yield(uint64(rid)) {
				return
			}
		}
	}
	for ; di >= 0; di-- {
		if !yield(uint64(dRows[di].rowid)) {
			return
		}
	}
}

// rowidBound is the store's smallest (max false) or largest rowid, by signed
// order, and false for an empty store. Over segments it walks in from the
// requested end and stops at the first rowid not dropped, instead of visiting
// every row.
func (s *rowStore) rowidBound(max bool) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	if max && s.maxKnown {
		return s.max, true
	}
	var best uint64
	found := false
	better := func(rid uint64) bool {
		if !found {
			return true
		}
		if max {
			return rowidLess(best, rid)
		}
		return rowidLess(rid, best)
	}
	for rid := range s.m {
		if better(rid) {
			best, found = rid, true
		}
	}
	if s.seg != nil && !s.baseGone {
		pick := func(rid uint64) bool {
			if s.gone[rid] {
				return true
			}
			if better(rid) {
				best, found = rid, true
			}
			return false // the first live rowid from this end is the bound
		}
		if max {
			s.seg.walkRowidsDesc(pick)
		} else {
			s.seg.walkRowids(pick)
		}
	}
	return best, found
}
