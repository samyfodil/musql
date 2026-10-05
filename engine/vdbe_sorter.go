// Package engine implements the sorter cursor: an insertion-ordered list of
// rows stably sorted by an ORDER BY key. Results never depend on which sorter
// site does the sorting, as all sites use orderTermLess.
package engine

import "sort"

// sorterKeyInfo describes an ORDER BY key: number of key columns, DESC flags,
// NULLS placement, and collations per column.
type sorterKeyInfo struct {
	nKey  int
	desc  []bool       // len == nKey
	nulls []NullsOrder // len == nKey
	coll  []string     // len == nKey

	// bound is the number of leading sorted rows to keep (OFFSET+LIMIT for
	// ORDER BY ... LIMIT queries). When > 0, the sorter is a bounded max-heap.
	// 0 means unbounded (keep everything, full stable sort).
	bound int
}

// sorterEntry is one row a sorter holds: its ORDER BY key columns (compared
// by (*vdbeSorter).sort, never themselves returned to the caller) and its
// payload (output) columns (returned via OpSorterData/OpRecordColumn once
// sorted).
type sorterEntry struct {
	key []Value
	row []Value
	seq int // insertion order, the stable tie-break when keys compare equal
}

// vdbeSorter is the sorter cursor OpSorterOpen creates: an insertion-ordered
// list of entries (insert, called by OpSorterInsert) that sort reorders in
// place, after which data/next walk it like any other cursor (OpSorterData/
// OpSorterNext).
type vdbeSorter struct {
	keyInfo sorterKeyInfo
	enc     TextEncoding // the database's text encoding; BINARY order depends on it (utf16.go)
	entries []sorterEntry
	pos     int
	nextSeq int // monotonic insertion counter (stable tie-break)
}

// keyLess compares two ORDER BY key tuples under this sorter's comparator
// alone -- per-column DESC direction, NULLS placement and collation, falling
// through to the next column on a tie -- reporting whether a sorts before b and
// whether they tie on EVERY key column (in which case less is meaningless and
// the caller applies the insertion-order tie-break).
func (s *vdbeSorter) keyLess(a, b []Value) (less, tie bool) {
	for k := 0; k < s.keyInfo.nKey; k++ {
		ot := OrderTerm{Desc: s.keyInfo.desc[k], Nulls: s.keyInfo.nulls[k]}
		l, equal := orderTermLess(ot, a[k], b[k], atOrEmpty(s.keyInfo.coll, k), s.enc)
		if equal {
			continue
		}
		return l, false
	}
	return false, true
}

// entryBefore reports whether entry a sorts strictly before entry b under the
// TOTAL order this sorter imposes: the ORDER BY key comparator first, ties
// broken by insertion order (seq). This is the exact order sort.SliceStable
// produces over insertion-ordered entries, so the bounded and unbounded paths
// agree byte-for-byte.
func (s *vdbeSorter) entryBefore(a, b sorterEntry) bool {
	less, tie := s.keyLess(a.key, b.key)
	if tie {
		return a.seq < b.seq
	}
	return less
}

// loses reports whether a row would be thrown away by insert, allowing the
// caller to skip building its record. For bounded sorts, a new row loses if it
// doesn't beat the largest entry held (ties lose).
func (s *vdbeSorter) loses(key []Value) bool {
	if s.keyInfo.bound <= 0 || len(s.entries) < s.keyInfo.bound {
		return false
	}
	// keyLess reports false on a tie; this row's seq is larger than all held seq.
	less, _ := s.keyLess(key, s.entries[0].key)
	return !less
}

// newSorter creates an empty sorter cursor for the given key shape.
func newSorter(ki *sorterKeyInfo, enc TextEncoding) *vdbeSorter {
	return &vdbeSorter{keyInfo: *ki, enc: enc}
}

// insert appends a record (key columns first, then payload) as a new entry.
func (s *vdbeSorter) insert(rec []Value) {
	e := sorterEntry{
		key: rec[:s.keyInfo.nKey],
		row: rec[s.keyInfo.nKey:],
		seq: s.nextSeq,
	}
	s.nextSeq++

	b := s.keyInfo.bound
	if b <= 0 {
		s.entries = append(s.entries, e) // unbounded: keep everything
		return
	}
	// Bounded top-N: keep only the b entries smallest under entryBefore, in a
	// max-heap (s.entries[0] is the current largest -- the first to evict). While
	// under capacity, push; once full, an entry that sorts before the current max
	// replaces it. Cost is O(n·log b), and the retained set is exactly the b
	// smallest, which sort() then orders.
	if len(s.entries) < b {
		s.entries = append(s.entries, e)
		s.siftUp(len(s.entries) - 1)
		return
	}
	if s.entryBefore(e, s.entries[0]) {
		s.entries[0] = e
		s.siftDown(0)
	}
}

// siftUp/siftDown maintain s.entries as a MAX-heap under entryBefore (the root
// is the entry that sorts LAST). Used only on the bounded path.
func (s *vdbeSorter) siftUp(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !s.entryBefore(s.entries[p], s.entries[i]) { // parent already >= child
			break
		}
		s.entries[p], s.entries[i] = s.entries[i], s.entries[p]
		i = p
	}
}

func (s *vdbeSorter) siftDown(i int) {
	n := len(s.entries)
	for {
		largest, l, r := i, 2*i+1, 2*i+2
		if l < n && s.entryBefore(s.entries[largest], s.entries[l]) {
			largest = l
		}
		if r < n && s.entryBefore(s.entries[largest], s.entries[r]) {
			largest = r
		}
		if largest == i {
			break
		}
		s.entries[i], s.entries[largest] = s.entries[largest], s.entries[i]
		i = largest
	}
}

// sort stably reorders s.entries by key: comparing each ORDER BY column in
// turn with orderTermLess (value_compare.go -- per-column collation (coll),
// DESC-flag direction, and per-column NULLS FIRST/LAST placement, defaulting
// to NULLs-lowest storage-class ordering when no explicit NULLS clause was
// given), falling through to the next key column on a tie, and preserving
// original (scan/insertion) order once every key column ties. Every other
// ORDER BY sort site in this engine -- compound ORDER BY
// (vdbe_compound_codegen.go), GROUP BY/DISTINCT (vdbe_group_distinct.go),
// recursive-CTE ordering (cte.go) -- runs the identical shape:
//
//	sort.SliceStable(rows, func(i, j int) bool {
//	    for k, ot := range orderBy {
//	        less, equal := orderTermLess(ot, keys[i][k], keys[j][k], orderColl[k])
//	        if equal { continue }
//	        return less
//	    }
//	    return false
//	})
//
// so a tie breaks the same way at all of them for the same input rows (all
// scan in the same ascending-rowid order before sorting, and all use a stable
// sort algorithm).
//
// It reports whether the sorter has any entries at all -- mirroring
// OpRewind's "jump if empty" convention: false with zero entries (nothing to
// drain), true otherwise, with the first sorted entry now current (pos==0).
func (s *vdbeSorter) sort() bool {
	// entryBefore is a strict TOTAL order (key comparator, then insertion seq),
	// so an ordinary sort.Slice reproduces the stable order exactly -- and, on the
	// bounded path where insert's max-heap has scrambled insertion order, the
	// explicit seq tie-break is what keeps ties byte-identical to a full stable
	// sort of the whole input. seq is in insertion order for the unbounded path,
	// so this matches the previous sort.SliceStable there too.
	sort.Slice(s.entries, func(i, j int) bool {
		return s.entryBefore(s.entries[i], s.entries[j])
	})
	s.pos = 0
	return len(s.entries) > 0
}

// data returns the current (post-sort) entry's payload columns.
func (s *vdbeSorter) data() []Value {
	return s.entries[s.pos].row
}

// next advances to the next sorted entry, reporting whether one exists --
// mirroring vdbeCursor.advance's/OpNext's "is there another row" convention.
func (s *vdbeSorter) next() bool {
	s.pos++
	return s.pos < len(s.entries)
}
