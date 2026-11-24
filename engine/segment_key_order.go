package engine

import (
	"slices"
	"strconv"
	"sync/atomic"
)

// segRowsInKeyOrder reports whether the table's rows, read in rowid order,
// already run in key's order, the order OpAutoIndexOrder sorts them into:
// every key column a clean int64 block, ascending, and no row below the one
// before it. Rows equal on the key keep rowid order, which is the index's own
// order among them. It reads the segments alone, so a table with rows in the
// delta log, or written in the session, declines.
func (p *ReadOnlyPager) segRowsInKeyOrder(root uint32, key *autoIndexKey) bool {
	if p == nil || p.segs == nil || key == nil || key.multiOr != nil || !p.segs.cleanFor(root) {
		return false
	}
	segs := p.segs.byRoot[root]
	if p.segs.isLive(root) {
		// A session's store over segments it has not written to: its rows
		// are those segments' (newSegRowSource).
		var ok bool
		if segs, ok = p.segs.liveBaseSegments(root); !ok {
			return false
		}
	}
	n := len(key.cols) - 1
	if n < 1 || len(key.desc) != n+1 || key.cols[n] != -1 || key.desc[n] {
		return false
	}
	for j := range n {
		if key.cols[j] < 0 || key.desc[j] {
			return false
		}
	}
	cols := key.cols[:n]
	var prev []int64
	var prevRid uint64
	for _, s := range segs {
		blocks, ok := s.ascendingOn(cols)
		if !ok {
			return false
		}
		if s.nRows == 0 {
			continue
		}
		first := make([]int64, n)
		for j, b := range blocks {
			first[j] = b[0]
		}
		if prev != nil && (s.Rowid(0) <= prevRid || slices.Compare(first, prev) < 0) {
			return false
		}
		if prev == nil {
			prev = make([]int64, n)
		}
		for j, b := range blocks {
			prev[j] = b[s.nRows-1]
		}
		prevRid = s.Rowid(s.nRows - 1)
	}
	return prev != nil
}

// ascendingOn returns the blocks of cols when each is a clean int64 block and
// the rows ascend on them in turn, and whether they do; the answer is kept.
func (s *segment) ascendingOn(cols []int) ([][]int64, bool) {
	blocks := make([][]int64, len(cols))
	for j, c := range cols {
		b, ok := segCleanInt64Column(s, c)
		if !ok || len(b) < s.nRows {
			return nil, false
		}
		blocks[j] = b
	}
	var kb [64]byte
	key := kb[:0]
	for _, c := range cols {
		key = strconv.AppendInt(append(key, ','), int64(c), 10)
	}
	k := string(key)
	s.keyOrderMu.Lock()
	defer s.keyOrderMu.Unlock()
	if ok, known := s.keyOrder[k]; known {
		return blocks, ok
	}
	ok := true
	for i := 1; i < s.nRows && ok; i++ {
		for _, b := range blocks {
			if b[i] != b[i-1] {
				ok = b[i] > b[i-1]
				break
			}
		}
	}
	if s.keyOrder == nil {
		s.keyOrder = map[string]bool{}
	}
	s.keyOrder[k] = ok
	return blocks, ok
}

// segKeyOrderList positions a lazy source on every row of cur's table in
// key's order -- the outermost scan OpAutoIndexOrder would otherwise build and
// sort -- where segRowsInKeyOrder's rules hold but for the rows' own order:
// each segment's rows sorted on the key columns (kept with the segment), the
// segments one after another, their key ranges following each other. nil
// where that is not so.
func (p *ReadOnlyPager) segKeyOrderList(cur *vdbeCursor, key *autoIndexKey, into *segRowSource) *segRowSource {
	if !p.segSeekable(cur) || key == nil || key.multiOr != nil || !p.segs.cleanFor(cur.tbl.root) {
		return nil
	}
	root := cur.tbl.root
	segs := p.segs.byRoot[root]
	if p.segs.isLive(root) {
		var ok bool
		if segs, ok = p.segs.liveBaseSegments(root); !ok {
			return nil
		}
	}
	n := len(key.cols) - 1
	if n < 1 || len(key.desc) != n+1 || key.cols[n] != -1 || key.desc[n] {
		return nil
	}
	for j := range n {
		if key.cols[j] < 0 || key.desc[j] {
			return nil
		}
	}
	cols := key.cols[:n]
	lsegs, lrows := into.listSegs[:0], into.listRows[:0]
	var prev []int64
	var prevRid uint64
	for _, s := range segs {
		if s == nil || len(s.cols) != len(cur.tbl.cols) {
			return nil
		}
		blocks, perm, ok := s.sortedOn(cols)
		if !ok {
			return nil
		}
		if s.nRows == 0 {
			continue
		}
		if prev != nil {
			if s.Rowid(0) <= prevRid {
				return nil
			}
			first := int(perm[0])
			for j, b := range blocks {
				if b[first] != prev[j] {
					if b[first] < prev[j] {
						return nil
					}
					break
				}
			}
		}
		if prev == nil {
			prev = make([]int64, n)
		}
		last := int(perm[len(perm)-1])
		for j, b := range blocks {
			prev[j] = b[last]
		}
		prevRid = s.Rowid(s.nRows - 1)
		for _, r := range perm {
			lsegs, lrows = append(lsegs, s), append(lrows, int(r))
		}
	}
	if prev == nil {
		return nil
	}
	*into = segRowSource{list: true, listSegs: lsegs, listRows: lrows, listOne: oneSegment(lsegs), listKeys: into.listKeys, listPerm: into.listPerm,
		listHits: into.listHits, cols: segColPlansFor(cur.tbl)}
	return into
}

// segScanListMin is the fewest rows segScanList lists: a smaller table is
// built, which native code walks from its first rewind.
const segScanListMin = 1024

// segScanLists counts the lists segScanList built, for tests.
var segScanLists atomic.Int64

// segScanList positions a lazy source on every row of cur's table in rowid
// order, for a full scan below the outermost loop. C scans such a table again
// for each outer row, reading only the columns it tests; built instead, every
// row took the table's full width. nil where the segments are not the whole
// answer, their rowids do not follow each other, or the table is small.
func (p *ReadOnlyPager) segScanList(cur *vdbeCursor, into *segRowSource) *segRowSource {
	if !p.segSeekable(cur) || !p.segs.cleanFor(cur.tbl.root) {
		return nil
	}
	root := cur.tbl.root
	segs := p.segs.byRoot[root]
	if p.segs.isLive(root) {
		var ok bool
		if segs, ok = p.segs.liveBaseSegments(root); !ok {
			return nil
		}
	}
	n := 0
	for _, s := range segs {
		if s == nil || len(s.cols) != len(cur.tbl.cols) {
			return nil
		}
		n += s.nRows
	}
	if n < segScanListMin {
		return nil
	}
	var prev *segment
	for _, s := range segs {
		if s.nRows == 0 {
			continue
		}
		if prev != nil && int64(s.Rowid(0)) <= int64(prev.Rowid(prev.nRows-1)) {
			return nil
		}
		prev = s
	}
	lsegs, lrows := into.listSegs[:0], into.listRows[:0]
	if cur.segFilter == nil {
		lsegs, lrows = slices.Grow(lsegs, n), slices.Grow(lrows, n)
	}
	var sel []int64
	for _, s := range segs {
		if cur.segFilter != nil {
			// The pre-filter names its own columns and the statement's
			// parameters only, so it selects the same rows on every rewind.
			var ok bool
			if sel, ok = segSelectRows(s, cur.segFilter, cur.tbl.ipkIndex, cur.params, sel[:0]); ok {
				for _, r := range sel {
					lsegs, lrows = append(lsegs, s), append(lrows, int(r))
				}
				continue
			}
		}
		for r := range s.nRows {
			lsegs, lrows = append(lsegs, s), append(lrows, r)
		}
	}
	segScanLists.Add(1)
	*into = segRowSource{list: true, listSegs: lsegs, listRows: lrows, listOne: oneSegment(lsegs), listKeys: into.listKeys, listPerm: into.listPerm,
		listHits: into.listHits, cols: segColPlansFor(cur.tbl)}
	return into
}

// sortedOn returns the blocks of cols when each is a clean int64 block, and
// the rows in their order, ties in row order; the order is kept.
func (s *segment) sortedOn(cols []int) ([][]int64, []int32, bool) {
	blocks := make([][]int64, len(cols))
	for j, c := range cols {
		b, ok := segCleanInt64Column(s, c)
		if !ok || len(b) < s.nRows {
			return nil, nil, false
		}
		blocks[j] = b
	}
	var kb [64]byte
	key := kb[:0]
	for _, c := range cols {
		key = strconv.AppendInt(append(key, ','), int64(c), 10)
	}
	k := string(key)
	s.keyOrderMu.Lock()
	defer s.keyOrderMu.Unlock()
	if perm, known := s.keyPerm[k]; known {
		return blocks, perm, true
	}
	perm := make([]int32, s.nRows)
	for i := range perm {
		perm[i] = int32(i)
	}
	inOrder := true
	for r := 1; r < s.nRows && inOrder; r++ {
		for _, bl := range blocks {
			if bl[r-1] != bl[r] {
				inOrder = bl[r-1] < bl[r]
				break
			}
		}
	}
	var scratch []int32
	for _, bl := range slices.Backward(blocks) {
		if inOrder {
			break
		}
		perm, scratch = radixSortByKeyWith(perm, scratch, func(r int32) int64 { return bl[r] })
	}
	if s.keyPerm == nil {
		s.keyPerm = map[string][]int32{}
	}
	s.keyPerm[k] = perm
	return blocks, perm, true
}

// segStoresReal reports whether a segment of the table at root stores column
// col as REALs: a PhysFloat64 or PhysNumeric block.
func (p *ReadOnlyPager) segStoresReal(root uint32, col int) bool {
	if p == nil || p.segs == nil {
		return false
	}
	segs := p.segs.byRoot[root]
	if p.segs.isLive(root) {
		var ok bool
		if segs, ok = p.segs.liveBaseSegments(root); !ok {
			return false
		}
	}
	for _, s := range segs {
		if col < len(s.cols) && (s.cols[col].phys == PhysFloat64 || s.cols[col].phys == PhysNumeric) {
			return true
		}
	}
	return false
}
