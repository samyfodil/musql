package engine

import (
	"slices"
	"strconv"
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
