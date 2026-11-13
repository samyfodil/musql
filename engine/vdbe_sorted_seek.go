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
	// keyOrder is the order rows compares the hint's key columns in (0 is
	// hint.col, j its more[j-1]); nil is that order itself. A probe wants
	// the run equal on every column, which any order groups alike.
	keyOrder []int
	// base is where the current run starts in rows.
	base int
}

// sortedSeek narrows cur to the rows whose column h.col equals probe, sorting
// the full list by that column the first time.
func (cur *vdbeCursor) sortedSeek(h *sortedSeekHint, probe Value, regs []Value, enc TextEncoding) {
	s := cur.sorted
	if s == nil || !s.hint.sameSort(h) {
		cur.unsort()
		s = cur.sortRowsBy(h, enc)
		cur.sorted = s
	}
	if !s.ok {
		return
	}
	lo, hi := s.find(h, probe, regs, enc)
	cur.rows, cur.rowids, s.base = s.rows[lo:hi:hi], s.ids[lo:hi:hi], lo
}

// unsort gives cur back its full row list, as it was before any seek.
func (cur *vdbeCursor) unsort() {
	if s := cur.sorted; s != nil && s.ok {
		cur.rows, cur.rowids = s.orig, s.origIDs
	}
	cur.sorted = nil
}

// intSortKey is a row's leading key columns that every row holds as an
// INTEGER or a NULL (nulls, a bit per column), and its position.
type intSortKey struct {
	k     [4]int64
	nulls uint8
	p     int32
}

func (cur *vdbeCursor) sortRowsBy(h *sortedSeekHint, enc TextEncoding) *sortedRows {
	s := &sortedRows{hint: h}
	if !cur.materialized || cur.sub != nil || cur.vt != nil || cur.segSrc != nil || len(cur.rowids) != len(cur.rows) || len(cur.rows) > 1<<31-1 {
		return s
	}
	cur.normalizeRowsNow()
	n := len(cur.rows)
	k := 1 + len(h.more)
	keyCol := func(j int) (int, string) {
		if j == 0 {
			return h.col, h.coll
		}
		return h.more[j-1].col, h.more[j-1].coll
	}
	for _, row := range cur.rows {
		if h.col >= len(row) || slices.ContainsFunc(h.more, func(m sortedSeekCol) bool { return m.col >= len(row) }) {
			return s
		}
	}
	// The key columns every row holds as an INTEGER or a NULL, up to four, go
	// first and are copied out flat and compared as int64s: an integer
	// compares as one under any collating sequence (as autoIndexOrderCursor
	// does), and a NULL before it. Only rows they leave tied compare the rest
	// as Values.
	var order, rest []int
	for j := range k {
		col, _ := keyCol(j)
		if len(order) < len(intSortKey{}.k) && !slices.ContainsFunc(cur.rows, func(row []Value) bool { return row[col].Typ != Int && row[col].Typ != Null }) {
			order = append(order, j)
		} else {
			rest = append(rest, j)
		}
	}
	lead := len(order)
	order = append(order, rest...)
	keys := make([]intSortKey, n)
	for i, row := range cur.rows {
		for j := range lead {
			col, _ := keyCol(order[j])
			if v := row[col]; v.Typ == Null {
				keys[i].nulls |= 1 << uint(j)
			} else {
				keys[i].k[j] = v.I
			}
		}
		keys[i].p = int32(i)
	}
	// The original position breaks ties, so the order is total and a run keeps
	// the list's order.
	slices.SortFunc(keys, func(a, b intSortKey) int {
		for j := range lead {
			na, nb := a.nulls>>uint(j)&1, b.nulls>>uint(j)&1
			if c := cmp.Compare(nb, na); c != 0 {
				return c // a NULL first
			}
			if c := cmp.Compare(a.k[j], b.k[j]); na == 0 && c != 0 {
				return c
			}
		}
		for _, j := range order[lead:] {
			col, coll := keyCol(j)
			if c := compareValuesCollatedEnc(cur.rows[a.p][col], cur.rows[b.p][col], coll, enc); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.p, b.p)
	})
	pos := make([]int32, n)
	for i := range keys {
		pos[i] = keys[i].p
	}
	var ints []int64
	if lead == 1 && k == 1 && !slices.ContainsFunc(keys, func(e intSortKey) bool { return e.nulls != 0 }) {
		ints = make([]int64, n)
		for i := range keys {
			ints[i] = keys[i].k[0]
		}
	}
	s.rows, s.ids, s.ints = make([][]Value, n), make([]uint64, n), ints
	if !slices.IsSorted(order) {
		s.keyOrder = order
	}
	for i, p := range pos {
		s.rows[i], s.ids[i] = cur.rows[p], cur.rowids[p]
	}
	s.pos, s.orig, s.origIDs, s.ok = pos, cur.rows, cur.rowids, true
	return s
}

// find is the run of rows whose key column equals probe, and each further
// column the key in its register (regs), once every key takes its
// comparison's affinity, under its collation. A NULL equals nothing.
func (s *sortedRows) find(h *sortedSeekHint, probe Value, regs []Value, enc TextEncoding) (int, int) {
	probe = applyAffinityToValue(probe, h.aff)
	if probe.Typ == Null {
		return 0, 0
	}
	if len(h.more) > 0 {
		return s.findTuple(h, probe, regs, enc)
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

// findTuple is find over the hint's further columns too.
func (s *sortedRows) findTuple(h *sortedSeekHint, probe Value, regs []Value, enc TextEncoding) (int, int) {
	keys := make([]Value, len(h.more))
	for i, mc := range h.more {
		if keys[i] = applyAffinityToValue(regs[mc.reg], mc.aff); keys[i].Typ == Null {
			return 0, 0
		}
	}
	cmpKey := func(row []Value, j int) int {
		if j == 0 {
			return compareValuesCollatedEnc(row[h.col], probe, h.coll, enc)
		}
		mc := h.more[j-1]
		return compareValuesCollatedEnc(row[mc.col], keys[j-1], mc.coll, enc)
	}
	cmpRow := func(row []Value) int {
		if s.keyOrder != nil {
			for _, j := range s.keyOrder {
				if c := cmpKey(row, j); c != 0 {
					return c
				}
			}
			return 0
		}
		for j := range len(h.more) + 1 {
			if c := cmpKey(row, j); c != 0 {
				return c
			}
		}
		return 0
	}
	lo := sort.Search(len(s.rows), func(i int) bool { return cmpRow(s.rows[i]) >= 0 })
	hi := lo + sort.Search(len(s.rows)-lo, func(i int) bool { return cmpRow(s.rows[lo+i]) > 0 })
	return lo, hi
}

// sameSort reports whether b sorts the rows as a does: the same columns under
// the same collations and affinities. A level a LEFT JOIN emits twice, for
// its matched rows and for its NULL row, seeks with two hints that do.
func (a *sortedSeekHint) sameSort(b *sortedSeekHint) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || a.col != b.col || a.coll != b.coll || a.aff != b.aff || len(a.more) != len(b.more) {
		return false
	}
	for i := range a.more {
		if x, y := a.more[i], b.more[i]; x.col != y.col || x.coll != y.coll || x.aff != y.aff {
			return false
		}
	}
	return true
}
