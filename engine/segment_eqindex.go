package engine

import "sync"

// An equality index over one columnar column: which rows hold a value,
// and how many. The implementation lives in segment_index_ondisk.go.
// A column whose file carries no index has no index; the caller scans.

// eqCountMu guards the lazily opened index views. A segment is read-only and
// shared between concurrent readers, so the first two queries to want the same
// column's view must not both build it into the same map.
type eqCountMu = sync.Mutex

// segEqualityCount answers "how many rows have col = v" from the file's index,
// or reports false when this column has none.
func (s *segment) segEqualityCount(col int, v int64) (int, bool) {
	s.eqMu.Lock()
	x, ok := s.diskIndexFor(col)
	s.eqMu.Unlock()
	if !ok {
		return 0, false
	}
	return x.countFor(v)
}

// segEqualityRows is "which rows have col = v", ascending by position -- which is
// ascending by ROWID too, because a segment stores its rows in rowid order (see
// segment_seek.go). Reports false when this column has no index.
func (s *segment) segEqualityRows(col int, v int64) ([]int32, bool) {
	s.eqMu.Lock()
	x, ok := s.diskIndexFor(col)
	s.eqMu.Unlock()
	if !ok || x.codec != segIdxCodecPostings {
		return nil, false
	}
	return x.rowsFor(v, nil), true
}

// diskIndexFor is this column's on-disk index, opened once and remembered. The
// view it returns points into the segment's mapped bytes and copies nothing.
func (s *segment) diskIndexFor(col int) (*segDiskIndex, bool) {
	if col < 0 || col >= len(s.cols) || s.cols[col].idxOff == 0 {
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
	x, ok := openSegColumnIndex(s.buf, s.cols[col].idxOff, s.nRows, s.heap)
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
