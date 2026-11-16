package engine

import (
	"encoding/binary"
	"math"
	"slices"
	"sort"
	"sync/atomic"
)

// segRangeSeeksServed counts the range seeks the segments answered, for tests.
var segRangeSeeksServed atomic.Int64

// A range on an indexed column ("x > 5", "x BETWEEN 2 AND 9") is answered
// from each segment's column index, whose values are sorted: the rows between
// the bounds' two places in it. Only an index over INTEGERs or REALs, with
// numeric bounds, is asked; anything else leaves the scan to decide. Like an
// equality seek, a range only prunes: every row it yields is tested again.

// idxSeekRange is a range seek key's bounds, each coerced to the comparison's
// affinity: a NULL bound is none, and incl whether a value equal to the bound
// is in the range.
type idxSeekRange struct {
	lo, hi         Value
	loIncl, hiIncl bool
}

// numeric reports whether every bound r has is a number, which the INTEGER
// and REAL indexes order values against.
func (r idxSeekRange) numeric() bool {
	num := func(v Value) bool { return v.Typ == Null || v.Typ == Int || v.Typ == Float && !math.IsNaN(v.F) }
	return num(r.lo) && num(r.hi) && (r.lo.Typ != Null || r.hi.Typ != Null)
}

// below reports whether v is below the range's lower bound, and above whether
// it is past its upper one, for a numeric v.
func (r idxSeekRange) below(v Value) bool {
	if r.lo.Typ == Null {
		return false
	}
	c := compareNumeric(v, r.lo)
	return c < 0 || c == 0 && !r.loIncl
}

func (r idxSeekRange) above(v Value) bool {
	if r.hi.Typ == Null {
		return false
	}
	c := compareNumeric(v, r.hi)
	return c > 0 || c == 0 && !r.hiIncl
}

// rangeSpan is the index's values in r, as the posting positions from..to;
// false for an index over neither INTEGERs nor REALs.
func (x *segDiskIndex) rangeSpan(r idxSeekRange) (from, to uint32, ok bool) {
	var at func(i int) Value
	switch x.codec {
	case segIdxCodecPostings:
		at = func(i int) Value { return Value{Typ: Int, I: x.valueAt(i)} }
	case segIdxCodecFloat:
		at = func(i int) Value { return Value{Typ: Float, F: math.Float64frombits(uint64(x.valueAt(i)))} }
	default:
		return 0, 0, false
	}
	lo := sort.Search(x.nVals, func(i int) bool { return !r.below(at(i)) })
	hi := lo + sort.Search(x.nVals-lo, func(i int) bool { return r.above(at(lo + i)) })
	return x.start(lo), x.start(hi), true
}

// postAt is the row position at posting p, for an index whose postings are
// four bytes (every codec but segIdxCodecBytesPerm).
func (x *segDiskIndex) postAt(p uint32) uint32 { return binary.LittleEndian.Uint32(x.posts[p*4:]) }

// segRangeRows is the row positions of this segment holding a value of col in
// r, in value order; false when col has no index over INTEGERs or REALs.
func (s *segment) segRangeRows(col int, r idxSeekRange) ([]int32, bool) {
	s.eqMu.Lock()
	x, ok := s.diskIndexFor(col)
	s.eqMu.Unlock()
	if !ok {
		return nil, false
	}
	from, to, ok := x.rangeSpan(r)
	if !ok {
		return nil, false
	}
	out := make([]int32, 0, to-from)
	for p := from; p < to; p++ {
		out = append(out, int32(x.postAt(p)))
	}
	return out, true
}

// segRangeCount is how many rows segRangeRows would answer.
func (s *segment) segRangeCount(col int, r idxSeekRange) (int, bool) {
	s.eqMu.Lock()
	x, ok := s.diskIndexFor(col)
	s.eqMu.Unlock()
	if !ok {
		return 0, false
	}
	from, to, ok := x.rangeSpan(r)
	return int(to - from), ok
}

// seekIndexRangeRowids is seekIndexRowids for a range: the rows of every
// segment whose value of col lies in r, and every row of the log, or of the
// session's store over them, whose value might, ascending. false when a
// segment cannot answer or a bound is not a number: the scan decides.
func (src *segSource) seekIndexRangeRowids(rootPage uint32, col int, r idxSeekRange) ([]int64, bool) {
	if !r.numeric() {
		return nil, false
	}
	rows, live, err := src.liveRows(rootPage)
	if err != nil {
		return nil, false
	}
	if live {
		return rows.snapshotRangeRowids(col, r)
	}
	segs, ok := src.byRoot[rootPage]
	if !ok {
		return nil, false
	}
	out, ok := segRangeRowids(segs, col, r)
	if !ok {
		return nil, false
	}
	return src.withDeltaRange(rootPage, out, col, r), true
}

// snapshotRangeRowids is snapshotIndexRowids for a range: the store's base
// segments' rows in r, the base's log rows, and every row the store has
// written whose value might lie in r.
func (s *rowStore) snapshotRangeRowids(col int, r idxSeekRange) ([]int64, bool) {
	if s == nil || s.seg == nil || s.spill != nil || s.seg.rp == nil || s.seg.rp.segs == nil {
		return nil, false
	}
	base := s.seg.rp.segs
	segs, ok := base.byRoot[s.seg.root]
	if !ok {
		return nil, false
	}
	out, ok := segRangeRowids(segs, col, r)
	if !ok {
		return nil, false
	}
	s.snapWork += len(s.m)
	for rid, vals := range s.m {
		if col >= len(vals) || !r.excludes(vals[col]) {
			out = append(out, int64(rid))
		}
	}
	return base.withDeltaRange(s.seg.root, out, col, r), true
}

// segRangeRowids is the rowids of segs' rows whose value of col lies in r;
// false when one of them cannot answer.
func segRangeRowids(segs []*segment, col int, r idxSeekRange) ([]int64, bool) {
	var out []int64
	for _, sg := range segs {
		if col < 0 || col >= len(sg.cols) {
			return nil, false
		}
		pos, ok := sg.segRangeRows(col, r)
		if !ok {
			return nil, false
		}
		out = slices.Grow(out, len(pos))
		for _, i := range pos {
			out = append(out, int64(sg.Rowid(int(i))))
		}
	}
	return out, true
}

// withDeltaRange is withDeltaRowids for a range: rowids with every row of the
// log whose value might lie in r, ascending and distinct.
func (src *segSource) withDeltaRange(rootPage uint32, rowids []int64, col int, r idxSeekRange) []int64 {
	if src.delta != nil {
		if ti, known := src.tableOf[rootPage]; known {
			for rid, vals := range src.delta.live[ti] {
				if col < len(vals) && r.excludes(vals[col]) {
					continue
				}
				rowids = append(rowids, rid)
			}
		}
	}
	slices.Sort(rowids)
	segRangeSeeksServed.Add(1)
	return slices.Compact(rowids)
}

// excludes reports that no comparison r stands for can be true of v: a NULL,
// or a number outside it. A TEXT or BLOB value compares above every number,
// so it is kept for the loop to test.
func (r idxSeekRange) excludes(v Value) bool {
	switch v.Typ {
	case Null:
		return true
	case Int, Float:
		return r.below(v) || r.above(v)
	}
	return false
}

// seekIndexRangeEstimate is seekIndexEstimate for a range: the rows of the
// segments in r, those under the session's store when it holds the table.
func (src *segSource) seekIndexRangeEstimate(rootPage uint32, col int, r idxSeekRange) (n, total int, ok bool) {
	if !r.numeric() {
		return 0, 0, false
	}
	rows, live, err := src.liveRows(rootPage)
	if err != nil {
		return 0, 0, false
	}
	segs, ok := src.byRoot[rootPage]
	if live {
		if rows.seg == nil || rows.seg.rp == nil || rows.seg.rp.segs == nil {
			return 0, 0, false
		}
		segs, ok = rows.seg.rp.segs.byRoot[rows.seg.root]
		total = len(rows.m)
	}
	if !ok {
		return 0, 0, false
	}
	for _, sg := range segs {
		c, ok := sg.segRangeCount(col, r)
		if !ok {
			return 0, 0, false
		}
		n += c
		total += sg.nRows
	}
	return n, total, true
}
