package engine

import (
	"encoding/binary"
	"math"
	"slices"
	"sync"
	"unsafe"
)

// An equality index over one columnar column: which rows hold a value,
// and how many. The implementation lives in segment_index_ondisk.go.
// A column whose file carries no index has no index; the caller scans.

// eqCountMu guards the lazily opened index views. A segment is read-only and
// shared between concurrent readers, so the first two queries to want the same
// column's view must not both build it into the same map.
type eqCountMu = sync.Mutex

// segEqualityCount answers "how many rows have col = v" from the column's
// index, or reports false when it has none or a row of another storage class
// might equal v. A NULL row equals nothing.
func (s *segment) segEqualityCount(col int, v int64) (int, bool) {
	s.eqMu.Lock()
	x, exc, ok := s.seekIndexFor(col)
	s.eqMu.Unlock()
	if !ok {
		return 0, false
	}
	probe := Value{Typ: Int, I: v}
	for _, r := range exc {
		if !certainlyUnequal(s.Value(col, int(r)), probe) {
			return 0, false
		}
	}
	return x.countFor(v)
}

// seekRows appends to dst the rows of col a seek for probe tests, ascending by
// position -- which is ascending by ROWID too, because a segment stores its
// rows in rowid order (see segment_seek.go): those the column's index holds
// probe in, and those of another storage class that might equal it. false
// when col has no index probe can ask.
func (s *segment) seekRows(col int, probe Value, dst []int32) ([]int32, bool) {
	s.eqMu.Lock()
	x, exc, ok := s.seekIndexFor(col)
	s.eqMu.Unlock()
	if !ok {
		return dst, false
	}
	at := len(dst)
	switch probe.Typ {
	case Int:
		switch x.codec {
		case segIdxCodecPostings:
			dst = x.rowsFor(probe.I, dst)
		case segIdxCodecFloat:
			// A REAL equals the INTEGER of its value. One too wide for a
			// double finds the REALs nearest it, which the seek tests again.
			dst, _ = x.rowsForFloat(float64(probe.I), dst)
		default:
			return dst, false
		}
	case Float:
		switch x.codec {
		case segIdxCodecFloat:
			dst, _ = x.rowsForFloat(probe.F, dst)
		case segIdxCodecPostings:
			// Only an integral REAL equals an INTEGER, and only the one of
			// its value.
			if f := probe.F; f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63 {
				dst = x.rowsFor(int64(f), dst)
			}
		default:
			return dst, false
		}
	case Text, Blob:
		if dst, ok = x.rowsForBytes(probe.S, dst); !ok {
			return dst, false
		}
	default:
		return dst, false
	}
	hit := len(dst)
	for _, r := range exc {
		if !certainlyUnequal(s.Value(col, int(r)), probe) {
			dst = append(dst, r)
		}
	}
	if hit > at && len(dst) > hit {
		slices.Sort(dst[at:])
	}
	return dst, true
}

// seekIndexFor is col's index for a seek, which tests each row it is given:
// the file's (diskIndexFor), or partIndexFor's, with the rows it leaves out
// that are of another storage class. A PhysNumeric column, INTEGERs and REALs
// in one block of doubles, has its index ordered by value as a REAL column's
// is. The caller holds eqMu.
func (s *segment) seekIndexFor(col int) (*segDiskIndex, []int32, bool) {
	if x, ok := s.diskIndexFor(col); ok {
		return x, nil, true
	}
	if p, ok := s.partIndexFor(col); ok {
		return p.x, p.exc, true
	}
	return nil, nil, false
}

// segPartIndex is the index of a column holding NULLs or values of another
// storage class over its other rows, and the rows of another class,
// ascending. A NULL equals nothing, so its rows are in neither.
type segPartIndex struct {
	x   *segDiskIndex
	exc []int32
}

// partIndexFor is col's segPartIndex, built on first use, for the column
// diskIndexFor refuses for its NULLs or exceptions. Only a segment whose rows
// are all full width has one: a short row is marked NULL and reads as the
// column's DEFAULT. The caller holds eqMu.
func (s *segment) partIndexFor(col int) (*segPartIndex, bool) {
	if col < 0 || col >= len(s.cols) || !s.fullWidth || s.nRows == 0 || s.cols[col].nullOff == 0 && !s.hasException(col) && s.cols[col].phys != PhysNumeric {
		return nil, false
	}
	if p, seen := s.partIdx[col]; seen {
		return p, p != nil
	}
	if s.partIdx == nil {
		s.partIdx = map[int]*segPartIndex{}
	}
	p := s.buildPartIndex(col)
	s.partIdx[col] = p
	return p, p != nil
}

// buildPartIndex is partIndexFor's index, nil when col's cells cannot be read
// in place or no row is of the column's own class.
func (s *segment) buildPartIndex(col int) *segPartIndex {
	var cells []int64
	var heap []byte
	switch s.cols[col].phys {
	case PhysInt64, PhysFloat64, PhysNumeric:
		v, ok := s.fixedBlock(col)
		if !ok {
			return nil
		}
		cells = v
	case PhysText, PhysBlob:
		c, ok := s.rawBytesCells(col)
		if !ok {
			return nil
		}
		cells, heap = unsafe.Slice((*int64)(unsafe.Pointer(unsafe.SliceData(c))), len(c)), s.heap
	default:
		return nil
	}
	if len(cells) != s.nRows {
		return nil
	}
	p := &segPartIndex{}
	pos := make([]int32, 0, s.nRows)
	sub := make([]int64, 0, s.nRows)
	var exc []uint64
	if s.hasException(col) {
		exc = s.excRows[col]
	}
	for i := range s.nRows {
		switch {
		case exc != nil && exc[i/64]&(1<<uint(i%64)) != 0:
			p.exc = append(p.exc, int32(i))
		case !s.isNull(col, i):
			pos, sub = append(pos, int32(i)), append(sub, cells[i])
		}
	}
	if len(sub) == 0 {
		return nil
	}
	var blk []byte
	var ok bool
	switch s.cols[col].phys {
	case PhysInt64:
		blk, ok = buildSegColumnIndex(sub)
	case PhysFloat64, PhysNumeric:
		blk, ok = buildSegColumnIndexFloat(sub)
	default:
		blk, ok = buildSegColumnIndexBytes(sub, heap)
	}
	if !ok {
		return nil
	}
	buf := make([]byte, 8+len(blk)) // 8-aligned, as in a file
	copy(buf[8:], blk)
	if p.x, ok = openSegColumnIndex(buf, 8, len(sub), heap, nil); !ok {
		return nil
	}
	// The record's positions are into sub; the segment's rows are pos.
	for k := range p.x.nRows {
		at := p.x.posts[4*k:]
		binary.LittleEndian.PutUint32(at, uint32(pos[binary.LittleEndian.Uint32(at)]))
	}
	return p
}

// diskIndexFor is this column's on-disk index, opened once and remembered. The
// view it returns points into the segment's mapped bytes and copies nothing.
func (s *segment) diskIndexFor(col int) (*segDiskIndex, bool) {
	if col < 0 || col >= len(s.cols) {
		return nil, false
	}
	// THE READER CHECKS WHAT THE INDEX CLAIMS TO DESCRIBE, rather than trusting
	// an offset. An int64 index is meaningful only over a column that really is a
	// clean int64 block: the integer physical type, no NULL bitmap, and no
	// exception value that did not fit. A writer that got its own condition wrong
	// would otherwise hand this a block of TEXT heap references read as integers,
	// and the lookup would answer confidently about rows that do not hold the
	// value at all.
	//
	// It is not hypothetical: the writer's guard was mutation-tested by removing
	// each of its three terms in turn, and with only the writer enforcing them all
	// three mutants went unnoticed. This is the half that catches them.
	if s.cols[col].nullOff != 0 || s.hasException(col) {
		return nil, false
	}
	switch s.cols[col].phys {
	case PhysInt64, PhysText, PhysBlob, PhysFloat64:
	default:
		return nil, false
	}
	if s.diskIdx == nil {
		s.diskIdx = map[int]*segDiskIndex{}
	}
	if x, seen := s.diskIdx[col]; seen {
		return x, x != nil
	}
	want := uint16(segIdxCodecPostings)
	switch s.cols[col].phys {
	case PhysText, PhysBlob:
		want = segIdxCodecBytes
	case PhysFloat64:
		want = segIdxCodecFloat
	}
	buf, off := s.buf, s.cols[col].idxOff
	if off == 0 {
		// A segment written without its indexes -- an append load streams
		// its rows into one and skips them, which is most of what made the
		// load cheaper than C's -- gets the same record built on first use, in
		// memory, and opened through the same validation below. Only a column
		// someone seeks pays for it, once per segment; the next compaction
		// writes it into the file like any other.
		blk, ok := s.buildColumnIndex(col)
		if !ok {
			s.diskIdx[col] = nil
			return nil, false
		}
		buf, off = make([]byte, 8+len(blk)), 8 // 8-aligned, as in a file
		copy(buf[8:], blk)
	}
	x, ok := openSegColumnIndex(buf, off, s.nRows, s.heap, func(i int) ([]byte, bool) { return s.cellBytes(col, i) })
	if ok && want == segIdxCodecBytes && x.codec == segIdxCodecBytesPerm {
		want = x.codec // the same question, in the packed layout
	}
	if ok && x.codec != want {
		// The codec must match what the COLUMN is, not merely be one this build
		// implements: an int64 index over a TEXT block would compare heap
		// references as numbers.
		ok = false
	}
	if !ok {
		s.diskIdx[col] = nil // a block that does not describe this segment
		return nil, false
	}
	s.diskIdx[col] = x
	return x, true
}

// buildColumnIndex lays out col's index record exactly as buildSegmentWith
// would have written it into the file, from the column's cells in place.
// false when the column has no flat cell block to read (the caller has
// already refused NULLs and exceptions), a row is short, or the record would
// be too large.
func (s *segment) buildColumnIndex(col int) ([]byte, bool) {
	// A short row reads as the column's DEFAULT whatever its cell holds. The
	// writer counts it a NULL, so its column has a bitmap and never gets here;
	// this refuses a file that says otherwise. Every append-load segment is
	// full width.
	if !s.fullWidth || s.nRows == 0 {
		return nil, false
	}
	switch s.cols[col].phys {
	case PhysInt64:
		vals, ok := s.Int64Column(col)
		if !ok {
			return nil, false
		}
		return buildSegColumnIndex(vals)
	case PhysFloat64:
		vals, ok := s.Float64Column(col)
		if !ok {
			return nil, false
		}
		return buildSegColumnIndexFloat(unsafe.Slice((*int64)(unsafe.Pointer(unsafe.SliceData(vals))), len(vals)))
	default:
		cells, heap, ok := s.BytesColumn(col)
		if !ok {
			return nil, false
		}
		return buildSegColumnIndexBytesPerm(unsafe.Slice((*int64)(unsafe.Pointer(unsafe.SliceData(cells))), len(cells)), heap)
	}
}

// segEqualityRowsBytes is segEqualityRows for a TEXT or BLOB probe, compared
// under BINARY -- the only collation this index can answer.
func (s *segment) segEqualityRowsBytes(col int, probe []byte) ([]int32, bool) {
	s.eqMu.Lock()
	x, ok := s.diskIndexFor(col)
	s.eqMu.Unlock()
	if !ok {
		return nil, false
	}
	return x.rowsForBytes(probe, nil)
}

// segEqualityCountOf is how many rows seekRows would answer for probe, a TEXT
// or BLOB probe compared under BINARY, without building their positions.
// false when this column has no index the probe can ask.
func (s *segment) segEqualityCountOf(col int, probe Value) (int, bool) {
	s.eqMu.Lock()
	x, exc, ok := s.seekIndexFor(col)
	s.eqMu.Unlock()
	if !ok {
		return 0, false
	}
	n, ok := x.countForValue(probe)
	for _, r := range exc {
		if !certainlyUnequal(s.Value(col, int(r)), probe) {
			n++
		}
	}
	return n, ok
}

// segEqualityRowsFloat is segEqualityRows for a REAL probe.
func (s *segment) segEqualityRowsFloat(col int, probe float64) ([]int32, bool) {
	s.eqMu.Lock()
	x, ok := s.diskIndexFor(col)
	s.eqMu.Unlock()
	if !ok {
		return nil, false
	}
	return x.rowsForFloat(probe, nil)
}
