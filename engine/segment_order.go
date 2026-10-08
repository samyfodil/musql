package engine

import (
	"bytes"
	"math"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
)

// ORDER BY ... LIMIT answered from the columnar segments.
//
// A segment stores keys as contiguous int64 blocks, allowing a pass over an
// array with no record decode and no Value materialization. The heap holds
// only `limit` entries.
//
// This path is narrow on purpose. Every key and output column must be a clean
// fixed-width int64 block with no NULL or exception, which makes cross-storage-class
// ordering, NULLS placement, and collation irrelevant when every value is an
// integer. Anything else declines to the peephole's fallback loop.

// segOrderPlan is what OpSegOrderLimit carries: which columns form the sort
// key, which direction each sorts, which columns the row emits, and how many
// rows to keep.
type segOrderPlan struct {
	keyCols []int
	keyDesc []bool
	keyColl []string // per key: the declared collation, "" for the default
	outCols []int
	limit   int // rows the heap keeps: the LIMIT plus the OFFSET
	offset  int // leading rows of those the statement skips
	nOut    int
}

// segOrderEntry is one candidate row: its key values, its output values, and
// its position in scan order.
//
// seq is the tie-break and it is not optional. vdbeSorter.entryBefore breaks a
// full key tie by INSERTION order, so two rows with equal keys must come out in
// scan order -- which for a table scan is rowid order. Dropping it would emit a
// different (also valid, but different) pair of tied rows than the loop does,
// and the corpus compares against C SQLite exactly.
type segOrderEntry struct {
	key []int64
	out []int64
	seq int
}

// segOrderBefore is vdbeSorter.entryBefore over integer keys: each key column
// in turn under its own direction, then insertion order.
func segOrderBefore(p *segOrderPlan, a, b segOrderEntry) bool {
	for i := range a.key {
		if a.key[i] == b.key[i] {
			continue
		}
		if p.keyDesc[i] {
			return a.key[i] > b.key[i]
		}
		return a.key[i] < b.key[i]
	}
	return a.seq < b.seq
}

// segOrderLimitTable answers the whole statement, or reports false to decline.
//
// The heap holds the `limit` entries that sort EARLIEST, with its root being
// the latest of those -- the next to be evicted. That is the mirror of
// vdbeSorter.insert's bounded path, and it is why the comparison at the root is
// "does this candidate sort before the worst one we are keeping".
// ipkCol is the table's INTEGER PRIMARY KEY column, or -1. It needs its own
// case because a rowid ALIAS is not stored in the record at all -- SQLite writes
// NULL there and substitutes the rowid on read, and the converter preserves that
// exactly (it must, or a round trip would not be byte-identical). So the column
// block for `id INTEGER PRIMARY KEY` is all NULLs and Int64Column refuses it,
// while the values the query wants sit in the segment's rowid block.
//
// Without this the very shape this path exists for -- "ORDER BY v DESC, id DESC"
// on a table with an INTEGER PRIMARY KEY, which is most tables -- declined every
// time, and did so silently because declining is the safe direction.
func (p *ReadOnlyPager) segOrderLimitTable(rootPage uint32, plan *segOrderPlan, ipkCol int) ([][]Value, bool) {
	// Gated on the log's size, not on whether a session holds the rows.
	// The log is reconciled into the merge; when it is large relative to the
	// file, the cost of merging dominates.
	if p != nil && p.segs != nil {
		if segs, have := p.segs.byRoot[rootPage]; !have || !p.segs.deltaIsSmallFor(rootPage, segs) {
			return nil, false
		}
	}
	if p == nil || p.segs == nil || plan == nil || plan.limit <= 0 {
		return nil, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok {
		return nil, false
	}
	// The log is MERGED rather than declined for: the positions it invalidated are
	// skipped as the blocks are walked, and its own live rows are offered to the
	// same heap afterwards. Same shape as the count and group paths -- see
	// segment_delta_merge.go for why the skips are POSITIONS and not a map probe
	// per row.
	skips, deltaLive, mergeable := p.segs.segDeltaSkips(rootPage, segs)
	if !mergeable {
		return nil, false
	}
	// A CLEAN table's one-key ORDER BY takes segOrderLimitBytes, which reads the
	// outputs for survivors only; a table with a log to merge stays here.
	noSkips := true
	for _, sk := range skips {
		noSkips = noSkips && len(sk) == 0
	}
	if noSkips && len(deltaLive) == 0 && p.segCleanFor(rootPage) {
		if rows, ok := segOrderLimitBytes(plan, nil, segs, ipkCol); ok {
			return rows, true
		}
	}
	heap := make([]segOrderEntry, 0, plan.limit)
	scratchKey := make([]int64, len(plan.keyCols))
	seq := 0
	for si, s := range segs {
		skip, sk := skips[si], 0
		keyCols, keyIsRowid, okKeys := segOrderColumns(s, plan.keyCols, ipkCol)
		if !okKeys {
			return nil, false
		}
		outCols, outIsRowid, okOuts := segOrderColumns(s, plan.outCols, ipkCol)
		if !okOuts {
			return nil, false
		}
		// Once the heap is full its root is the cutoff: a row whose FIRST key
		// sorts strictly after the root's cannot enter, whatever the later
		// keys say. That is tested on the bare value before anything is built,
		// and the zone maps test it for a whole run of rows at once.
		var first []int64
		var zones *segZones
		if !keyIsRowid[0] {
			first = keyCols[0]
			zones, _ = s.intZones(plan.keyCols[0])
		}
		desc := plan.keyDesc[0]
		after := func(v, cut int64) bool { // v sorts strictly after cut
			if desc {
				return v < cut
			}
			return v > cut
		}
		for r := 0; r < s.nRows; r++ {
			if first != nil && len(heap) == plan.limit {
				cut := heap[0].key[0]
				if zones != nil && r%segFilterBatch == 0 {
					b := r / segFilterBatch
					best := zones.zmax[b]
					if !desc {
						best = zones.zmin[b]
					}
					if after(best, cut) {
						end := min(r+segFilterBatch, s.nRows)
						seq += end - r - segSkipsIn(skip, &sk, r, end)
						r = end - 1
						continue
					}
				}
				if r < len(first) && after(first[r], cut) {
					for sk < len(skip) && skip[sk] < r {
						sk++
					}
					if !(sk < len(skip) && skip[sk] == r) {
						seq++
					}
					continue
				}
			}
			// A row the log superseded or removed is not this block's to offer.
			for sk < len(skip) && skip[sk] < r {
				sk++
			}
			if sk < len(skip) && skip[sk] == r {
				sk++
				continue
			}
			cand := segOrderEntry{seq: seq, key: scratchKey}
			seq++
			// Build the key first, in a reused buffer, and test it before
			// touching the output columns, which is loses()'s saving: a row
			// that cannot make the cut costs one comparison and no allocation.
			for i := range keyCols {
				if keyIsRowid[i] {
					cand.key[i] = int64(s.Rowid(r))
					continue
				}
				if r >= len(keyCols[i]) {
					return nil, false
				}
				cand.key[i] = keyCols[i][r]
			}
			if len(heap) == plan.limit && !segOrderBefore(plan, cand, heap[0]) {
				continue
			}
			// A full heap evicts its root: its slices become this entry's.
			if len(heap) == plan.limit {
				cand.key, cand.out = heap[0].key, heap[0].out
				copy(cand.key, scratchKey)
			} else {
				cand.key = append([]int64(nil), scratchKey...)
				cand.out = make([]int64, len(outCols))
			}
			for i := range outCols {
				if outIsRowid[i] {
					cand.out[i] = int64(s.Rowid(r))
					continue
				}
				if r >= len(outCols[i]) {
					return nil, false
				}
				cand.out[i] = outCols[i][r]
			}
			if len(heap) < plan.limit {
				heap = append(heap, cand)
				segOrderSiftUp(plan, heap, len(heap)-1)
				continue
			}
			heap[0] = cand
			segOrderSiftDown(plan, heap, 0)
		}
	}
	// ...AND THE LOG'S OWN LIVE ROWS, offered to the same heap. They are
	// row-major, so their values come out of the row rather than a block, and a
	// value the plan cannot represent as an int64 declines the WHOLE statement
	// rather than leaving a row out -- a top-N that silently skipped a candidate
	// would return the wrong twenty.
	if len(deltaLive) > 0 {
		rids := make([]int64, 0, len(deltaLive))
		for rid := range deltaLive {
			rids = append(rids, rid)
		}
		slices.Sort(rids)
		for _, rid := range rids {
			vals := deltaLive[rid]
			cand := segOrderEntry{seq: seq}
			seq++
			var bad bool
			cand.key, bad = segOrderRowCells(vals, plan.keyCols, ipkCol, rid)
			if bad {
				return nil, false
			}
			if len(heap) == plan.limit && !segOrderBefore(plan, cand, heap[0]) {
				continue
			}
			cand.out, bad = segOrderRowCells(vals, plan.outCols, ipkCol, rid)
			if bad {
				return nil, false
			}
			if len(heap) < plan.limit {
				heap = append(heap, cand)
				segOrderSiftUp(plan, heap, len(heap)-1)
				continue
			}
			heap[0] = cand
			segOrderSiftDown(plan, heap, 0)
		}
	}
	sort.SliceStable(heap, func(i, j int) bool { return segOrderBefore(plan, heap[i], heap[j]) })
	rows := make([][]Value, len(heap))
	for i, e := range heap {
		row := make([]Value, len(e.out))
		for j, v := range e.out {
			row[j] = Value{Typ: Int, I: v}
		}
		rows[i] = row
	}
	return rows, true
}

// segCleanInt64Column is the one condition everything here rests on: a
// fixed-width int64 block with no NULL and no exception.
func segCleanInt64Column(s *segment, col int) ([]int64, bool) {
	if kind, _ := s.scanKind(col); kind != segScanSlice {
		return nil, false
	}
	return s.Int64Column(col)
}

// A max-heap under segOrderBefore: the root is the entry that sorts LAST among
// those kept, so it is the one an incoming better row replaces.
func segOrderSiftUp(p *segOrderPlan, h []segOrderEntry, i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !segOrderBefore(p, h[parent], h[i]) {
			return
		}
		h[parent], h[i] = h[i], h[parent]
		i = parent
	}
}

func segOrderSiftDown(p *segOrderPlan, h []segOrderEntry, i int) {
	for {
		l, r, worst := 2*i+1, 2*i+2, i
		if l < len(h) && segOrderBefore(p, h[worst], h[l]) {
			worst = l
		}
		if r < len(h) && segOrderBefore(p, h[worst], h[r]) {
			worst = r
		}
		if worst == i {
			return
		}
		h[i], h[worst] = h[worst], h[i]
		i = worst
	}
}

// segOrderColumns resolves a list of table column indexes to int64 blocks,
// marking the one that is the rowid alias -- whose values come from the rowid
// block rather than from a column block that holds only NULLs.
// segProgColumns is segOrderColumns for a COMPILED PROGRAM, which may want a
// column as its NULL INDICATOR rather than its value (segment_nullidx.go).
//
// An indicator slot is 0/1 per row and is always available -- it is derived
// from the bitmap, not from the block -- so a TEXT, BLOB or NULL-bearing column
// that segCleanInt64Column refuses as a VALUE is still readable here as a
// null-ness test. That is the whole reason "WHERE payload IS NULL" can compile.
func segProgColumns(s *segment, want []int, asNull []bool, ipkCol int) (blocks [][]int64, isRowid []bool, ok bool) {
	blocks = make([][]int64, len(want))
	isRowid = make([]bool, len(want))
	for i, c := range want {
		if i < len(asNull) && asNull[i] {
			idx, okIdx := s.nullIndicator(c)
			if !okIdx {
				return nil, nil, false
			}
			blocks[i] = idx
			continue
		}
		if c == ipkCol && ipkCol >= 0 {
			isRowid[i] = true
			continue
		}
		col, okCol := segCleanInt64Column(s, c)
		if !okCol {
			return nil, nil, false
		}
		blocks[i] = col
	}
	return blocks, isRowid, true
}

func segOrderColumns(s *segment, want []int, ipkCol int) (blocks [][]int64, isRowid []bool, ok bool) {
	blocks = make([][]int64, len(want))
	isRowid = make([]bool, len(want))
	for i, c := range want {
		if c == ipkCol && ipkCol >= 0 {
			isRowid[i] = true
			continue
		}
		col, okCol := segCleanInt64Column(s, c)
		if !okCol {
			return nil, nil, false
		}
		blocks[i] = col
	}
	return blocks, isRowid, true
}

// ---- The VALUE path, for a key or output column that is not a clean int64 ----
//
// The int64 path above is narrow on purpose: with every value an integer,
// cross-storage-class ordering, NULLS placement, and collation become irrelevant.
// This path must handle them, so it delegates to compareValuesCollatedEnc,
// the same comparator every other comparison in the engine uses. There is no
// second ordering implementation, which is where wrong answers slip past.
//
// NULL placement is restricted to the default: compareValues sorts NULL lowest,
// so ASC puts NULLs first and DESC puts them last, which is SQLite's default.

// segOrderEntryVal is segOrderEntry over Values rather than int64s.
type segOrderEntryVal struct {
	key []Value
	out []Value
	seq int
}

// segOrderBeforeVal is segOrderBefore's delegating twin. Same tie-break: a full
// key tie falls through to insertion order, because vdbeSorter.entryBefore does.
//
// plainBinary is per key column and is resolved once per statement
// (segOrderPlainBinaryKeys), not once per comparison, to avoid redundant
// collation name matching in the hot loop.
func segOrderBeforeVal(p *segOrderPlan, enc TextEncoding, plainBinary []bool, a, b segOrderEntryVal) bool {
	for i := range a.key {
		var c int
		if i < len(plainBinary) && plainBinary[i] {
			c = compareValues(a.key[i], b.key[i])
		} else {
			coll := ""
			if i < len(p.keyColl) {
				coll = p.keyColl[i]
			}
			c = compareValuesCollatedEnc(a.key[i], b.key[i], coll, enc)
		}
		if c == 0 {
			continue
		}
		if p.keyDesc[i] {
			return c > 0
		}
		return c < 0
	}
	return a.seq < b.seq
}

// segOrderPlainBinaryKeys reports, per key column, whether compareValues answers
// exactly what compareValuesCollatedEnc would -- which is the case when the
// collation is BINARY (or absent, which defaults to it) AND the database is
// UTF-8, because compareValues orders TEXT by its raw bytes.
//
// Resolved once so the name match does not run per comparison. Everything else
// still goes through the collation-aware comparator; there is no second ordering
// implementation here, which is the whole design of this file.
func segOrderPlainBinaryKeys(p *segOrderPlan, enc TextEncoding) []bool {
	out := make([]bool, len(p.keyDesc))
	for i := range out {
		coll := ""
		if i < len(p.keyColl) {
			coll = p.keyColl[i]
		}
		out[i] = enc == UTF8 && (coll == "" || strings.EqualFold(coll, "BINARY"))
	}
	return out
}

func segOrderSiftUpVal(p *segOrderPlan, enc TextEncoding, pb []bool, h []segOrderEntryVal, i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !segOrderBeforeVal(p, enc, pb, h[parent], h[i]) {
			return
		}
		h[parent], h[i] = h[i], h[parent]
		i = parent
	}
}

func segOrderSiftDownVal(p *segOrderPlan, enc TextEncoding, pb []bool, h []segOrderEntryVal, i int) {
	for {
		l, r, worst := 2*i+1, 2*i+2, i
		if l < len(h) && segOrderBeforeVal(p, enc, pb, h[worst], h[l]) {
			worst = l
		}
		if r < len(h) && segOrderBeforeVal(p, enc, pb, h[worst], h[r]) {
			worst = r
		}
		if worst == i {
			return
		}
		h[worst], h[i] = h[i], h[worst]
		i = worst
	}
}

// segOrderLimitValues is segOrderLimitTable reading through segment.Value, which
// is correct for every column and every physical type -- so a TEXT key, a BLOB
// output column or a NULL-bearing one are all in scope.
//
// segment.Value hands back a slice INTO the mapped buffer for a TEXT or BLOB, so
// reading a key costs no copy; the heap retains only `limit` of them.
func (p *ReadOnlyPager) segOrderLimitValues(rootPage uint32, plan *segOrderPlan, ipkCol int) ([][]Value, bool) {
	// Gated on the log's size, not on whether a session holds the rows.
	// The log is reconciled into the merge; when it is large relative to the
	// file, the cost of merging dominates.
	if p != nil && p.segs != nil {
		if segs, have := p.segs.byRoot[rootPage]; !have || !p.segs.deltaIsSmallFor(rootPage, segs) {
			return nil, false
		}
	}
	// A row-major delta beside the segments makes a raw block read wrong; see
	// segCleanFor.
	if !p.segCleanFor(rootPage) {
		return nil, false
	}
	if p == nil || p.segs == nil || plan == nil || plan.limit <= 0 {
		return nil, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok {
		return nil, false
	}
	enc := p.encoding()
	pbin := segOrderPlainBinaryKeys(plan, enc)
	if rows, ok := segOrderLimitBytes(plan, pbin, segs, ipkCol); ok {
		return rows, true
	}
	heap := make([]segOrderEntryVal, 0, plan.limit)
	keyBuf := make([]Value, len(plan.keyCols))
	seq := 0
	for _, s := range segs {
		for _, c := range plan.keyCols {
			if c != ipkCol && (c < 0 || c >= len(s.cols)) {
				return nil, false
			}
		}
		for _, c := range plan.outCols {
			if c != ipkCol && (c < 0 || c >= len(s.cols)) {
				return nil, false
			}
		}
		// TYPED READERS, chosen once per column rather than once per CELL. A
		// column that genuinely needs the general accessor still gets it
		// (segColGeneric); what changes is that the decision stops being made per
		// cell, along with the NULL-bitmap check and exception probe behind it.
		keyReaders := segColReadersLenient(s, plan.keyCols, ipkCol)
		outReaders := segColReadersLenient(s, plan.outCols, ipkCol)
		read := func(rd *segColReader, r int) Value {
			v, _ := rd.value(s, r)
			return v
		}
		for r := 0; r < s.nRows; r++ {
			cand := segOrderEntryVal{seq: seq, key: keyBuf}
			seq++
			// The key goes into a reused buffer and is tested before anything is
			// allocated, saving allocations for rows that don't make the cut.
			for i := range plan.keyCols {
				keyBuf[i] = read(&keyReaders[i], r)
			}
			if len(heap) == plan.limit && !segOrderBeforeVal(plan, enc, pbin, cand, heap[0]) {
				continue
			}
			// It made the cut. While the heap is still FILLING it gets slices of
			// its own, because keyBuf is about to be overwritten by the next row.
			if len(heap) < plan.limit {
				cand.key = append([]Value(nil), keyBuf...)
				cand.out = make([]Value, len(plan.outCols))
				for i := range plan.outCols {
					cand.out[i] = read(&outReaders[i], r)
				}
				heap = append(heap, cand)
				segOrderSiftUpVal(plan, enc, pbin, heap, len(heap)-1)
				continue
			}
			// The winner replaces the worst entry, reusing its slices instead of
			// allocating a pair and dropping a pair. Safe because the entry being
			// overwritten is the one being evicted: its slices are reachable from
			// nowhere else, and only survivors are returned.
			victim := heap[0]
			copy(victim.key, keyBuf) // same width by construction
			for i := range plan.outCols {
				victim.out[i] = read(&outReaders[i], r)
			}
			victim.seq = cand.seq
			heap[0] = victim
			segOrderSiftDownVal(plan, enc, pbin, heap, 0)
		}
	}
	sort.SliceStable(heap, func(i, j int) bool { return segOrderBeforeVal(plan, enc, pbin, heap[i], heap[j]) })
	rows := make([][]Value, len(heap))
	for i, e := range heap {
		rows[i] = e.out
	}
	return rows, true
}

// segOrderRowCells pulls a row-major row's values for the named columns as the
// int64 lanes the ordering works in. bad is true for anything it cannot
// represent -- a short row, or a value that is not an integer -- and the caller
// must then decline the whole statement rather than drop the row.
func segOrderRowCells(vals []Value, cols []int, ipkCol int, rid int64) (out []int64, bad bool) {
	out = make([]int64, len(cols))
	for i, c := range cols {
		if c == ipkCol || c < 0 {
			out[i] = rid
			continue
		}
		if c >= len(vals) || vals[c].Typ != Int {
			return nil, true
		}
		out[i] = vals[c].I
	}
	return out, false
}

// segOrderEntryBytes is a heap entry of segOrderLimitBytes: where the row is and
// what its key is, but not its output -- that is read only for the survivors.
type segOrderEntryBytes struct {
	pre uint64 // the key's order-preserving prefix (segment_prefix.go)
	key []byte // the key's bytes, a slice of its segment's mapped heap
	seg *segment
	row int
	seq int
}

// segOrderBeforeBytes is segOrderBeforeVal for one BINARY byte key: the prefix
// decides unless it ties, then the bytes, then scan order. bytes.Compare is
// compareValues' own order for two TEXT or two BLOB values, which is all this
// path is ever handed (segOrderLimitBytes checks the class).
func segOrderBeforeBytes(desc bool, a, b *segOrderEntryBytes) bool {
	c := 0
	switch {
	case a.pre < b.pre:
		c = -1
	case a.pre > b.pre:
		c = 1
	default:
		c = bytes.Compare(a.key, b.key)
	}
	if c == 0 {
		return a.seq < b.seq
	}
	if desc {
		return c > 0
	}
	return c < 0
}

// segOrderLimitBytes is ORDER BY ... LIMIT for one key column with no NULL
// or exception, using order-preserving uint64 per row instead of Values.
// This avoids comparing 48-byte Values and defers reading output columns to
// the survivors only.
//
// The key per row:
//   - TEXT/BLOB under BINARY: its 8-byte prefix, with heap bytes compared only
//     on a tie;
//   - INTEGER or rowid alias: int64 with sign bit flipped, ordering as the
//     signed value does;
//   - REAL: floatOrderKey, with -0.0 folded to +0.0 per compareValues.
//
// For numeric kinds the uint64 IS the value, so an equal key is a genuine tie.
func segOrderLimitBytes(plan *segOrderPlan, pbin []bool, segs []*segment, ipkCol int) ([][]Value, bool) {
	if len(plan.keyCols) != 1 || plan.limit <= 0 || len(segs) == 0 {
		return nil, false
	}
	col := plan.keyCols[0]
	isRowid := col == ipkCol || col < 0
	var phys PhysicalType
	for i, s := range segs {
		for _, c := range plan.outCols {
			if c != ipkCol && (c < 0 || c >= len(s.cols)) {
				return nil, false
			}
		}
		if isRowid {
			continue
		}
		if col >= len(s.cols) || s.cols[col].nullOff != 0 || s.hasException(col) {
			return nil, false
		}
		if i == 0 {
			phys = s.cols[col].phys
		} else if s.cols[col].phys != phys {
			return nil, false // e.g. TEXT sorts below BLOB: one kind of key per statement
		}
		switch phys {
		case PhysText, PhysBlob:
			if len(pbin) != 1 || !pbin[0] {
				return nil, false
			}
			if _, ok := s.bytesPrefixes(col); !ok {
				return nil, false
			}
		case PhysInt64:
			if _, ok := s.Int64Column(col); !ok {
				return nil, false
			}
		case PhysFloat64:
			if _, ok := s.Float64Column(col); !ok {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	desc := plan.keyDesc[0]
	h := make([]segOrderEntryBytes, 0, plan.limit)
	before := func(i, j int) bool { return segOrderBeforeBytes(desc, &h[i], &h[j]) }
	siftDown := func(i int) {
		for {
			l, r, worst := 2*i+1, 2*i+2, i
			if l < len(h) && before(worst, l) {
				worst = l
			}
			if r < len(h) && before(worst, r) {
				worst = r
			}
			if worst == i {
				return
			}
			h[worst], h[i] = h[i], h[worst]
			i = worst
		}
	}
	const signBit = uint64(1) << 63
	seq := 0
	for _, s := range segs {
		var (
			pre       []uint64
			cells     []uint64
			heapBytes []byte
			ints      []int64
			floats    []float64
		)
		switch {
		case isRowid:
		case phys == PhysInt64:
			ints, _ = s.Int64Column(col)
		case phys == PhysFloat64:
			floats, _ = s.Float64Column(col)
		default:
			pre, _ = s.bytesPrefixes(col)
			cells, heapBytes, _ = s.BytesColumn(col)
		}
		for r := 0; r < s.nRows; r++ {
			cand := segOrderEntryBytes{seg: s, row: r, seq: seq}
			seq++
			switch {
			case isRowid:
				cand.pre = s.Rowid(r) ^ signBit
			case ints != nil:
				cand.pre = uint64(ints[r]) ^ signBit
			case floats != nil:
				cand.pre = floatOrderKey(floats[r])
			default:
				raw := cells[r]
				o, n := uint32(raw), uint32(raw>>32)
				cand.pre, cand.key = pre[r], heapBytes[o:o+n]
			}
			if len(h) < plan.limit {
				h = append(h, cand)
				for i := len(h) - 1; i > 0; {
					parent := (i - 1) / 2
					if !before(parent, i) {
						break
					}
					h[parent], h[i] = h[i], h[parent]
					i = parent
				}
				continue
			}
			if !segOrderBeforeBytes(desc, &cand, &h[0]) {
				continue
			}
			h[0] = cand
			siftDown(0)
		}
	}
	sort.SliceStable(h, func(i, j int) bool { return segOrderBeforeBytes(desc, &h[i], &h[j]) })
	segOrderBytesServed.Add(1)
	rows := make([][]Value, len(h))
	readers := map[*segment][]segColReader{}
	for i, e := range h {
		rd, have := readers[e.seg]
		if !have {
			rd = segColReadersLenient(e.seg, plan.outCols, ipkCol)
			readers[e.seg] = rd
		}
		out := make([]Value, len(plan.outCols))
		for j := range plan.outCols {
			out[j], _ = rd[j].value(e.seg, e.row)
		}
		rows[i] = out
	}
	return rows, true
}

// floatOrderKey maps f to a uint64 that orders as f does: a non-negative
// float's bits with the sign bit set, a negative one's bits inverted. -0.0 is
// folded onto +0.0 first, because compareValues calls them equal. A NaN is
// never stored (SQLite reads it as NULL), and a NULL column never reaches here.
func floatOrderKey(f float64) uint64 {
	if f == 0 {
		f = 0
	}
	b := math.Float64bits(f)
	if b&(1<<63) != 0 {
		return ^b
	}
	return b | 1<<63
}

// segOrderBytesServed counts the statements segOrderLimitBytes answered, so a
// test can tell that path from the general one it falls back to.
var segOrderBytesServed atomic.Int64

// SegOrderBytesServedForTest reports segOrderBytesServed and zeroes it.
func SegOrderBytesServedForTest() int64 { return segOrderBytesServed.Swap(0) }

// segSkipsIn counts the skipped positions in [lo, hi), advancing *sk past them,
// so a run passed over whole keeps seq exactly what a row-at-a-time walk would.
func segSkipsIn(skip []int, sk *int, lo, hi int) int {
	for *sk < len(skip) && skip[*sk] < lo {
		*sk++
	}
	n := 0
	for *sk < len(skip) && skip[*sk] < hi {
		*sk++
		n++
	}
	return n
}
