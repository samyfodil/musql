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
	// one is the table's one segment, when it is: then perm, its rows in the
	// column's order, and block, the column, are the segment's own (sortedOn),
	// and keys, segs and rows are not built.
	one   *segment
	perm  []int32
	block []int64
	// order is the automatic index key a run's rows follow, when the seek
	// stands in for the OpAutoIndexOrder that would have built and sorted the
	// table; ordered marks the runs already put in it, by first position.
	order   *autoIndexKey
	tbl     *resolvedTable
	enc     TextEncoding
	ordered map[int]bool
}

// segSortedSeek positions cur on the rows whose column h.col equals probe,
// straight from the segments, as sortedSeek narrows built rows. false, with
// nothing changed, where the segments are not the whole table or the column
// is not an INTEGER block in every one of them.
//
// An OpAutoIndexOrder waiting on cur is taken over rather than applied: a run
// of the built rows, once sorted by the key, is those rows in the key's order,
// so each run is put in that order here, without building the table.
func (cur *vdbeCursor) segSortedSeek(h *sortedSeekHint, probe Value, enc TextEncoding) bool {
	if h.built || len(h.more) > 0 || cur.materialized || cur.fill != nil || cur.ranged || cur.seekConfigured || cur.idxSeekConfigured || cur.seekSetConfigured ||
		cur.segSorted == nil && (cur.segSrc != nil || cur.keyListReady) {
		return false
	}
	orderOp := cur.segOrder
	if cur.pendingOrder != nil {
		orderOp = cur.pendingOrder
	}
	var key *autoIndexKey
	if orderOp != nil {
		if key = orderOp.P4.(*autoIndexKey); !segRunOrderable(cur.tbl, key) {
			return false
		}
	}
	s := cur.segSorted
	if s == nil || !s.hint.sameSort(h) || s.order != key {
		s = cur.pager.segSortedRowsFor(cur, h)
		if s.ok && key != nil && s.one != nil {
			s.perm = slices.Clone(s.perm) // the segment's own, shared
		}
		s.order, s.tbl, s.enc = key, cur.tbl, enc
		cur.segSorted = s
	}
	if !s.ok {
		return false
	}
	cur.segOrder, cur.pendingOrder = orderOp, nil
	lo, hi := s.find(probe)
	s.orderRun(lo, hi)
	src := &cur.pointSrc
	segs, rows := src.listSegs[:0], src.listRows[:0]
	if s.one != nil {
		for _, r := range s.perm[lo:hi] {
			segs, rows = append(segs, s.one), append(rows, int(r))
		}
	} else {
		segs, rows = append(segs, s.segs[lo:hi]...), append(rows, s.rows[lo:hi]...)
	}
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

// unserveSegSorted takes back a cursor segSortedSeek positioned when the
// segments stop serving its seeks: its rows are built, in the order the seek
// took over.
func (cur *vdbeCursor) unserveSegSorted() {
	if cur.segSorted == nil || !cur.keyListReady {
		return
	}
	cur.keyListReady, cur.segSrc, cur.segCur, cur.segSorted, cur.pos = false, nil, nil, nil, -1
	if cur.segOrder != nil {
		cur.pendingOrder, cur.segOrder = cur.segOrder, nil
	}
}

// segRunOrderable reports whether segSortedRows.orderRun can put a run in
// key's order: plain columns of the table, or the rowid.
func segRunOrderable(tbl *resolvedTable, key *autoIndexKey) bool {
	if key.multiOr != nil || key.exprs != nil || len(key.cols) == 0 || len(key.colls) < len(key.cols) {
		return false
	}
	for _, c := range key.cols {
		if c != -1 && (c < 0 || c >= len(tbl.cols)) {
			return false
		}
	}
	return true
}

// orderRun puts the run [lo, hi) in s.order's order, as autoIndexOrderCursor
// orders the built rows: the key's values as the built rows hold them, ties
// in the run's own order, which is the rowids'.
func (s *segSortedRows) orderRun(lo, hi int) {
	if s.order == nil || hi-lo < 2 || s.ordered[lo] {
		return
	}
	key, n := s.order, hi-lo
	nk := len(key.cols)
	vals := make([]Value, n*nk)
	for i := range n {
		sg, r := s.at(lo + i)
		for k, c := range key.cols {
			var v Value
			switch {
			case c < 0 || c == s.tbl.ipkIndex:
				v = Value{Typ: Int, I: int64(sg.Rowid(r))}
			default:
				if v = sg.Value(c, r); v.Typ == Int && s.tbl.cols[c].Aff == affReal {
					v = Value{Typ: Float, F: float64(v.I)}
				}
			}
			vals[i*nk+k] = v
		}
	}
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	autoIndexSortValues(perm, key, func(i, k int) Value { return vals[i*nk+k] }, s.enc, nil)
	if s.one != nil {
		run := slices.Clone(s.perm[lo:hi])
		for i, j := range perm {
			s.perm[lo+i] = run[j]
		}
	} else {
		segs, rows := slices.Clone(s.segs[lo:hi]), slices.Clone(s.rows[lo:hi])
		for i, j := range perm {
			s.segs[lo+i], s.rows[lo+i] = segs[j], rows[j]
		}
	}
	if s.ordered == nil {
		s.ordered = map[int]bool{}
	}
	s.ordered[lo] = true
}

// at is the segment and row at position i of the column's order.
func (s *segSortedRows) at(i int) (*segment, int) {
	if s.one != nil {
		return s.one, int(s.perm[i])
	}
	return s.segs[i], s.rows[i]
}

// segSortedRowsFor builds cur's segSortedRows for h: the committed segments,
// with nothing in the log or the session's store over them, ascending by
// rowid, each as wide as the table and holding h.col as an int64 block with no
// exception. A NULL equals no probe, so its rows are left out. A REAL column's
// values are read as REALs, and the INTEGER PRIMARY KEY's are the rowids; both
// are left to the built rows.
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
	n, nonEmpty := 0, 0
	var one *segment
	for _, sg := range segs {
		if sg == nil || len(sg.cols) != len(cur.tbl.cols) {
			return s
		}
		n += sg.nRows
		if sg.nRows > 0 {
			nonEmpty, one = nonEmpty+1, sg
		}
	}
	if nonEmpty == 1 {
		// sortedOn's stable sort keeps a segment's rows, which are in rowid
		// order, in that order among equal keys.
		if blocks, perm, ok := one.sortedOn([]int{h.col}); ok {
			s.one, s.perm, s.block, s.ok = one, perm, blocks[0], true
			return s
		}
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
		b, ok := sg.Int64Column(h.col)
		if !ok || len(b) < sg.nRows || sg.hasException(h.col) {
			return s
		}
		nulls := sg.cols[h.col].nullOff != 0
		for i := range sg.nRows {
			if !nulls || !sg.isNull(h.col, i) {
				keys, ksegs, krows = append(keys, b[i]), append(ksegs, sg), append(krows, i)
			}
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
	if s.one != nil {
		lo, _ := slices.BinarySearchFunc(s.perm, k, func(r int32, k int64) int { return cmp.Compare(s.block[r], k) })
		hi := lo
		for hi < len(s.perm) && s.block[s.perm[hi]] == k {
			hi++
		}
		return lo, hi
	}
	lo, _ := slices.BinarySearch(s.keys, k)
	hi := lo
	for hi < len(s.keys) && s.keys[hi] == k {
		hi++
	}
	return lo, hi
}
