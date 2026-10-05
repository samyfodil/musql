package engine

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"math"
	"slices"
)

// AN EQUALITY INDEX THAT LIVES IN THE FILE, not in the Go heap.
//
// It is written inside the segment it indexes, which makes staleness
// impossible: a segment is immutable and written in one pass, so an index
// in it describes exactly the rows beside it and can never disagree with them.
//
// ---- the record ----
//
// A DESCRIBED, ALIGNED, LENGTH-BEARING record, not a bare offset. The first cut
// was a bare offset to three arrays, and a review pointed out that
// it had nowhere to put the next revision and no way to say where it ended:
//
//	version   uint16    1
//	codec     uint16    segIdxCodecPostings
//	nVals     uint32    distinct values
//	nRows     uint32    rows this index describes, checked against the segment
//	byteLen   uint32    the whole record, so the next one's bytes are not ours
//	values    int64  x nVals      distinct column values, ascending
//	starts    uint32 x nVals+1    prefix sums into posts
//	posts     uint32 x nRows      row positions, grouped by value, ascending
//
// The header is 16 bytes and the record is 8-BYTE ALIGNED, so `values` begins
// 8-aligned too and a little-endian host could reinterpret it as []int64 rather
// than decoding. codec exists because this posting layout is the WRONG one at
// high cardinality -- for a UNIQUE column it costs 20 bytes per row where a
// sorted (value,pos) pair record costs 12, and `starts` is then pure overhead.
// That codec is not written yet; the field is what lets it be, without a second
// guess about what an offset points at.
//
// ---- what it does NOT claim ----
//
// It is not zero-allocation. rowsFor decodes each position and appends, so a
// lookup that matches k rows allocates k. An earlier version of this comment said
// otherwise and the code never did; the honest statement is that it allocates for
// the RESULT and never for the index. countFor is genuinely allocation-free and
// is what the count fast path uses.
//
// ---- validation ----
//
// EVERY FIELD IS CHECKED, because this structure decides which rows a query sees
// and the file may be anything. A corrupt record must decline, never panic (the
// engine's invariant 3) and never return a row that does not hold the value.
// openSegColumnIndex checks the record fits, that its length covers its own
// arrays, that nRows matches the segment, that `values` really is ascending, that
// `starts` is monotone and ends at nRows, and that every position is in range.
// That is O(nVals + nRows) once per column per open, against an O(nRows) scan per
// QUERY, and it is the only thing standing between a damaged file and a wrong
// answer.

const (
	segIdxVersion = 1
	// segIdxCodecPostings orders by int64 value. segIdxCodecBytes has the same
	// layout but orders by bytes: a TEXT or BLOB cell is packed as (heapOffset,
	// length) uint64, so only the comparator changes. TEXT columns benefit most
	// from indexing.
	segIdxCodecPostings = 1
	segIdxCodecBytes    = 2
	// segIdxCodecFloat orders by the float value, not the bits: the sign bit
	// makes negatives sort above positives, and -0.0 and +0.0 have different
	// bits but compare equal.
	segIdxCodecFloat = 3
	segIdxHdr           = 16 // version, codec, nVals, nRows, byteLen
)

// segIdxTooLarge caps what a single column's index may cost. A segment holds up
// to 2^24 rows, and at 20 bytes a row a UNIQUE column of that size would add
// 320 MiB to a 128 MiB column -- built, sorted and written on every rewrite,
// whether any query wants it or not. Past this the column keeps its scan.
const segIdxTooLarge = 64 << 20

// buildSegColumnIndex lays out one column's index over int64 values. A nil
// compare is buildSegColumnIndexWith's cue to sort (value, position) pairs
// directly rather than positions through a callback.
func buildSegColumnIndex(vals []int64) ([]byte, bool) {
	return buildSegColumnIndexWith(vals, segIdxCodecPostings, nil)
}

// buildSegColumnIndexFloat lays out one column's index over REAL cells, ordered
// by VALUE.
//
// A NaN anywhere refuses the column. NaN compares false against everything
// including itself, so it has no place in a sorted array -- there is no position
// at which it belongs, and a binary search over an array containing one can walk
// the wrong way. SQLite stores a NaN as NULL on the way in, so this is a guard
// against a file that says otherwise rather than a shape a write can produce.
func buildSegColumnIndexFloat(cells []int64) ([]byte, bool) {
	for _, c := range cells {
		if f := math.Float64frombits(uint64(c)); math.IsNaN(f) {
			return nil, false
		}
	}
	return buildSegColumnIndexWith(cells, segIdxCodecFloat, func(a, b int64) int {
		x, y := math.Float64frombits(uint64(a)), math.Float64frombits(uint64(b))
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0 // includes -0.0 == +0.0, which is what SQLite compares them as
	})
}

// buildSegColumnIndexBytes lays out one column's index over TEXT or BLOB cells,
// ordered by BYTES -- SQLite's BINARY collation, which is the only one this can
// answer (NOCASE and RTRIM change which values are equal, so a column compared
// under either gets no index and is scanned).
//
// cells are the raw packed (heapOffset, length) uint64s; heap is the segment's
// own byte arena, so a comparison reads the mapped bytes and copies nothing.
func buildSegColumnIndexBytes(cells []int64, heap []byte) ([]byte, bool) {
	at := func(c int64) []byte {
		o, n := uint32(uint64(c)), uint32(uint64(c)>>32)
		if int(o)+int(n) > len(heap) {
			return nil
		}
		return heap[o : o+n]
	}
	for _, c := range cells {
		if at(c) == nil {
			return nil, false // a cell that does not describe heap bytes
		}
	}
	return buildSegColumnIndexWith(cells, segIdxCodecBytes, func(a, b int64) int {
		return bytes.Compare(at(a), at(b))
	})
}

// radixOrderInt64 returns the positions of vals ordered by (value, position):
// an LSD radix sort on the value with its sign bit flipped. Each pass is stable
// and the positions start ascending, so ties come out in position order. A pass
// whose byte every key shares moves nothing and is skipped, so clustered values
// cost fewer passes.
func radixOrderInt64(vals []int64) []int32 {
	n := len(vals)
	keys, tk := make([]uint64, n), make([]uint64, n)
	order, to := make([]int32, n), make([]int32, n)
	for i, v := range vals {
		keys[i] = uint64(v) ^ 1<<63
		order[i] = int32(i)
	}
	for shift := uint(0); shift < 64; shift += 8 {
		var at [256]int
		for _, k := range keys {
			at[k>>shift&0xff]++
		}
		if at[keys[0]>>shift&0xff] == n {
			continue
		}
		sum := 0
		for b, c := range at {
			at[b] = sum
			sum += c
		}
		for i, k := range keys {
			b := k >> shift & 0xff
			tk[at[b]], to[at[b]] = k, order[i]
			at[b]++
		}
		keys, tk = tk, keys
		order, to = to, order
	}
	return order
}

func buildSegColumnIndexWith(vals []int64, codec uint16, compare func(a, b int64) int) ([]byte, bool) {
	n := len(vals)
	if n == 0 {
		return nil, false
	}
	// Order the row positions by value. Ties keep position order, which is
	// load-bearing: a segment stores rows in rowid order, so every posting list's
	// consumers rely on getting rowids ascending. The position tie-break makes
	// the order total, so an unstable sort is exact.
	var order []int32
	if compare == nil {
		// Natural int64 order, by radix: see radixOrderInt64.
		order = radixOrderInt64(vals)
		compare = cmp.Compare[int64]
	} else {
		order = make([]int32, n)
		for i := range order {
			order[i] = int32(i)
		}
		slices.SortFunc(order, func(x, y int32) int {
			if c := compare(vals[x], vals[y]); c != 0 {
				return c
			}
			return cmp.Compare(x, y)
		})
	}
	nVals := 1
	for i := 1; i < n; i++ {
		if compare(vals[order[i]], vals[order[i-1]]) != 0 {
			nVals++
		}
	}
	size := segIdxHdr + nVals*8 + (nVals+1)*4 + n*4
	if size > segIdxTooLarge {
		return nil, false
	}
	out := make([]byte, size)
	binary.LittleEndian.PutUint16(out, segIdxVersion)
	binary.LittleEndian.PutUint16(out[2:], codec)
	binary.LittleEndian.PutUint32(out[4:], uint32(nVals))
	binary.LittleEndian.PutUint32(out[8:], uint32(n))
	binary.LittleEndian.PutUint32(out[12:], uint32(size))
	valsAt := segIdxHdr
	startsAt := valsAt + nVals*8
	postsAt := startsAt + (nVals+1)*4
	v := 0
	for i := 0; i < n; i++ {
		if i == 0 || compare(vals[order[i]], vals[order[i-1]]) != 0 {
			binary.LittleEndian.PutUint64(out[valsAt+v*8:], uint64(vals[order[i]]))
			binary.LittleEndian.PutUint32(out[startsAt+v*4:], uint32(i))
			v++
		}
		binary.LittleEndian.PutUint32(out[postsAt+i*4:], uint32(order[i]))
	}
	binary.LittleEndian.PutUint32(out[startsAt+nVals*4:], uint32(n))
	return out, true
}


// segDiskIndex is one column's on-disk index, as a view over the mapped bytes.
// Every field is a subslice of the segment's own buffer; nothing is copied.
type segDiskIndex struct {
	codec  uint16
	nVals  int
	nRows  int
	vals   []byte // nVals * 8
	starts []byte // (nVals+1) * 4
	posts  []byte // nRows * 4
	heap   []byte // the segment's byte arena, for segIdxCodecBytes
}

// bytesAt is the heap bytes a packed (heapOffset, length) cell names.
func (x *segDiskIndex) bytesAt(i int) []byte {
	c := uint64(x.valueAt(i))
	o, n := uint32(c), uint32(c>>32)
	if int(o)+int(n) > len(x.heap) {
		return nil
	}
	return x.heap[o : o+n]
}

// openSegColumnIndex validates one index record against the segment it belongs
// to. false for anything that does not check out -- which sends the caller to the
// scan, the one answer that is always right.
func openSegColumnIndex(buf []byte, off uint32, nRows int, heap []byte) (*segDiskIndex, bool) {
	if off == 0 || off%8 != 0 || int(off)+segIdxHdr > len(buf) {
		return nil, false
	}
	b := buf[off:]
	if binary.LittleEndian.Uint16(b) != segIdxVersion {
		return nil, false
	}
	codec := binary.LittleEndian.Uint16(b[2:])
	switch codec {
	case segIdxCodecPostings, segIdxCodecBytes, segIdxCodecFloat:
	default:
		return nil, false // a codec this build does not implement
	}
	nVals := int(binary.LittleEndian.Uint32(b[4:]))
	claimRows := int(binary.LittleEndian.Uint32(b[8:]))
	byteLen := int(binary.LittleEndian.Uint32(b[12:]))
	if claimRows != nRows || nVals <= 0 || nVals > nRows {
		return nil, false
	}
	need := segIdxHdr + nVals*8 + (nVals+1)*4 + nRows*4
	// byteLen bounds THIS record, so a damaged one cannot reach into the next.
	if byteLen < need || byteLen > len(b) {
		return nil, false
	}
	b = b[:byteLen]
	valsAt := segIdxHdr
	startsAt := valsAt + nVals*8
	postsAt := startsAt + (nVals+1)*4
	idx := &segDiskIndex{
		codec:  codec,
		heap:   heap, // BEFORE validation: the ascending check compares BYTES for
		nVals:  nVals, // the bytes codec, and a nil heap makes every value equal
		nRows:  nRows,
		vals:   b[valsAt:startsAt],
		starts: b[startsAt:postsAt],
		posts:  b[postsAt : postsAt+nRows*4],
	}
	// ASCENDING VALUES, or the binary search below is meaningless and would
	// answer about whichever value it happened to land on.
	for i := 1; i < nVals; i++ {
		if idx.compareAt(i-1, i) >= 0 {
			return nil, false
		}
	}
	// STRICTLY INCREASING STARTS, ending exactly at nRows, so every posting list
	// is a real sub-slice and together they partition the positions.
	//
	// Strictly, not merely non-decreasing: every value in the array came FROM a
	// row, so an empty posting list for one is a contradiction -- and accepting it
	// would let a damaged record hide rows that exist. A non-decreasing check let
	// exactly that through (starts[1] forced to 0) until the corruption test said
	// so.
	prev := idx.start(0)
	if prev != 0 {
		return nil, false
	}
	for i := 1; i <= nVals; i++ {
		s := idx.start(i)
		if s <= prev || int(s) > nRows {
			return nil, false
		}
		prev = s
	}
	if prev != uint32(nRows) {
		return nil, false
	}
	// POSITIONS IN RANGE. Not that each one holds its group's value -- that would
	// need the column block and cost a second pass over it -- but in range is what
	// stands between a corrupt index and an out-of-bounds read.
	for p := 0; p < nRows; p++ {
		if int(binary.LittleEndian.Uint32(idx.posts[p*4:])) >= nRows {
			return nil, false
		}
	}
	return idx, true
}

// compareAt orders two entries under this index's own codec.
func (x *segDiskIndex) compareAt(i, j int) int {
	switch x.codec {
	case segIdxCodecBytes:
		return bytes.Compare(x.bytesAt(i), x.bytesAt(j))
	case segIdxCodecFloat:
		p, q := math.Float64frombits(uint64(x.valueAt(i))), math.Float64frombits(uint64(x.valueAt(j)))
		switch {
		case p < q:
			return -1
		case p > q:
			return 1
		}
		return 0
	}
	a, b := x.valueAt(i), x.valueAt(j)
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// findBytes is find for segIdxCodecBytes: the index of probe under BINARY
// ordering, and whether it is there.
func (x *segDiskIndex) findBytes(probe []byte) (int, bool) {
	lo, hi := 0, x.nVals
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if bytes.Compare(x.bytesAt(mid), probe) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < x.nVals && bytes.Equal(x.bytesAt(lo), probe)
}

// rowsForFloat is rowsFor for a REAL probe.
func (x *segDiskIndex) rowsForFloat(probe float64, dst []int32) ([]int32, bool) {
	if x.codec != segIdxCodecFloat || math.IsNaN(probe) {
		return dst, false
	}
	lo, hi := 0, x.nVals
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if math.Float64frombits(uint64(x.valueAt(mid))) < probe {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= x.nVals || math.Float64frombits(uint64(x.valueAt(lo))) != probe {
		return dst, true
	}
	return x.appendPosts(lo, dst), true
}

// rowsForBytes is rowsFor for a TEXT or BLOB probe.
func (x *segDiskIndex) rowsForBytes(probe []byte, dst []int32) ([]int32, bool) {
	if x.codec != segIdxCodecBytes {
		return dst, false
	}
	i, ok := x.findBytes(probe)
	if !ok {
		return dst, true
	}
	return x.appendPosts(i, dst), true
}

// appendPosts adds value i's row positions to dst.
func (x *segDiskIndex) appendPosts(i int, dst []int32) []int32 {
	from, to := x.start(i), x.start(i+1)
	if n := int(to - from); cap(dst)-len(dst) < n {
		grown := make([]int32, len(dst), len(dst)+n)
		copy(grown, dst)
		dst = grown
	}
	for p := from; p < to; p++ {
		dst = append(dst, int32(binary.LittleEndian.Uint32(x.posts[p*4:])))
	}
	return dst
}

func (x *segDiskIndex) valueAt(i int) int64 {
	return int64(binary.LittleEndian.Uint64(x.vals[i*8:]))
}

func (x *segDiskIndex) start(i int) uint32 {
	return binary.LittleEndian.Uint32(x.starts[i*4:])
}

// find is the binary search: the index of v in the ascending value array, and
// whether it is there. Reads the mapped bytes directly and allocates nothing.
func (x *segDiskIndex) find(v int64) (int, bool) {
	lo, hi := 0, x.nVals
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if x.valueAt(mid) < v {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < x.nVals && x.valueAt(lo) == v
}

// rowsFor appends the row positions holding v to dst, ascending. It allocates for
// the RESULT -- one entry per matching row -- and never for the index.
func (x *segDiskIndex) rowsFor(v int64, dst []int32) []int32 {
	if x.codec != segIdxCodecPostings {
		return dst
	}
	i, ok := x.find(v)
	if !ok {
		return dst
	}
	return x.appendPosts(i, dst)
}

// countFor is how many rows hold v, without materialising their positions. This
// one really is allocation-free, and it is what the count fast path uses.
//
// served is false for an index whose codec is not the int64 one. It MUST be a
// separate signal from a count of zero: an integer probe against a TEXT column's
// index has no answer, and returning 0 as though it did is a wrong count wearing
// the shape of an empty result. TestEqualityIndexMatchesTheScan asserts exactly
// that, and caught this the moment TEXT columns started carrying indexes.
func (x *segDiskIndex) countFor(v int64) (int, bool) {
	if x.codec != segIdxCodecPostings {
		return 0, false
	}
	i, ok := x.find(v)
	if !ok {
		return 0, true
	}
	return int(x.start(i+1) - x.start(i)), true
}
