package engine

import (
	"slices"
	"sort"
	"sync/atomic"
)

// A point lookup on our format: seekRowid finds a row by rowid through segments,
// delta, and live row store. Rows are checked in precedence order: live rows
// first, then delta live/dead, then segments. Segments store rowids in a sorted
// array and the delta's maps answer in O(1).
func (src *segSource) seekRowid(rootPage uint32, rid int64) (vals []Value, found, served bool) {
	// A write session's own rows: O(1), and already authoritative. A table that
	// could not be loaded declines, and the scan the caller falls back to reports
	// the load error (ScanTable's errFn).
	if rows, have, err := src.liveRows(rootPage); err != nil {
		return nil, false, false
	} else if have {
		v, ok := rows.get(uint64(rid))
		return v, ok, true
	}
	segs, ok := src.byRoot[rootPage]
	if !ok {
		return nil, false, false
	}
	if src.delta != nil {
		if ti, known := src.tableOf[rootPage]; known && !src.delta.emptyFor(ti) {
			// PUT beats the segment's copy; a KILL removes it. Checked in that
			// order because the replay already resolved a rowid that was both
			// (segment_delta.go): it lands in exactly one of the two maps.
			if v, live := src.delta.live[ti][rid]; live {
				return v, true, true
			}
			if src.delta.dead[ti][rid] {
				return nil, false, true
			}
		}
	}
	for _, sg := range segs {
		i, hit := sg.findRowid(rid)
		if !hit {
			continue
		}
		return sg.rowAt(i), true, true
	}
	return nil, false, true
}

// findRowid is the binary search: the index of rid in this segment's ascending
// rowid array, or false.
func (s *segment) findRowid(rid int64) (int, bool) {
	// Bound first to skip segments outside this one's rowid range.
	if s.nRows == 0 || rid < int64(s.Rowid(0)) || rid > int64(s.Rowid(s.nRows-1)) {
		return 0, false
	}
	i := sort.Search(s.nRows, func(i int) bool { return int64(s.Rowid(i)) >= rid })
	if i < s.nRows && int64(s.Rowid(i)) == rid {
		return i, true
	}
	return 0, false
}

// rowAt materializes one row with Width columns (short rows from ALTER ADD COLUMN).
func (s *segment) rowAt(i int) []Value {
	w := s.Width(i)
	if w > len(s.cols) {
		w = len(s.cols)
	}
	row := make([]Value, w)
	for c := 0; c < w; c++ {
		row[c] = s.Value(c, i)
	}
	return row
}

// SeekRowidSegments answers a rowid point lookup from this pager's segments.
// served is false when no segment source covers rootPage, which is the caller's
// signal to fall back to whatever it would have done.
func (p *ReadOnlyPager) SeekRowidSegments(rootPage uint32, rid int64) (vals []Value, found, served bool) {
	if p == nil || p.segs == nil {
		return nil, false, false
	}
	return p.segs.seekRowid(rootPage, rid)
}

// A secondary-index equality seek: finds rowids where a column matches a probe.
// An index seek only prunes: the WHERE conjunct is re-evaluated on every row,
// so overestimating is slow but safe. Results are ascending rowid.
func (src *segSource) seekIndexRowids(rootPage uint32, col int, probe Value) (rowids []int64, served bool) {
	// A table the session holds live is answered from ITS rows, never from the
	// committed segments in byRoot: the live store is this session's view --
	// a transaction's snapshot, its own writes -- and byRoot may be a newer
	// file. After another connection committed, the committed index lost a
	// transaction's own rows, and a replication op log reused a seq it had
	// just written (max(seq) WHERE site=? could not see it).
	rows, live, err := src.liveRows(rootPage)
	if err != nil {
		return nil, false // the caller scans, and the scan reports the error
	}
	if !live {
		segs, ok := src.byRoot[rootPage]
		if !ok || (!src.deltaIsSmallFor(rootPage, segs) && probe.Typ != Int) {
			return nil, false
		}
		out, ok := segIndexRowids(segs, col, probe)
		if !ok {
			return nil, false
		}
		return src.withDeltaRowids(rootPage, out), true
	}
	// A live store over segments answers from THOSE segments' equality index
	// -- the snapshot it was loaded from -- with every rowid it has written
	// over them added. A seek only prunes (every row it yields is re-checked,
	// and one the store no longer holds is dropped by the fetch), so the extra
	// candidates are safe. Answering such a table by a scan made every TEXT,
	// REAL and BLOB equality on a table merely loaded a full scan: 33 ms a
	// lookup at 100k rows, against 2.8 us.
	if out, ok := rows.snapshotIndexRowids(col, probe); ok {
		return out, true
	}
	// Row store index is int64-only; TEXT/BLOB probes against dirty tables scan.
	if probe.Typ != Int {
		return nil, false
	}
	hits, ok := rows.eqRowids(col, probe.I)
	if !ok {
		return nil, false
	}
	out := make([]int64, len(hits))
	for i, r := range hits {
		out[i] = int64(r)
	}
	return out, true
}

// snapshotIndexRowids is a segment-backed row store's equality candidates for
// col = probe: its base segments' index hits, the base's delta rows, and every
// rowid written over them. false when the store is not over segments, has
// spilled, or a segment cannot index col.
func (s *rowStore) snapshotIndexRowids(col int, probe Value) ([]int64, bool) {
	if s == nil || s.seg == nil || s.spill != nil || s.seg.rp == nil || s.seg.rp.segs == nil {
		return nil, false
	}
	base := s.seg.rp.segs
	segs, ok := base.byRoot[s.seg.root]
	if !ok {
		return nil, false
	}
	out, ok := segIndexRowids(segs, col, probe)
	if !ok {
		return nil, false
	}
	for rid := range s.m {
		out = append(out, int64(rid))
	}
	return base.withDeltaRowids(s.seg.root, out), true
}

// segIndexSeeksServed counts index seeks the segments answered, for tests.
// Atomic: concurrent connections seek at once (replication runs several).
var segIndexSeeksServed atomic.Int64

// segIndexRowids finds rowids holding probe across a table's segments from each
// segment's equality index. Returns false when any segment cannot answer.
func segIndexRowids(segs []*segment, col int, probe Value) ([]int64, bool) {
	var out []int64
	for _, sg := range segs {
		if col < 0 || col >= len(sg.cols) {
			return nil, false
		}
		var pos []int32
		var have bool
		switch probe.Typ {
		case Int:
			pos, have = sg.segEqualityRows(col, probe.I)
		case Text, Blob:
			pos, have = sg.segEqualityRowsBytes(col, probe.S)
		case Float:
			pos, have = sg.segEqualityRowsFloat(col, probe.F)
		default:
			return nil, false
		}
		if !have {
			return nil, false // this column is not indexable; scan instead
		}
		for _, i := range pos {
			out = append(out, int64(sg.Rowid(int(i))))
		}
	}
	return out, true
}

// deltaIsSmallFor reports whether this table's log is small enough that the
// block index plus a whole-log union beats the row store. The union is
// unconditional, so a log the size of the table becomes a full scan.
func (src *segSource) deltaIsSmallFor(rootPage uint32, segs []*segment) bool {
	if src.delta == nil {
		return true
	}
	ti, known := src.tableOf[rootPage]
	if !known {
		return true
	}
	n := len(src.delta.live[ti])
	if n == 0 {
		return true
	}
	rows := 0
	for _, sg := range segs {
		rows += sg.nRows
	}
	return n*8 <= rows
}

// withDeltaRowids unions in every rowid the log holds live for this table, then
// sorts and dedupes.
//
// Unioned WHOLE rather than value-tested: those rows postdate the column index,
// and a test here would only duplicate the WHERE re-evaluation that runs on every
// row this returns anyway. A rowid the log KILLED is left in, because the row
// fetch that follows reports it absent and drops it.
func (src *segSource) withDeltaRowids(rootPage uint32, rowids []int64) []int64 {
	if src.delta != nil {
		if ti, known := src.tableOf[rootPage]; known {
			for rid := range src.delta.live[ti] {
				rowids = append(rowids, rid)
			}
		}
	}
	slices.Sort(rowids)
	return slices.Compact(rowids)
}

// SeekIndexRowidsSegments answers a leading-column equality seek from this
// pager's segments. served is false when the seek cannot be served -- no segment
// source, no index on that column, a non-integer probe -- and the caller scans.
//
// coll is the seek's collating sequence. The column index matches a value's
// exact bytes, which is equality only under BINARY -- C compares an index key's
// text under the column's own sequence (sqlite3VdbeRecordCompareWithSkip,
// vdbeaux.c:4855-4861, pKeyInfo->aColl[i]) -- so a TEXT probe under NOCASE or
// RTRIM is refused, and the scan's own comparison decides. Measured over
// "s TEXT COLLATE NOCASE" holding 'b','B','c','b': "WHERE s='B'" answered one
// row here, the exact 'B', where C answers three.
func (p *ReadOnlyPager) SeekIndexRowidsSegments(rootPage uint32, col int, probe Value, coll string) ([]int64, bool) {
	if p == nil || p.segs == nil {
		return nil, false
	}
	if probe.Typ == Text && !equalFoldName(effectiveCollation(coll), "BINARY") {
		return nil, false
	}
	return p.segs.seekIndexRowids(rootPage, col, probe)
}

// RowidBound is min(rowid) (max false) or max(rowid) of the table rooted at
// rootPage, as this pager reads it: from the session's live rows when it holds
// the table, else from the committed segments and delta. served is false when
// this pager has no segment source for it, and the caller scans.
func (p *ReadOnlyPager) RowidBound(rootPage uint32, max bool) (rid uint64, found, served bool) {
	if p == nil || p.segs == nil {
		return 0, false, false
	}
	rows, live, err := p.segs.liveRows(rootPage)
	if err != nil {
		return 0, false, false
	}
	if !live {
		if _, ok := p.segs.byRoot[rootPage]; !ok {
			return 0, false, false
		}
		rows = &rowStore{seg: &segRows{rp: p, root: rootPage}}
	}
	rid, found = rows.rowidBound(max)
	return rid, found, true
}
