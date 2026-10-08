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
	vals, found, served := b.rp.SeekRowidSegments(b.root, int64(rid))
	if !served || !found {
		return nil, false
	}
	row, err := fullStoredRow(b.tbl, rid, vals)
	if err != nil {
		return nil, false
	}
	return row, true
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

// segEachSortedOver is segEachSorted over the base rows walk hands it, which
// may be a subset (segRows.walkFiltered): every overlay row still goes out.
func (s *rowStore) segEachSortedOver(walk func(func(uint64, []Value) bool), yield func(uint64, []Value) bool) {
	over := s.overlaySorted()
	oi := 0
	stop := false
	if !s.baseGone {
		walk(func(rid uint64, vals []Value) bool {
			for oi < len(over) && rowidLess(over[oi], rid) {
				if v, ok := s.m[over[oi]]; ok && !yield(over[oi], s.resolve(v)) {
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
		if v, ok := s.m[over[oi]]; ok && !yield(over[oi], s.resolve(v)) {
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
		s.seg.walkRowids(func(rid uint64) bool {
			if !s.gone[rid] && (!found || rowidLess(max, rid)) {
				max, found = rid, true
			}
			return true
		})
	}
	return max, found
}

// segSelPool holds walkFiltered's selection buffers.
var segSelPool = sync.Pool{New: func() any { return new([]int64) }}
