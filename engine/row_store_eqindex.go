package engine

import "slices"

// An equality index over a write session's row store.
// This is the sibling of segment_eqindex.go's posting list,
// for the other place a table's rows live.
//
// It was a full scan of the row store per lookup. The segment side of the same
// query measures 65.60us once its posting list is built, which is what said the
// structure was right and only the SITE was missing.
//
// WHY A MAP. A session's rows are a plain map with no index of any kind, so
// there is nothing to probe and something has to be built.
//
// WHAT IT REFUSES, and why each refusal is the safe direction. A seek only
// PRUNES -- the keyed conjunct is re-evaluated on every row handed back -- so a
// superset costs time and a subset loses rows. Therefore:
//
//   - a column holding any TEXT, BLOB or FLOAT value refuses the whole index,
//     rather than indexing the integers and quietly dropping the rest. A Float 5.0
//     must match "v = 5", and a collation must decide a text comparison; neither
//     belongs in an int64 map.
//   - a NULL is skipped rather than refusing, because NULL equals nothing: no
//     probe can match it, so leaving it out of every posting list loses nothing.
//   - a SHORT row -- one written before an ALTER TABLE ADD COLUMN, which reads as
//     the column's DEFAULT rather than NULL -- is added to EVERY posting list.
//     Excluding it would lose exactly the rows whose default happens to equal the
//     probe, and there are few enough of them that the cost is a rounding error.
//
// INVALIDATION IS INCREMENTAL, and it started out as a sledgehammer -- any write
// threw every column's posting list away and the next query rebuilt it. That is
// the obviously-safe design and it was MEASURED to be worse than no index at all
// for the workload that matters most:
//
//	warm query, index already built              34.6us
//	one-row UPDATE, then the same query      14,218.1us
//
// 411x, because the rebuild is O(rows) and the write was O(1). The scan that this
// index replaced took 11.67ms, so interleaved write-and-read -- the ordinary OLTP
// shape -- came out SLOWER than having no index. A cache that is thrown away more
// often than it is used is not a cache.
//
// So the three mutators maintain it instead: put moves a rowid from its old
// value's list to its new one, drop removes it, clear drops everything. The
// argument against doing that was "a finer-grained update needs the row's PREVIOUS
// value at every mutation site, and one missed site is a stale posting list, which
// is a lost row" -- and the answer is that there are exactly THREE such sites,
// they are all in row_store.go, and each one already has the old row in hand
// (put's own sortedValid probe reads it; drop deletes it). The choke point is the
// store's own mutators, which is where the sledgehammer lived too.
//
// WHEN IN DOUBT, DROP THE COLUMN'S INDEX rather than update it approximately: a
// value that is not an integer, or a row too short to reach the column, makes
// noteWrite abandon that column entirely and the next query rebuilds it (and then
// refuses it, if the new value really is untypable). Every uncertain case costs a
// rebuild, never a wrong answer.

// rsEqIndex is one column's posting list: value -> the rowids holding it.
type rsEqIndex struct {
	rows map[int64][]uint64
	// short is every rowid whose stored row does not reach this column. They
	// read as the column DEFAULT, so they are unioned into every answer.
	short []uint64
}

// eqRowids is "which rowids have column col = v", ascending, or false when this
// column cannot be indexed.
func (s *rowStore) eqRowids(col int, v int64) ([]uint64, bool) {
	if s == nil {
		return nil, false
	}
	idx, ok := s.eqIndexFor(col)
	if !ok {
		return nil, false
	}
	hits := idx.rows[v]
	if len(idx.short) == 0 {
		return hits, true
	}
	out := make([]uint64, 0, len(hits)+len(idx.short))
	out = append(out, hits...)
	out = append(out, idx.short...)
	return out, true
}

// eqIndexFor builds col's index on first use and caches it.
func (s *rowStore) eqIndexFor(col int) (*rsEqIndex, bool) {
	if s.eqIdx != nil {
		if idx, have := s.eqIdx[col]; have {
			return idx, idx != nil
		}
	}
	if s.eqIdx == nil {
		s.eqIdx = map[int]*rsEqIndex{}
	}
	idx := &rsEqIndex{rows: make(map[int64][]uint64, len(s.m))}
	for rid, vals := range s.all() {
		if col >= len(vals) {
			idx.short = append(idx.short, rid)
			continue
		}
		switch vals[col].Typ {
		case Int:
			idx.rows[vals[col].I] = append(idx.rows[vals[col].I], rid)
		case Null:
			// Equals nothing; no posting list can want it.
		default:
			// Remember the refusal so the column is not re-examined per query.
			s.eqIdx[col] = nil
			return nil, false
		}
	}
	// Ascending, which is the order a b-tree seek and a rowid-ordered scan both
	// produce and what a no-ORDER-BY result's neutrality relies on. Sorted once
	// per build rather than per probe.
	for v := range idx.rows {
		sortUint64s(idx.rows[v])
	}
	sortUint64s(idx.short)
	s.eqIdx[col] = idx
	return idx, true
}

// invalidateEqIndexes drops every column's posting list. Used where a write
// cannot be described row by row -- clear, and the fallbacks in noteWrite.
func (s *rowStore) invalidateEqIndexes() {
	if s.eqIdx != nil {
		s.eqIdx = nil
	}
}

// noteWrite maintains every built posting list across one row write: old is the
// row being replaced (nil for an insert) and neu the row now stored (nil for a
// delete).
//
// It runs only for columns an index has actually been BUILT for, so a store
// nobody has queried pays nothing, and a query that never asked about a column
// never makes its writes slower.
func (s *rowStore) noteWrite(rowid uint64, old, neu []Value) {
	if len(s.eqIdx) == 0 {
		return
	}
	for col, idx := range s.eqIdx {
		if idx == nil {
			continue // a column already refused; nothing to keep current
		}
		if old != nil {
			if !idx.removeRow(col, rowid, old) {
				s.eqIdx[col] = nil // could not be described exactly: rebuild later
				continue
			}
		}
		if neu != nil {
			if !idx.addRow(col, rowid, neu) {
				s.eqIdx[col] = nil
			}
		}
	}
}

// removeRow takes rowid out of the list its OLD value put it in. false means the
// row could not be placed exactly, and the caller must drop the whole column.
func (idx *rsEqIndex) removeRow(col int, rowid uint64, vals []Value) bool {
	if col >= len(vals) {
		return removeSorted(&idx.short, rowid)
	}
	switch vals[col].Typ {
	case Int:
		v := vals[col].I
		list := idx.rows[v]
		if !removeSorted(&list, rowid) {
			return false
		}
		if len(list) == 0 {
			delete(idx.rows, v)
		} else {
			idx.rows[v] = list
		}
		return true
	case Null:
		return true // it was in no list
	default:
		return false
	}
}

// addRow puts rowid into the list its NEW value belongs to, keeping the list
// ascending. false means the value is one this index cannot hold.
func (idx *rsEqIndex) addRow(col int, rowid uint64, vals []Value) bool {
	if col >= len(vals) {
		insertSorted(&idx.short, rowid)
		return true
	}
	switch vals[col].Typ {
	case Int:
		list := idx.rows[vals[col].I]
		insertSorted(&list, rowid)
		idx.rows[vals[col].I] = list
		return true
	case Null:
		return true // equals nothing, so it belongs in no list
	default:
		return false
	}
}

// removeSorted deletes rowid from an ascending slice. false when it is not there,
// which means the index and the store disagree and the column must be rebuilt.
func removeSorted(a *[]uint64, rowid uint64) bool {
	i, found := slices.BinarySearch(*a, rowid)
	if !found {
		return false
	}
	*a = append((*a)[:i], (*a)[i+1:]...)
	return true
}

// insertSorted adds rowid to an ascending slice, at most once.
func insertSorted(a *[]uint64, rowid uint64) {
	i, found := slices.BinarySearch(*a, rowid)
	if found {
		return
	}
	*a = append(*a, 0)
	copy((*a)[i+1:], (*a)[i:])
	(*a)[i] = rowid
}

// sortUint64s sorts ascending. Insertion order from a map range is arbitrary, so
// this is a real sort rather than a check.
func sortUint64s(a []uint64) { slices.Sort(a) }
