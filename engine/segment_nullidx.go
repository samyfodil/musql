package engine

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
