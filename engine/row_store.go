package engine

import (
	"cmp"
	"iter"
	"maps"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

// rowStore is one table's rows, keyed by rowid. A nil *rowStore is a table
// whose rows have not been loaded. Row slices are never edited in place.
type rowStore struct {
	m map[uint64][]Value
	// mPeak is the most rows m has held since it was made, or the room
	// reserve made it with: a map never gives back its room, so a reserve
	// that fits under it need not rebuild m. Zero whenever m is replaced.
	mPeak int

	// seg is a table's committed rows from the segment file and delta,
	// never loaded whole. m holds rows written since; gone tracks dropped rowids.
	seg  *segRows
	gone map[uint64]bool
	// goneBits is gone as bits (rowidBits), for isGone: kept beside a gone
	// map this store started, nil once gone is replaced whole.
	goneBits *rowidBits
	baseGone bool
	n        int
	nKnown   bool

	// swaps counts replacements of m as a whole (clear, and an undo that
	// restores a prior map): the one way a row leaves m without a gone mark.
	// rowSeen relies on it.
	swaps uint32
	// writes counts every put, drop and swap: while it has not moved, no row
	// has entered or left m or the dropped set. rowSeen relies on it.
	writes uint64
	// reshapes counts the writes that were not appends: a put past every
	// rowid the sorted overlay holds while it is valid (put) does not move
	// it. While it stands still, the overlay has only grown at its end, which
	// is what lets the join kernel extend its reading of the store
	// (segJoinTableOf) rather than read it again.
	reshapes uint64

	// max caches the largest rowid for nextRowidForTable. Cleared on drop
	// to match SQLite's rowid reuse behavior.
	max      uint64
	maxKnown bool

	// ub is an UPPER BOUND on every rowid the store holds, while ubKnown:
	// raised by every put, left alone by a drop (still a bound), and dropped
	// only where rows can come back without a put (clear, an undo that
	// restores a whole map). has() answers "past ub" without a lookup --
	// an INSERT's rowid-conflict probe for rows appended in order, a map
	// lookup per row that otherwise missed every time.
	ub      uint64
	ubKnown bool

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

	// joinTable is the join kernel's reading of this store's rows
	// (segJoinTableOf), stamped with the write counters it was read at.
	// segJoinCacheMu guards it.
	joinTable *segJoinTable

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
	// undoSealed is the log's length when a snapshot last took an offset into
	// it (startUndo): an entry before it is one a rollback may stop short of, so
	// a later put never extends it into a run.
	undoSealed int

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

// eachOf calls fn with the row of each of rowids the store holds, in the
// order given: get for each, with a segment-backed store's committed rows read
// as one run (segRun). base is whether the row came from those committed rows,
// decoded for this call so that nothing else holds it, rather than from the
// overlay or a read others share: for rowSeen, which a false base only costs
// its shortcut, and for an UPDATE that stores its new row into it.
//
// notGone says the caller already knows no rowid the overlay does not hold
// was deleted (segEqRowids filtered them), so that is not looked up again.
func (s *rowStore) eachOf(rowids []uint64, notGone bool, fn func(rid uint64, vals []Value, base bool)) {
	if s == nil {
		return
	}
	if s.seg == nil {
		for _, rid := range rowids {
			if vals, ok := s.get(rid); ok {
				fn(rid, vals, false)
			}
		}
		return
	}
	run := s.seg.run()
	if run.ok && len(rowids) >= eachOfParallelMin && runtime.GOMAXPROCS(0) > 1 && !hasGeneratedCols(s.seg.tbl.cols) {
		s.eachOfParallel(rowids, notGone, fn)
		return
	}
	for _, rid := range rowids {
		if vals, ok := s.m[rid]; ok {
			fn(rid, s.resolve(vals), false)
			continue
		}
		if !notGone && (s.baseGone || s.isGone(rid)) {
			continue
		}
		if vals, ok, owned := run.get(rid); ok {
			fn(rid, vals, owned)
		}
	}
}

// eachOfParallelMin is how many rowids make eachOf decode the committed rows
// on several cores: decoding is most of what reading thousands of them costs
// (a bulk UPDATE's seek), and below this the goroutines cost more than they
// save.
const eachOfParallelMin = 4096

// eachOfParallel is eachOf with the committed rows decoded by several
// goroutines first, each through a run of its own over the pieces it takes --
// a run's arena is its own, so every row is still decoded for this call alone
// -- and fn then called for every row in the order given, on this goroutine.
// The decoding only reads: segments, the delta, the table's columns. A table
// with generated columns, which decoding computes, is left to eachOf.
func (s *rowStore) eachOfParallel(rowids []uint64, notGone bool, fn func(rid uint64, vals []Value, base bool)) {
	// Which rows are the overlay's (kept, so they are not looked up twice),
	// which are gone, which are to be read.
	base := make([]uint64, 0, len(rowids))
	var over map[uint64][]Value
	for _, rid := range rowids {
		if vals, in := s.m[rid]; in {
			if over == nil {
				over = map[uint64][]Value{}
			}
			over[rid] = vals
		} else if notGone || !s.baseGone && !s.isGone(rid) {
			base = append(base, rid)
		}
	}
	out := make([][]Value, len(base))
	state := make([]uint8, len(base)) // 0 no row, 1 a row, 2 a row decoded for this call
	// In pieces taken from a counter, by this goroutine as well as its
	// helpers: a helper the scheduler starts late (every core busy) then
	// takes fewer pieces rather than holding up the rest.
	const piece = 512
	var next atomic.Int64
	decode := func() {
		run := s.seg.run()
		for {
			lo := int(next.Add(piece)) - piece
			if lo >= len(base) {
				return
			}
			for j := lo; j < min(lo+piece, len(base)); j++ {
				if vals, ok, owned := run.get(base[j]); ok {
					out[j], state[j] = vals, 1
					if owned {
						state[j] = 2
					}
				}
			}
		}
	}
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0)-1, len(base)/piece-1, 7) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decode()
		}()
	}
	decode()
	wg.Wait()
	b := 0
	for _, rid := range rowids {
		if vals, in := over[rid]; in {
			fn(rid, s.resolve(vals), false)
			continue
		}
		if b < len(base) && base[b] == rid {
			if state[b] != 0 {
				fn(rid, out[b], state[b] == 2)
			}
			b++
		}
	}
}

// rowSeen is row(rowid) for a caller already holding seen, the row this store
// handed out for rowid when m had been swapped swaps times (a write cursor's
// own row): seen itself when the store provably still holds that row, so it is
// not decoded from its segment a second time. origin says where seen came
// from: seenBase the committed rows, seenOverlay m, seenUnknown either.
//
// Provably: seen was the base row, and rowid is still in neither m nor the
// dropped set, and m has not been replaced since -- the base does not change
// under a session. Where seen came from unknown, it was the base row exactly
// when the base has rowid: a row leaves m only by a drop, which marks a base
// rowid gone, or by a swap. Anything else reads the store as row() does.
//
// writes is the store's write count when seen was known to be the base row
// still: if nothing has written the store since, a base row is still the
// store's row with no lookup at all.
func (s *rowStore) rowSeen(rowid uint64, seen []Value, swaps uint32, writes uint64, origin seenOrigin) []Value {
	if seen != nil && origin == seenBase && s.writes == writes && s.swaps == swaps && s.seg != nil {
		return seen
	}
	if seen == nil || origin == seenOverlay || s.seg == nil || s.swaps != swaps || s.baseGone || s.isGone(rowid) {
		return s.row(rowid)
	}
	if _, over := s.m[rowid]; over || origin == seenUnknown && !s.seg.has(rowid) {
		return s.row(rowid)
	}
	return seen
}

// seenOrigin is where a row handed to rowSeen came from.
type seenOrigin uint8

const (
	seenUnknown seenOrigin = iota
	seenBase
	seenOverlay
)

// inPlaceOK reports whether the store may take an UPDATE's new row written over
// the old one (applyGathered): it keeps no copy of an old row -- no undo log,
// no equality or conflict index -- that would then show the new values.
func (s *rowStore) inPlaceOK() bool {
	return s != nil && s.seg != nil && !s.logging && len(s.eqIdx) == 0 && len(s.uqIdx) == 0
}

// committedRow is rowid's committed row, as the store's committed rows (not
// its overlay) hold it, or nil: what an update that wrote over its only copy of
// the old row puts back on a rollback (undoBase).
func (s *rowStore) committedRow(rowid uint64) []Value {
	if s == nil || s.seg == nil {
		return nil
	}
	vals, _ := s.seg.get(rowid)
	return vals
}

// isGone is gone[rid], answered from goneBits when they rule it out.
func (s *rowStore) isGone(rid uint64) bool {
	if len(s.gone) == 0 || !s.goneBits.has(int64(rid)) {
		return false
	}
	return s.gone[rid]
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
	s.putHeld(rowid, vals, nil)
}

// putOver is put for a caller holding held, the row the store holds for
// rowid right now (an UPDATE's own old row): it is the old row, so put does
// not look it up again.
func (s *rowStore) putOver(rowid uint64, vals, held []Value) {
	s.putHeld(rowid, vals, held)
}

func (s *rowStore) putHeld(rowid uint64, vals, held []Value) {
	// The OLD row, for the two things that need it: the sorted-order cache (only
	// while it is valid) and the equality indexes (only for columns one has been
	// built for). Both are conditional, so an unqueried store on a bulk load does
	// one map probe it already did.
	var old []Value
	haveOld := false
	if s.seg != nil {
		// The MERGED view's old row, or only whether there is one: the
		// visible count moves only when the rowid was not already a row, and
		// the row itself is wanted only by an index or the undo log kept
		// here. Reading it back regardless was a second fetch of every row an
		// UPDATE writes, the first being the cursor's own.
		switch {
		case held != nil:
			old, haveOld = held, true
		case s.ubKnown && rowidLess(s.ub, rowid):
			// Past the upper bound nothing is there (see has): a row appended
			// after the last one is new without a search of the segments, which
			// was half of what storing it cost.
		case len(s.eqIdx) > 0 || len(s.uqIdx) > 0 || s.logging:
			old, haveOld = s.segGet(rowid)
		default:
			haveOld = s.segHas(rowid)
		}
		if !haveOld {
			s.sorted, s.sortedValid = nil, false
			if s.nKnown {
				s.n++
			}
		}
		if held == nil {
			// A held row is a row the store holds, and a dropped rowid holds
			// none: only an unheld rowid can be in the dropped set.
			delete(s.gone, rowid)
			s.goneBits.del(int64(rowid))
		}
	} else if held != nil {
		old, haveOld = held, true
	} else if s.sortedValid || len(s.eqIdx) > 0 || len(s.uqIdx) > 0 || s.logging {
		old, haveOld = s.m[rowid]
		old = s.resolve(old)
		if s.sortedValid && !haveOld {
			s.sorted, s.sortedValid = nil, false
		}
	}
	if s.logging {
		if l := len(s.undo) - 1; !haveOld && l >= s.undoSealed && l >= 0 && s.undo[l].clr == nil && !s.undo[l].had &&
			s.undo[l].run < math.MaxUint32 && s.undo[l].rowid+uint64(s.undo[l].run)+1 == rowid {
			s.undo[l].run++ // the next new rowid after the last entry's run
		} else {
			s.undo = append(s.undo, rsUndo{rowid: rowid, old: old, had: haveOld})
		}
	}
	n0 := len(s.m)
	s.writes++
	s.m[rowid] = vals
	if len(s.m) != n0 {
		// A rowid past every one the overlay holds -- an append, the common
		// insert -- keeps it sorted; anything else sorts it again.
		if s.overValid && len(s.over) == n0 && (n0 == 0 || rowidLess(s.over[n0-1], rowid)) {
			s.over = append(s.over, rowid)
		} else {
			s.overValid = false
			s.reshapes++
		}
		s.mPeak = max(s.mPeak, len(s.m))
	} else {
		s.reshapes++
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
	if s.ubKnown && rowidLess(s.ub, rowid) {
		s.ub = rowid
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
			s.undo = append(s.undo, rsUndo{clr: &rsClear{prior: s.m, priorGone: s.gone, priorBaseGone: s.baseGone, priorN: s.n, priorNKnown: s.nKnown, segClear: true}})
		}
		s.m, s.gone, s.goneBits, s.baseGone, s.n, s.nKnown = map[uint64][]Value{}, nil, nil, true, 0, true
		s.mPeak = 0
		s.swaps++
		s.writes++
		s.reshapes++
		s.max, s.maxKnown, s.sorted, s.sortedValid = 0, false, nil, false
		s.ubKnown = false
		s.overValid, s.memBytes, s.unspilt = false, 0, nil
		s.rangeKnown = false
		s.invalidateEqIndexes()
		s.uqIdx = nil
		return n
	}
	n := int64(len(s.m))
	if s.logging {
		// The whole map is set aside, not copied: nothing writes it again.
		s.undo = append(s.undo, rsUndo{clr: &rsClear{prior: s.m}})
	}
	s.m, s.mPeak = map[uint64][]Value{}, 0
	s.swaps++
	s.writes++
	s.reshapes++
	s.max, s.maxKnown, s.sorted, s.sortedValid = 0, false, nil, false
	s.ubKnown = false
	s.overValid, s.memBytes, s.unspilt = false, 0, nil
	s.rangeKnown = false
	s.invalidateEqIndexes()
	s.uqIdx = nil
	return n
}

func (s *rowStore) drop(rowid uint64) {
	s.writes++
	s.reshapes++
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
				s.gone, s.goneBits = map[uint64]bool{}, &rowidBits{}
			}
			s.gone[rowid] = true
			if s.goneBits != nil {
				s.goneBits.add(int64(rowid))
			}
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
		var rd spillReader
		for _, rowid := range s.sortedRowids() {
			if !yield(rowid, rd.resolve(s, s.m[rowid])) {
				return
			}
		}
	}
}

// eachSortedRaw is eachSortedUntil handing out rows as m holds them: a
// spilled row as its sentinel, for a caller that can use one as it is
// (segment_ddl.go's spillRun).
func (s *rowStore) eachSortedRaw(yield func(rowid uint64, vals []Value) bool) {
	switch {
	case s == nil:
	case s.seg != nil:
		s.segEachSortedRaw(yield)
	default:
		for _, rowid := range s.sortedRowids() {
			if !yield(rowid, s.m[rowid]) {
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
	cp.mPeak = len(cp.m)
	cp.gone = maps.Clone(s.gone) // the overlay is the copy; the base is shared and read-only
	cp.goneBits = nil            // this store's own; the copy reads its map
	// The sorted overlay is appended to in place (put), so the copy keeps none
	// of its backing array.
	cp.over, cp.overValid = nil, false
	return &cp
}

// sortedRowidsOf returns s's key set in ascending SIGNED-rowid order, by
// ranging the map and sorting: rowStore.sorted's rebuild path, and the oracle
// that cache is tested against.
func sortedRowidsOf(s *rowStore) []uint64 {
	out := make([]uint64, 0, s.len())
	// The keys alone: a walk through all() would read every spilled row back
	// to throw it away.
	for rid := range s.m {
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

// reserve makes room in the overlay for n rows about to be put, so a
// post-commit settle of n appended rows (insert_append_delta.go) or a bulk
// UPDATE does not grow the map a step at a time. An overlay holding a few rows
// already is rebuilt at the size, which costs a copy of those few.
func (s *rowStore) reserve(n int) {
	switch {
	case s == nil || n <= 0 || s.mPeak >= len(s.m)+n:
	case len(s.m) == 0:
		s.m, s.mPeak = make(map[uint64][]Value, n), n
	case n >= 1024 && len(s.m) < n/4:
		m := make(map[uint64][]Value, len(s.m)+n)
		maps.Copy(m, s.m)
		s.m, s.mPeak = m, len(s.m)+n
	}
}

// isEmpty reports whether the store holds no row, without counting them: a
// segment-backed store's first count walks every base rowid, and a caller
// that only asks "any?" stops at the first.
func (s *rowStore) isEmpty() bool {
	if s == nil {
		return true
	}
	if s.seg == nil || s.nKnown {
		return s.len() == 0
	}
	// The largest rowid is kept current by put and drop once known, and found
	// from the top of the base without decoding a row.
	_, any := s.maxRowid()
	return !any
}
