package engine

import (
	"cmp"
	"iter"
	"maps"
	"slices"
)

// rowStore is one table's rows, keyed by rowid. A nil *rowStore is a table
// whose rows have not been loaded. Row slices are never edited in place.
type rowStore struct {
	m map[uint64][]Value

	// seg is a table's committed rows from the segment file and delta,
	// never loaded whole. m holds rows written since; gone tracks dropped rowids.
	seg      *segRows
	gone     map[uint64]bool
	baseGone bool
	n        int
	nKnown   bool

	// max caches the largest rowid for nextRowidForTable. Cleared on drop
	// to match SQLite's rowid reuse behavior.
	max      uint64
	maxKnown bool

	// rangeMax caches the largest rowid in [rangeLo, rangeHi) for per-range
	// rowid allocation (replication's per-site ranges).
	rangeLo, rangeHi, rangeMax uint64
	rangeKnown, rangeHas       bool

	// sorted is m's keys in ascending signed-rowid order (b-tree order).
	// Never edited in place, only replaced. Shared with snapshots and cursors.
	sorted      []uint64
	sortedValid bool

	// over is a segment-backed store's overlay keys (m's keys) in ascending
	// signed order, merged with segments in ordered walks.
	over      []uint64
	overValid bool

	// eqIdx is the lazily built per-column equality index, nil until a seek asks
	// for one and thrown away whole by any mutation. See row_store_eqindex.go.
	eqIdx map[int]*rsEqIndex

	// uqIdx is each UNIQUE index's conflict-probe index, built on the first probe
	// and maintained by the same three mutators. See row_store_uniqindex.go.
	uqIdx map[*indexMeta]*rsUniqIndex

	// undo is the transaction's undo log for this store, recorded while logging
	// is on: what each put, drop and clear replaced, in order. A BEGIN or
	// SAVEPOINT holds an OFFSET into it rather than a copy of the rows, and a
	// rollback replays it backwards to that offset. See row_store_undo.go.
	undo    []rsUndo
	logging bool

	// spill holds the rows this store let go of, and memBytes counts the bytes of
	// rows it still holds in m -- see row_store_spill.go.
	spill    *spillFile
	memBytes int64
	unspilt  []uint64 // keys put since the last spill, which is all a spill visits
}

// newRowStore wraps rows a load read from disk: nil when rows is (a table not
// loaded), and sorted is the b-tree walk's own ascending order when the load
// vouched for it (nil otherwise, which leaves it to be rebuilt).
func newRowStore(rows map[uint64][]Value, sorted []uint64) *rowStore {
	if rows == nil {
		return nil
	}
	return &rowStore{m: rows, sorted: sorted, sortedValid: sorted != nil}
}

// get returns rowid's row.
func (s *rowStore) get(rowid uint64) ([]Value, bool) {
	if s == nil {
		return nil, false
	}
	if s.seg != nil {
		return s.segGet(rowid)
	}
	vals, ok := s.m[rowid]
	return s.resolve(vals), ok
}

// row returns rowid's row, or nil when there is none.
func (s *rowStore) row(rowid uint64) []Value {
	vals, _ := s.get(rowid)
	return vals
}

func (s *rowStore) len() int {
	if s == nil {
		return 0
	}
	if s.seg != nil {
		return s.segLen()
	}
	return len(s.m)
}

// all yields every row in no particular order. The body may put an existing
// rowid or drop any rowid as it goes, as a range over a map allows.
func (s *rowStore) all() iter.Seq2[uint64, []Value] {
	if s == nil {
		return func(func(uint64, []Value) bool) {}
	}
	if s.seg != nil {
		return s.segAll
	}
	if s.spill == nil {
		return maps.All(s.m)
	}
	return func(yield func(uint64, []Value) bool) {
		for rowid, vals := range s.m {
			if !yield(rowid, s.resolve(vals)) {
				return
			}
		}
	}
}

func (s *rowStore) put(rowid uint64, vals []Value) {
	// The OLD row, for the two things that need it: the sorted-order cache (only
	// while it is valid) and the equality indexes (only for columns one has been
	// built for). Both are conditional, so an unqueried store on a bulk load does
	// one map probe it already did.
	var old []Value
	haveOld := false
	if s.seg != nil {
		// The old row is the MERGED view's, and it is always needed: the
		// visible count moves only when the rowid was not already a row.
		old, haveOld = s.segGet(rowid)
		if !haveOld {
			s.sorted, s.sortedValid = nil, false
			if s.nKnown {
				s.n++
			}
		}
		delete(s.gone, rowid)
	} else if s.sortedValid || len(s.eqIdx) > 0 || len(s.uqIdx) > 0 || s.logging {
		old, haveOld = s.m[rowid]
		old = s.resolve(old)
		if s.sortedValid && !haveOld {
			s.sorted, s.sortedValid = nil, false
		}
	}
	if s.logging {
		s.undo = append(s.undo, rsUndo{rowid: rowid, old: old, had: haveOld})
	}
	n0 := len(s.m)
	s.m[rowid] = vals
	if len(s.m) != n0 {
		s.overValid = false
	}
	s.noteWritten(rowid, vals)
	if !haveOld {
		old = nil
	}
	if len(s.eqIdx) > 0 {
		s.noteWrite(rowid, old, vals)
	}
	if len(s.uqIdx) > 0 {
		s.noteUniqWrite(rowid, old, vals)
	}
	if s.maxKnown && rowidLess(s.max, rowid) {
		s.max = rowid
	}
	if s.rangeKnown && rowid >= s.rangeLo && rowid < s.rangeHi && (!s.rangeHas || rowid > s.rangeMax) {
		s.rangeMax, s.rangeHas = rowid, true
	}
}

// rewrite stores vals under rowid leaving the table's indexes alone, as DROP
// COLUMN's row rewrite does (OP_Insert on the table cursor only,
// alter.c:2356-2362): the dropped column is in no index.
func (s *rowStore) rewrite(rowid uint64, vals []Value) {
	s.put(rowid, vals)
}

// clear empties the store and reports how many rows it held -- the truncate
// optimisation's OP_Clear.
func (s *rowStore) clear() int64 {
	if s.seg != nil {
		n := int64(s.segLen())
		if s.logging {
			s.undo = append(s.undo, rsUndo{prior: s.m, priorGone: s.gone, priorBaseGone: s.baseGone, priorN: s.n, priorNKnown: s.nKnown, segClear: true})
		}
		s.m, s.gone, s.baseGone, s.n, s.nKnown = map[uint64][]Value{}, nil, true, 0, true
		s.max, s.maxKnown, s.sorted, s.sortedValid = 0, false, nil, false
		s.overValid, s.memBytes, s.unspilt = false, 0, nil
		s.rangeKnown = false
		s.invalidateEqIndexes()
		s.uqIdx = nil
		return n
	}
	n := int64(len(s.m))
	if s.logging {
		// The whole map is set aside, not copied: nothing writes it again.
		s.undo = append(s.undo, rsUndo{prior: s.m})
	}
	s.m = map[uint64][]Value{}
	s.max, s.maxKnown, s.sorted, s.sortedValid = 0, false, nil, false
	s.overValid, s.memBytes, s.unspilt = false, 0, nil
	s.rangeKnown = false
	s.invalidateEqIndexes()
	s.uqIdx = nil
	return n
}

func (s *rowStore) drop(rowid uint64) {
	// Unconditional, including for a rowid the store does not hold: clearing
	// is always safe, and gating it on a lookup would cost the delete's own
	// map probe twice over.
	s.sorted, s.sortedValid = nil, false
	s.overValid = false
	if s.seg != nil {
		old, have := s.segGet(rowid)
		if !have {
			return
		}
		if s.logging {
			s.undo = append(s.undo, rsUndo{rowid: rowid, old: old, had: true})
		}
		if len(s.eqIdx) > 0 || len(s.uqIdx) > 0 {
			s.noteWrite(rowid, old, nil)
			s.noteUniqWrite(rowid, old, nil)
		}
		delete(s.m, rowid)
		if !s.baseGone && s.seg.has(rowid) {
			if s.gone == nil {
				s.gone = map[uint64]bool{}
			}
			s.gone[rowid] = true
		}
		if s.nKnown {
			s.n--
		}
		if s.maxKnown && s.max == rowid {
			s.maxKnown = false
		}
		if s.rangeHas && s.rangeMax == rowid {
			s.rangeKnown = false
		}
		return
	}
	if s.logging {
		if old, have := s.m[rowid]; have {
			s.undo = append(s.undo, rsUndo{rowid: rowid, old: s.resolve(old), had: true})
		}
	}
	if len(s.eqIdx) > 0 || len(s.uqIdx) > 0 {
		if old, have := s.m[rowid]; have {
			old = s.resolve(old)
			s.noteWrite(rowid, old, nil)
			s.noteUniqWrite(rowid, old, nil)
		}
	}
	delete(s.m, rowid)
	if s.maxKnown && s.max == rowid {
		s.maxKnown = false
	}
	if s.rangeHas && s.rangeMax == rowid {
		s.rangeKnown = false
	}
}

// maxRowidIn returns the largest rowid in [lo, hi), and false when the store
// holds none there. Both bounds are positive, so the unsigned comparison is the
// signed one.
func (s *rowStore) maxRowidIn(lo, hi uint64) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	if s.rangeKnown && s.rangeLo == lo && s.rangeHi == hi {
		return s.rangeMax, s.rangeHas
	}
	var max uint64
	found := false
	for rowid := range s.all() {
		if rowid >= lo && rowid < hi && (!found || rowid > max) {
			max, found = rowid, true
		}
	}
	s.rangeLo, s.rangeHi, s.rangeMax, s.rangeHas, s.rangeKnown = lo, hi, max, found, true
	return max, found
}

// maxRowid returns the largest rowid, and false for an empty store.
func (s *rowStore) maxRowid() (uint64, bool) {
	if s == nil {
		return 0, false
	}
	if s.maxKnown {
		return s.max, true
	}
	if s.seg != nil {
		max, found := s.segMaxRowid()
		s.max, s.maxKnown = max, found
		return max, found
	}
	var max uint64
	found := false
	for rowid := range s.m {
		// rowidLess, not a bare >: a negative rowid is stored bit-for-bit in
		// the uint64 and would otherwise look like the largest.
		if !found || rowidLess(max, rowid) {
			max, found = rowid, true
		}
	}
	s.max, s.maxKnown = max, found
	return max, found
}

// sortedRowids returns every rowid in ascending signed order, memoized. The
// caller must not modify the slice.
func (s *rowStore) sortedRowids() []uint64 {
	if s == nil {
		return nil
	}
	// The length comparison is belt-and-braces on sortedValid: a future site
	// that changes the key set's size without clearing it gets a slower answer
	// rather than a wrong one.
	if s.seg != nil {
		if !s.sortedValid {
			s.sorted, s.sortedValid = s.segSortedRowids(), true
		}
		return s.sorted
	}
	if !s.sortedValid || len(s.sorted) != len(s.m) {
		s.sorted, s.sortedValid = sortedRowidsOf(s), true
	}
	return s.sorted
}

// eachSorted calls fn with every (rowid, row) in ascending signed-rowid order:
// a segment-backed store walks its segments in order, a map-backed one its
// memoized order -- never the rowid list followed by a lookup per row, which
// is a whole-table scan done as N random lookups. C's scan is the same shape,
// sqlite3BtreeFirst then sqlite3BtreeNext on ONE cursor (btree.c:5688/6384).
func (s *rowStore) eachSorted(fn func(rowid uint64, vals []Value)) {
	if s == nil {
		return
	}
	if s.seg != nil {
		s.segEachSorted(func(rowid uint64, vals []Value) bool { fn(rowid, vals); return true })
		return
	}
	for _, rowid := range s.sortedRowids() {
		fn(rowid, s.resolve(s.m[rowid]))
	}
}

// eachSortedUntil is eachSorted for a caller that may stop: yield returning
// false ends the walk there, so a LIMIT over a table reads no further.
func (s *rowStore) eachSortedUntil(yield func(rowid uint64, vals []Value) bool) {
	switch {
	case s == nil:
	case s.seg != nil:
		s.segEachSorted(yield)
	default:
		for _, rowid := range s.sortedRowids() {
			if !yield(rowid, s.resolve(s.m[rowid])) {
				return
			}
		}
	}
}

// clone is a snapshot of s for the table owner: a new map over the SAME row
// slices. That is sound because a stored row is never edited in place -- put
// replaces it -- which is this type's contract, and the one place that broke it
// (a read's fix-ups, vdbeCursor.normalizeFrom) now copies. It used to deep-copy
// every row and every TEXT/BLOB byte, "defensively": 31% of a 20,000-row
// insert transaction's allocated bytes over a 100,000-row table, paid by the
// BEGIN before a single row was written. C's BEGIN copies nothing; its
// rollback journal takes a page only when the page is first written.
//
// Rows that came from a segment alias its mapping, so a snapshot sharing them
// does too. That is safe because the mapping is replaced only by a commit
// (rewriteFile), and every commit ends the SQL transaction -- and with it every
// snapshot -- first.
func (s *rowStore) clone(owner *tableMeta) *rowStore {
	if s == nil {
		return nil
	}
	cp := *s
	// The posting lists are NOT shared: noteWrite edits them in place, so a
	// snapshot holding the live store's lists would be restored by ROLLBACK TO
	// with rows from before the savepoint and an index from after it -- a seek
	// then lost the row a rolled-back UPDATE had moved. C's index b-trees roll
	// back with the table's pages (pagerPlaybackSavepoint, pager.c:3452); here
	// the snapshot rebuilds its own on first use.
	cp.eqIdx = nil
	cp.uqIdx = nil // the same, for the unique-index probe (row_store_uniqindex.go)
	// A copy starts with no undo log of its own: sharing the backing array would
	// let its appends overwrite this store's entries.
	cp.undo, cp.logging = nil, false
	cp.m = maps.Clone(s.m)
	cp.gone = maps.Clone(s.gone) // the overlay is the copy; the base is shared and read-only
	return &cp
}

// sortedRowidsOf returns s's key set in ascending SIGNED-rowid order, by
// ranging the map and sorting: rowStore.sorted's rebuild path, and the oracle
// that cache is tested against.
func sortedRowidsOf(s *rowStore) []uint64 {
	out := make([]uint64, 0, s.len())
	for rid := range s.all() {
		out = append(out, rid)
	}
	radixSortRowids(out)
	return out
}

// radixSortRowids sorts rowids in place in ascending SIGNED order -- rowidLess's
// order -- by an LSD radix sort on the sign-flipped key, skipping any byte every
// key shares (radixOrderInt64's trick, segment_index_ondisk.go). Measured over
// 120k rowids, the comparison sorts it replaced took 11.0ms (slices.Sort over
// int64) and 16.6ms (slices.SortFunc over uint64); rowids are dense, so most
// passes are skipped.
func radixSortRowids(keys []uint64) {
	n := len(keys)
	if n < 256 {
		slices.SortFunc(keys, func(a, b uint64) int { return cmp.Compare(int64(a), int64(b)) })
		return
	}
	src, dst := keys, make([]uint64, n)
	for shift := uint(0); shift < 64; shift += 8 {
		var at [256]int
		for _, k := range src {
			at[(k^1<<63)>>shift&0xff]++
		}
		if at[(src[0]^1<<63)>>shift&0xff] == n {
			continue
		}
		sum := 0
		for b, c := range at {
			at[b] = sum
			sum += c
		}
		for _, k := range src {
			b := (k ^ 1<<63) >> shift & 0xff
			dst[at[b]] = k
			at[b]++
		}
		src, dst = dst, src
	}
	if &src[0] != &keys[0] {
		copy(keys, src)
	}
}

// rowidLess reports whether a orders before b as a table b-tree KEY -- a
// rowid, i.e. a SIGNED 64-bit SQLite INTEGER stored bit-for-bit in a uint64.
// A plain a < b (unsigned) is wrong for any negative rowid: rowid -1, stored
// as 0xFFFFFFFFFFFFFFFF, would compare far GREATER than rowid 5, where
// SQLite's own signed rowid ordering (and integrity_check's "Rowid N out of
// order") requires -1 to sort BEFORE 5. Every place that orders or searches
// by table-b-tree key uses this rather than a bare comparison on the raw
// uint64, or a negative-rowid table produces a tree C SQLite's
// integrity_check rejects (which compat-harness's write fuzzer caught).
func rowidLess(a, b uint64) bool { return int64(a) < int64(b) }

// fullStoredRow widens a record read from tbl's b-tree to the row store's
// shape -- see readTableRowsFromPager, which does the same for a whole table.
func fullStoredRow(tbl *tableMeta, rowid uint64, row []Value) ([]Value, error) {
	return fullStoredRowOf(tbl.name, tbl.cols, tbl.ipkIndex, rowid, row)
}

// fullStoredRowOf is fullStoredRow for a table whose metadata is not assembled
// yet (a schema load).
func fullStoredRowOf(name string, cols []columnInfo, ipkIndex int, rowid uint64, row []Value) ([]Value, error) {
	tbl := &tableMeta{name: name, cols: cols, ipkIndex: ipkIndex}
	gen := hasGeneratedCols(tbl.cols)
	if gen {
		row = expandStoredRow(tbl.cols, row)
	}
	// Padding comes first: a record written before an ALTER TABLE ADD COLUMN is
	// short of the column list, and a generated expression is computed over the
	// whole row.
	row = padStoredRow(tbl.cols, row)
	if gen {
		if tbl.ipkIndex >= 0 && row[tbl.ipkIndex].Typ == Null {
			row[tbl.ipkIndex] = Value{Typ: Int, I: int64(rowid)}
			if err := computeGeneratedInto(tbl.name, tbl.cols, row); err != nil {
				return nil, err
			}
			row[tbl.ipkIndex] = Value{Typ: Null}
		} else if err := computeGeneratedInto(tbl.name, tbl.cols, row); err != nil {
			return nil, err
		}
	}
	return row, nil
}
