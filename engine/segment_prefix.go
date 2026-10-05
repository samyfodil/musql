package engine

import "encoding/binary"

// A TEXT or BLOB column's order-preserving prefixes: the first 8 bytes
// big-endian, zero-padded. A comparison can usually decide without a heap read.
//
// Two prefixes that differ differ first at byte k < 8. If both cells have a
// real byte at k, that byte decides order. If one cell ended, its padding is 0,
// and the ended cell sorts first -- exactly bytes.Compare order. Equal prefixes
// require heap comparison.
//
// Cached like null indicators: a segment is read-only, so the cache is stable.
func (s *segment) bytesPrefixes(col int) ([]uint64, bool) {
	cells, heap, ok := s.BytesColumn(col)
	if !ok {
		return nil, false
	}
	s.prefMu.Lock()
	defer s.prefMu.Unlock()
	if p, have := s.prefIdx[col]; have {
		return p, p != nil
	}
	if s.prefIdx == nil {
		s.prefIdx = make(map[int][]uint64)
	}
	out := make([]uint64, len(cells))
	for i, raw := range cells {
		o, n := uint32(raw), uint32(raw>>32)
		if int(o)+int(n) > len(heap) {
			// A cell that does not describe heap bytes: refuse the column, so the
			// exact loop keeps its own handling of it (it excludes the row).
			s.prefIdx[col] = nil
			return nil, false
		}
		out[i] = bytesPrefix8(heap[o : o+n])
	}
	s.prefIdx[col] = out
	return out, true
}

// bytesPrefix8 is b's first 8 bytes as a big-endian uint64, zero-padded.
func bytesPrefix8(b []byte) uint64 {
	if len(b) >= 8 {
		return binary.BigEndian.Uint64(b)
	}
	var p [8]byte
	copy(p[:], b)
	return binary.BigEndian.Uint64(p[:])
}
