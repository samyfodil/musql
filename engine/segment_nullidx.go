package engine

import (
	"encoding/binary"
	"math"
)

// A column's NULLs as a 0/1 int64 block per row (1 = NULL), cached per segment
// so the JIT can compose IsNull with other predicates. The mutex prevents
// concurrent readers from racing to build the same cache.
func (s *segment) nullIndicator(col int) ([]int64, bool) {
	if col < 0 || col >= len(s.cols) || s.nRows == 0 {
		return nil, false
	}
	s.nullMu.Lock()
	defer s.nullMu.Unlock()
	if idx, ok := s.nullIdx[col]; ok {
		return idx, true
	}
	out := make([]int64, s.nRows)
	// nullOff == 0 means the column stored no NULL at all, which leaves the
	// block all zeros -- the honest answer, and the reason this needs no
	// special case: "IS NULL" over such a column is false for every row.
	if off := int(s.cols[col].nullOff); off != 0 {
		for r := 0; r < s.nRows; r++ {
			if s.buf[off+r/8]&(1<<uint(r%8)) != 0 {
				out[r] = 1
			}
		}
	}
	if s.nullIdx == nil {
		s.nullIdx = make(map[int][]int64)
	}
	s.nullIdx[col] = out
	return out, true
}

// realNullNaN is REAL column col's block with each NULL row a NaN, which is
// how a compiled program holds a REAL NULL (jit.POpI2F): Float64Column
// itself when the column has no NULL, built once and cached otherwise.
// false for a column that is not a clean REAL block -- not PhysFloat64, or
// holding a value that did not fit one.
func (s *segment) realNullNaN(col int) ([]float64, bool) {
	if col < 0 || col >= len(s.cols) || s.cols[col].nullOff == 0 {
		return s.Float64Column(col)
	}
	if s.cols[col].phys != PhysFloat64 || !segLittleEndian || s.hasException(col) {
		return nil, false
	}
	s.nullMu.Lock()
	defer s.nullMu.Unlock()
	if f, ok := s.nanReal[col]; ok {
		return f, true
	}
	off, null := int(s.cols[col].blockOff), int(s.cols[col].nullOff)
	if off == 0 || off+8*s.nRows > len(s.buf) {
		return nil, false
	}
	nan := math.NaN()
	out := make([]float64, s.nRows)
	for r := range out {
		if s.buf[null+r/8]&(1<<uint(r%8)) != 0 {
			out[r] = nan
			continue
		}
		out[r] = math.Float64frombits(binary.LittleEndian.Uint64(s.buf[off+8*r:]))
	}
	if s.nanReal == nil {
		s.nanReal = make(map[int][]float64)
	}
	s.nanReal[col] = out
	return out, true
}
