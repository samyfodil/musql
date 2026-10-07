package engine

import (
	"fmt"
	"iter"
	"os"
)

// Serving table scans from columnar segments. Every read -- VDBE cursors,
// schema loader, integrity_check -- reaches table rows through ScanTable,
// making every read path columnar with no second executor.

// ScanTable yields the rows of the table rooted at rootPage in ascending rowid
// order, with each row's rowid. A table this database holds no rows for -- an
// empty database, or a root nothing is stored under -- yields none. Because
// iter.Seq2 has no room for an error, the scan's error is only known once
// iteration has stopped: call errFn afterward.
func (p *ReadOnlyPager) ScanTable(rootPage uint32) (seq iter.Seq2[uint64, []Value], errFn func() error) {
	if p != nil && p.segs != nil {
		if seq, ok, err := p.segs.scanSegments(rootPage); ok {
			return seq, func() error { return err }
		}
	}
	return func(func(uint64, []Value) bool) {}, func() error { return nil }
}

// segSource is the columnar view of a database, attached to a read pager. Nil
// on a pager over an empty database.
type segSource struct {
	file *SegmentFile
	// byRoot maps a table's b-tree root page to its segments, because that is
	// the handle ScanTable is called with. Built once at attach.
	byRoot map[uint32][]*segment

	// delta is the replayed row-major log beside the segment file, and tableOf
	// maps a table's root to that log's table index. See segment_delta.go.
	delta   *segDeltaState
	tableOf map[uint32]int

	// live is a write session's own row stores by root page, including uncommitted
	// changes. Set only by Session.ReadPager. Preferred over segments and delta
	// because it is the already-merged state the session has been mutating.
	live map[uint32]*rowStore

	// unloaded is the tables session had not loaded when this source was built,
	// by root page. liveRows loads them on first touch, so statements pay for
	// only the tables they read. An unloaded table has not been written by this
	// session, so its rows remain in session.segments.
	unloaded map[uint32]bool
	session  *DB
	// frozen marks a source whose rows stay as they were when built.
	// frozenSegs is the session's segment source at that moment.
	frozen     bool
	frozenSegs *ReadOnlyPager
}

// isLive reports whether rootPage's rows are a write session's row store, loaded
// or still to be loaded on first touch (liveRows). The checks that only ask
// WHERE a table's rows come from use it, so asking never loads anything.
func (src *segSource) isLive(rootPage uint32) bool {
	_, live := src.live[rootPage]
	return live || src.unloaded[rootPage]
}

// liveRows is the write session's row store for rootPage, loading the table if
// this source was built before it was (see unloaded). A load error comes back
// with have true: the rows ARE the session's, they just could not be read.
func (src *segSource) liveRows(rootPage uint32) (rows *rowStore, have bool, err error) {
	if rows, have := src.live[rootPage]; have {
		return rows, true, nil
	}
	if !src.unloaded[rootPage] {
		return nil, false, nil
	}
	db := src.session
	t := db.pageTableByRoot(rootPage)
	if t == nil {
		return nil, false, nil
	}
	rows, err = src.loadRows(db, t)
	if err != nil {
		return nil, true, err
	}
	src.live[rootPage] = rows
	delete(src.unloaded, rootPage)
	// Invalidate the cache: a source that loaded a table no longer matches
	// its original cache key.
	if db.segSrcCache == src {
		db.segSrcCache = nil
	}
	return rows, true, nil
}

// loadRows is the row store liveRows serves for a table this source was built
// before the session loaded.
//
// A live source serves the session's own store, loading it. A frozen one serves
// a copy: of the session's store when the table is STILL unloaded, since nothing
// can have written a table nobody has loaded; otherwise of the rows frozenSegs
// holds, because the statement has since loaded the table and may have written
// it.
func (src *segSource) loadRows(db *DB, t *tableMeta) (*rowStore, error) {
	if src.frozen && t.loaded {
		if t.isTemp || db.segments != src.frozenSegs {
			// A TEMP table's rows are not in frozenSegs, and a rewrite that
			// replaced it loaded every table first -- so neither can be an
			// unloaded table at freeze time. Refused rather than guessed.
			return nil, fmt.Errorf("engine: segment read: %s was loaded after its snapshot was taken", t.name)
		}
		return newSegRowStore(src.frozenSegs, t), nil
	}
	if err := db.ensureTableLoaded(t); err != nil {
		return nil, err
	}
	if src.frozen {
		return t.rows.clone(t), nil
	}
	return t.rows, nil
}

// overlayFor is the delta rows and tombstones for the table at rootPage.
func (src *segSource) overlayFor(rootPage uint32) (rows []segOverlayRow, dead map[int64]bool, clean bool) {
	if src.delta == nil {
		return nil, nil, true
	}
	ti, ok := src.tableOf[rootPage]
	if !ok {
		return nil, nil, true
	}
	if src.delta.emptyFor(ti) {
		return nil, nil, true
	}
	return src.delta.rowsFor(ti), src.delta.dead[ti], false
}

// ScannedFromSegments reports whether the table at rootPage would have its ROWS
// scanned columnar. Exported because a benchmark or a gate that means to
// measure the columnar path has to be able to assert it got one -- silently
// falling back to the b-tree would make a "columnar" measurement a b-tree
// measurement.
//
// It asks the SAME question ScanTable does, and that is load-bearing rather
// than tidy: vdbeCursor.rewind declines the streaming b-tree cursor for any
// table this reports true for, so the two cannot SPLIT -- a streamable scan
// reading a b-tree while every other read of the same table read segments.
func (p *ReadOnlyPager) ScannedFromSegments(rootPage uint32) bool {
	if p == nil || p.segs == nil {
		return false
	}
	// A WRITE SESSION's live row store counts: its rows are served through
	// scanSegments like any other segment source, and the streaming b-tree cursor
	// must decline for it just the same. Missing this was silent and total -- the
	// cursor read a b-tree at a SYNTHETIC root page, found nothing there, and
	// every query against a held segment session returned zero rows while
	// ScanTable on the same root returned four.
	if p.segs.isLive(rootPage) {
		return true
	}
	_, ok := p.segs.byRoot[rootPage]
	return ok
}

// ServesLiveRows reports whether rootPage's rows come from a write session's own
// row store (segSource.live), whose slices scanSegments and seekRowid hand out
// as they are: shared with the store, its BEGIN/SAVEPOINT snapshots and other
// cursors, so a caller must copy before fixing one up (normalizeRow, not
// vdbeCursor.normalize).
func (p *ReadOnlyPager) ServesLiveRows(rootPage uint32) bool {
	if p == nil || p.segs == nil {
		return false
	}
	return p.segs.isLive(rootPage)
}

// scanSegments yields a table's rows from its segments, in the same order and
// with the same values a b-tree walk produces.
//
// Values come back through segment.Value, not through a zero-copy slice: this
// is the ROW interface, and a row needs every column. The zero-copy path is for
// a caller that wants one column of many (segFilterCount), which is a different
// question asked through a different door.
func (src *segSource) scanSegments(rootPage uint32) (iter.Seq2[uint64, []Value], bool, error) {
	// A write session's own rows come first: they are the merge already done,
	// plus whatever this session has changed since. See segSource.live.
	if rows, have, err := src.liveRows(rootPage); err != nil {
		return func(func(uint64, []Value) bool) {}, true, err
	} else if have {
		return func(yield func(uint64, []Value) bool) {
			// One ordered walk rather than a rowid list plus a lookup per row:
			// a table's committed rows stream from the mapped file.
			rows.eachSortedUntil(yield)
		}, true, nil
	}
	segs, ok := src.byRoot[rootPage]
	if !ok {
		return nil, false, nil
	}
	// Merge with delta if present: segment rows with log removals skipped and
	// log rows spliced in at their rowid (preserving rowid order).
	dRows, dead, clean := src.overlayFor(rootPage)
	if !clean {
		return func(yield func(uint64, []Value) bool) {
			di := 0
			for _, sg := range segs {
				nCols := len(sg.cols)
				for i := 0; i < sg.nRows; i++ {
					rowid := int64(sg.Rowid(i))
					// Every overlay row strictly before this segment row comes
					// first, which is what keeps the output in rowid order.
					for di < len(dRows) && dRows[di].rowid < rowid {
						if !yield(uint64(dRows[di].rowid), dRows[di].vals) {
							return
						}
						di++
					}
					// Equal rowid: the log's value supersedes the segment's,
					// and matching POSITIONALLY here is what makes the skip O(1)
					// -- both sides are rowid-ordered, so a scan of the overlay
					// per segment row would be the same answer for O(rows x log).
					if di < len(dRows) && dRows[di].rowid == rowid {
						if !yield(uint64(rowid), dRows[di].vals) {
							return
						}
						di++
						continue
					}
					if dead[rowid] {
						continue // the log removed this row
					}
					w := sg.Width(i)
					if w > nCols {
						w = nCols
					}
					row := make([]Value, w)
					for c := 0; c < w; c++ {
						row[c] = sg.Value(c, i)
					}
					if !yield(uint64(rowid), row) {
						return
					}
				}
			}
			// Rows the log added past the end of every segment.
			for ; di < len(dRows); di++ {
				if !yield(uint64(dRows[di].rowid), dRows[di].vals) {
					return
				}
			}
		}, true, nil
	}
	return func(yield func(uint64, []Value) bool) {
		// Rows are carved from chunks. Each row is its own disjoint slice
		// so callers can retain them. Capacity is capped to prevent aliasing.
		const chunkRows = 512
		var arena []Value
		for _, s := range segs {
			nCols := len(s.cols)
			for i := 0; i < s.nRows; i++ {
				w := s.Width(i)
				if w > nCols {
					w = nCols
				}
				if len(arena) < w {
					n := chunkRows * nCols
					if n < w {
						n = w
					}
					arena = make([]Value, n)
				}
				row := arena[:w:w]
				arena = arena[w:]
				for c := 0; c < w; c++ {
					row[c] = s.Value(c, i)
				}
				if !yield(s.Rowid(i), row) {
					return
				}
			}
		}
	}, true, nil
}

// segFilterCountTable answers a filter over every segment of one table.
// served is false when the table is not columnar here, or when any segment
// declines the predicate shape -- in which case the caller must fall back, and
// a PARTIAL count is never returned.
func (p *ReadOnlyPager) segFilterCountTable(rootPage uint32, preds []segPred) (int, bool) {
	if p == nil || p.segs == nil {
		return 0, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok {
		return 0, false
	}
	// TEXT bounds require UTF-8 encoding, as byte comparison is collation-aware.
	for i := range preds {
		if preds[i].Val.Typ == Text && p.meta.encoding != UTF8 {
			return 0, false
		}
	}
	// A row-major delta in front of the segments makes a raw block read wrong, and
	// this used to decline for it outright -- which meant declining after every
	// commit, because a delta is what a commit appends. The log is MERGED now
	// instead: the correction is O(delta), not O(rows). See
	// segment_delta_merge.go.
	sub, add, mergeable := p.segs.segDeltaCorrection(rootPage, segs, preds)
	if !mergeable {
		return 0, false
	}
	// No predicates: `SELECT count(*) FROM t`. The row count is recorded per
	// segment, so this is a sum over a handful of integers rather than a scan of
	// a hundred thousand rows -- which is what C SQLite's OP_Count achieves by
	// counting b-tree entries without decoding them.
	if len(preds) == 0 {
		total := 0
		for _, s := range segs {
			total += s.nRows
		}
		return total - sub + add, true
	}
	// A single EQUALITY answers from the column's equality index -- O(1) per
	// segment against O(rows) for the kernel. It is the one predicate shape
	// where scanning loses to a lookup, and it is the shape an indexed column
	// is usually queried with (segment_eqindex.go has why a hash and not a
	// b-tree, and why a zone map would not have worked here).
	if len(preds) == 1 && preds[0].Op == segEQ && preds[0].Val.Typ == Int {
		total, served := 0, true
		for _, s := range segs {
			n, ok := s.segEqualityCount(preds[0].Col, preds[0].Val.I)
			if !ok {
				served = false
				break
			}
			total += n
		}
		if served {
			return total - sub + add, true
		}
		// A column with no index falls through to the scan below rather than
		// declining: the kernel answers equality perfectly well, just slower.
	}
	counts := make([]int, len(segs))
	scanned := segEach(len(segs), func(i int) bool {
		s := segs[i]
		// Refuse columns whose NULLs might not be NULLs (ALTER TABLE ADD COLUMN
		// leaves older records short and synthesizes DEFAULT at read time).
		if !segPredColsFilterable(s, preds) {
			return false
		}
		counts[i] = segFilterCount(s, preds)
		return counts[i] >= 0
	})
	if !scanned {
		return 0, false
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	return total - sub + add, true
}

// segFilterSumTable answers `sum(col) WHERE <preds>` from the segments.
// It declines on integer overflow rather than approximating.
func (p *ReadOnlyPager) segFilterSumTable(rootPage uint32, preds []segPred, col int) (Value, bool) {
	// A row-major delta beside the segments makes a raw block read wrong; see
	// segCleanFor.
	if !p.segCleanFor(rootPage) {
		return Value{}, false
	}
	if p == nil || p.segs == nil {
		return Value{}, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok {
		return Value{}, false
	}
	// Each segment sums on its own; the parts are added afterwards. That is
	// only the same answer as one pass in row order when no partial sum can
	// overflow in ANY order, so the sum of |v| must fit -- otherwise this
	// declines, and the row loop decides between a value and C SQLite's
	// "integer overflow" error.
	type part struct {
		sum     int64
		abs     uint64
		matched int
	}
	parts := make([]part, len(segs))
	ok = segEach(len(segs), func(si int) bool {
		s := segs[si]
		pt := &parts[si]
		// The column's range proves the sum cannot overflow: no per-row check,
		// and runs the zone maps rule out are not read at all.
		if sum, n, ok := segFilterSumZoned(s, preds, col); ok {
			vz, _ := s.intZones(col)
			pt.sum, pt.matched = sum, n
			pt.abs = uint64(s.nRows) * uint64(max(-(vz.min+1), vz.max)+1)
			return true
		}
		vals, okCol := segCleanInt64Column(s, col)
		if !okCol {
			return false
		}
		cols, okPreds := segPredColumns(s, preds)
		if !okPreds {
			return false
		}
		for i := 0; i < s.nRows; i++ {
			keep := true
			for j, pr := range preds {
				if i >= len(cols[j]) || !pr.Op.satisfied(cmpInt64(cols[j][i], pr.Val.I)) {
					keep = false
					break
				}
			}
			if !keep {
				continue
			}
			if i >= len(vals) {
				return false
			}
			v := vals[i]
			u := uint64(v)
			if v < 0 {
				u = -u
			}
			if pt.abs+u < pt.abs || pt.abs+u > 1<<63-1 {
				return false
			}
			pt.abs += u
			pt.sum += v
			pt.matched++
		}
		return true
	})
	if !ok {
		return Value{}, false
	}
	var total int64
	var abs uint64
	matched := 0
	for _, pt := range parts {
		if abs+pt.abs < abs || abs+pt.abs > 1<<63-1 {
			return Value{}, false
		}
		abs += pt.abs
		total += pt.sum
		matched += pt.matched
	}
	if matched == 0 {
		return Value{Typ: Null}, true
	}
	return Value{Typ: Int, I: total}, true
}

// segPredColumns resolves each predicate's column to a clean int64 block, or
// reports false when one is not.
func segPredColumns(s *segment, preds []segPred) ([][]int64, bool) {
	cols := make([][]int64, len(preds))
	for i, pr := range preds {
		if pr.Val.Typ != Int {
			return nil, false
		}
		c, ok := segCleanInt64Column(s, pr.Col)
		if !ok {
			return nil, false
		}
		cols[i] = c
	}
	return cols, true
}

// cmpInt64 is the three-way compare segPredOp.satisfied expects.
func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// segFilePages is this segment database's size in PAGES of its own recorded page
// size -- the segment file plus the delta beside it, which together are the
// database.
//
// "PRAGMA page_count" answers it. See that pragma's own note for why a number is
// better than a decline here, and why it is not C's number for the same data.
func (p *ReadOnlyPager) segFilePages() uint32 {
	if p == nil || p.segs == nil {
		return 0
	}
	ps := int64(p.meta.pageSize)
	if ps <= 0 {
		ps = 4096
	}
	var total int64
	for _, path := range []string{p.mainPath, segDeltaPath(p.mainPath)} {
		if path == "" {
			continue
		}
		if st, err := os.Stat(path); err == nil {
			total += st.Size()
		}
	}
	return uint32((total + ps - 1) / ps)
}

// pageTableByRoot is the table whose b-tree is rooted at root.
func (db *DB) pageTableByRoot(root uint32) *tableMeta {
	for _, t := range db.tables {
		if t.rootPage == root {
			return t
		}
	}
	return nil
}
