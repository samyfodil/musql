package engine

import (
	"cmp"
	"slices"
	"sort"
)

// sortedRows is OpSortedSeek's state on a cursor (vdbeCursor.sorted).
type sortedRows struct {
	hint *sortedSeekHint
	// ok is false when the rows could not be sorted; the cursor then keeps
	// all of them.
	ok bool
	// rows and ids are the cursor's rows sorted by hint.col, stably, and pos
	// each one's place in orig/origIDs, the list as the cursor had it.
	rows    [][]Value
	ids     []uint64
	pos     []int32
	orig    [][]Value
	origIDs []uint64
	// ints is rows' key column when every value is an INTEGER.
	ints []int64
	// base is where the current run starts in rows.
	base int
}

// sortedSeek narrows cur to the rows whose column h.col equals probe, sorting
// the full list by that column the first time.
func (cur *vdbeCursor) sortedSeek(h *sortedSeekHint, probe Value, enc TextEncoding) {
	s := cur.sorted
	if s == nil || s.hint != h {
		cur.unsort()
		s = cur.sortRowsBy(h, enc)
		cur.sorted = s
	}
	if !s.ok {
		return
	}
	lo, hi := s.find(probe, enc)
	cur.rows, cur.rowids, s.base = s.rows[lo:hi:hi], s.ids[lo:hi:hi], lo
}

// unsort gives cur back its full row list, as it was before any seek.
func (cur *vdbeCursor) unsort() {
	if s := cur.sorted; s != nil && s.ok {
		cur.rows, cur.rowids = s.orig, s.origIDs
	}
	cur.sorted = nil
}

func (cur *vdbeCursor) sortRowsBy(h *sortedSeekHint, enc TextEncoding) *sortedRows {
	s := &sortedRows{hint: h}
	if !cur.materialized || cur.sub != nil || cur.vt != nil || cur.segSrc != nil || len(cur.rowids) != len(cur.rows) || len(cur.rows) > 1<<31-1 {
		return s
	}
	cur.normalizeRowsNow()
	n := len(cur.rows)
	ints := make([]int64, n)
	for i, row := range cur.rows {
		if h.col >= len(row) {
			return s
		}
		if v := row[h.col]; v.Typ == Int && ints != nil {
			ints[i] = v.I
		} else {
			ints = nil
		}
	}
	pos := make([]int32, n)
	for i := range pos {
		pos[i] = int32(i)
	}
	// The original position breaks ties, so the order is total and a run keeps
	// the list's order.
	if ints != nil {
		slices.SortFunc(pos, func(a, b int32) int {
			if c := cmp.Compare(ints[a], ints[b]); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
	} else {
		slices.SortFunc(pos, func(a, b int32) int {
			if c := compareValuesCollatedEnc(cur.rows[a][h.col], cur.rows[b][h.col], h.coll, enc); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
	}
	s.rows, s.ids = make([][]Value, n), make([]uint64, n)
	if ints != nil {
		s.ints = make([]int64, n)
	}
	for i, p := range pos {
		s.rows[i], s.ids[i] = cur.rows[p], cur.rowids[p]
		if ints != nil {
			s.ints[i] = ints[p]
		}
	}
	s.pos, s.orig, s.origIDs, s.ok = pos, cur.rows, cur.rowids, true
	return s
}

// find is the run of rows whose key column equals probe, once probe takes the
// comparison's affinity, under the hint's collation. A NULL equals nothing.
func (s *sortedRows) find(probe Value, enc TextEncoding) (int, int) {
	probe = applyAffinityToValue(probe, s.hint.aff)
	if probe.Typ == Null {
		return 0, 0
	}
	if s.ints != nil && probe.Typ == Int {
		lo, _ := slices.BinarySearch(s.ints, probe.I)
		hi := lo
		for hi < len(s.ints) && s.ints[hi] == probe.I {
			hi++
		}
		return lo, hi
	}
	col, coll := s.hint.col, s.hint.coll
	lo := sort.Search(len(s.rows), func(i int) bool {
		return compareValuesCollatedEnc(s.rows[i][col], probe, coll, enc) >= 0
	})
	hi := lo + sort.Search(len(s.rows)-lo, func(i int) bool {
		return compareValuesCollatedEnc(s.rows[lo+i][col], probe, coll, enc) > 0
	})
	return lo, hi
}
