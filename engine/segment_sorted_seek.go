package engine

import (
	"cmp"
	"math"
	"slices"
	"sync/atomic"
)

// segSortedSeeksServed counts the sorted seeks the segments served, so a test
// can tell them from the built rows'.
var segSortedSeeksServed atomic.Int64

// segSortedRows is OpSortedSeek's index when the segments serve it: every row
// of the table's segments, as (segment, row), in the order of the seek's
// INTEGER column, ties in rowid order -- the order sortRowsBy's stable sort of
// the built rows gives. A seek positions the cursor on a run of segment rows,
// so the table is neither built nor normalized. ok is false when the segments
// cannot serve it, remembered so the next seek does not ask again.
type segSortedRows struct {
	hint *sortedSeekHint
	ok   bool
	keys []int64
	segs []*segment
	rows []int
}

// segSortedSeek positions cur on the rows whose column h.col equals probe,
// straight from the segments, as sortedSeek narrows built rows. false, with
// nothing changed, where the segments are not the whole table or the column
// is not a clean INTEGER block in every one of them.
func (cur *vdbeCursor) segSortedSeek(h *sortedSeekHint, probe Value) bool {
	if h.built || cur.materialized || cur.fill != nil || cur.ranged || cur.seekConfigured || cur.idxSeekConfigured || cur.seekSetConfigured ||
		cur.segSorted == nil && (cur.segSrc != nil || cur.keyListReady) {
		return false
	}
	s := cur.segSorted
	if s == nil || s.hint != h {
		s = cur.pager.segSortedRowsFor(cur, h)
		cur.segSorted = s
	}
	if !s.ok {
		return false
	}
	lo, hi := s.find(probe)
	src := &cur.pointSrc
	segs := append(src.listSegs[:0], s.segs[lo:hi]...)
	rows := append(src.listRows[:0], s.rows[lo:hi]...)
	*src = segRowSource{list: true, listSegs: segs, listRows: rows, listOne: oneSegment(segs), listKeys: src.listKeys, listPerm: src.listPerm,
		listHits: src.listHits, cols: segColPlansFor(cur.tbl)}
	cur.seekGen = seekGens.Add(1)
	cur.rowids, cur.rows = cur.rowids[:0], cur.rows[:0]
	cur.segSrc, cur.segCur, cur.segSrcCols = src, nil, src.cols
	cur.streamErrPending = nil
	cur.pos = -1
	cur.rowidNull = false
	cur.keyListReady = true
	segSortedSeeksServed.Add(1)
	return true
}

// segSortedRowsFor builds cur's segSortedRows for h: the committed segments,
// with nothing in the log or the session's store over them, ascending by
// rowid, each as wide as the table and holding h.col as an int64 block with no
// NULL and no exception. A REAL column's values are read as REALs, and the
// INTEGER PRIMARY KEY's are the rowids; both are left to the built rows.
func (p *ReadOnlyPager) segSortedRowsFor(cur *vdbeCursor, h *sortedSeekHint) *segSortedRows {
	s := &segSortedRows{hint: h}
	if !p.segSeekable(cur) || h.col < 0 || h.col >= len(cur.tbl.cols) || h.col == cur.tbl.ipkIndex || cur.tbl.cols[h.col].Aff == affReal {
		return s
	}
	root := cur.tbl.root
	segs := p.segs.byRoot[root]
	if p.segs.isLive(root) {
		var ok bool
		if segs, ok = p.segs.liveBaseSegments(root); !ok {
			return s
		}
	} else if !p.segs.cleanFor(root) {
		return s
	}
	n := 0
	for _, sg := range segs {
		if sg == nil || len(sg.cols) != len(cur.tbl.cols) {
			return s
		}
		n += sg.nRows
	}
	keys, ksegs, krows := make([]int64, 0, n), make([]*segment, 0, n), make([]int, 0, n)
	var prevRid uint64
	for _, sg := range segs {
		if sg.nRows == 0 {
			continue
		}
		if len(keys) > 0 && sg.Rowid(0) <= prevRid {
			return s
		}
		b, ok := segCleanInt64Column(sg, h.col)
		if !ok || len(b) < sg.nRows {
			return s
		}
		for i := range sg.nRows {
			keys, ksegs, krows = append(keys, b[i]), append(ksegs, sg), append(krows, i)
		}
		prevRid = sg.Rowid(sg.nRows - 1)
	}
	perm := make([]int32, len(keys))
	for i := range perm {
		perm[i] = int32(i)
	}
	slices.SortFunc(perm, func(a, b int32) int {
		if c := cmp.Compare(keys[a], keys[b]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	s.keys, s.segs, s.rows = make([]int64, len(perm)), make([]*segment, len(perm)), make([]int, len(perm))
	for i, j := range perm {
		s.keys[i], s.segs[i], s.rows[i] = keys[j], ksegs[j], krows[j]
	}
	s.ok = true
	return s
}

// find is the run of rows whose key equals probe once probe takes the
// comparison's affinity. Every key is an INTEGER: a REAL equals one only when
// it is that integer exactly, and NULL, TEXT and BLOB equal none.
func (s *segSortedRows) find(probe Value) (int, int) {
	probe = applyAffinityToValue(probe, s.hint.aff)
	var k int64
	switch probe.Typ {
	case Int:
		k = probe.I
	case Float:
		f := probe.F
		if f != math.Trunc(f) || f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
			return 0, 0
		}
		k = int64(f)
	default:
		return 0, 0
	}
	lo, _ := slices.BinarySearch(s.keys, k)
	hi := lo
	for hi < len(s.keys) && s.keys[hi] == k {
		hi++
	}
	return lo, hi
}
