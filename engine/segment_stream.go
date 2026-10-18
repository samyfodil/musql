package engine

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// segStream builds one segment a row at a time, keeping each column as it
// arrives in memory with no pointers in it -- a type byte and an 8-byte cell
// per row, and the column's TEXT and BLOB payload in a heap of its own -- so
// the rows of an append load (insert_append_delta.go) cost the garbage
// collector nothing to hold, and finishing the segment is laying those
// columns out, not walking a [][]Value.
//
// finish gives the bytes buildSegmentWith(..., false) gives for the same rows:
// same census, same physical types, same exceptions, no equality indexes.
// TestSegStreamMatchesBuildSegment holds it to that, byte for byte. Rows must
// be full width (an append load's are: the VM builds every column).
type segStream struct {
	nCols  int
	rowids []uint64
	cols   []streamCol
}

type streamCol struct {
	typ    []byte   // the Value's type, streamIntReal set for an intReal Float
	cell   []uint64 // INTEGER bits, REAL bits, or a payload's offset | length<<32
	heap   []byte   // TEXT and BLOB payloads, in row order
	census ColumnTypeCensus
}

const streamIntReal = 0x80

func newSegStream(nCols int) *segStream {
	return &segStream{nCols: nCols, cols: make([]streamCol, nCols)}
}

// add takes one row. It reports false, taking nothing, for a row that is not
// full width.
func (s *segStream) add(rowid uint64, vals []Value) bool {
	if len(vals) != s.nCols {
		return false
	}
	s.rowids = append(s.rowids, rowid)
	for c := range s.cols {
		col := &s.cols[c]
		v := &vals[c]
		t := byte(v.Typ)
		var cell uint64
		switch v.Typ {
		case Null:
			col.census.Null++
		case Int:
			col.census.Int++
			cell = uint64(v.I)
		case Float:
			col.census.Real++
			cell = floatBits(v.F)
			if v.intReal {
				t |= streamIntReal
			}
		case Text, Blob:
			if v.Typ == Text {
				col.census.Text++
			} else {
				col.census.Blob++
			}
			cell = uint64(len(col.heap)) | uint64(len(v.S))<<32
			col.heap = append(col.heap, v.S...)
		}
		col.typ = append(col.typ, t)
		col.cell = append(col.cell, cell)
	}
	return true
}

func (s *segStream) rows() int { return len(s.rowids) }

// truncate keeps the first n rows.
func (s *segStream) truncate(n int) {
	if n >= len(s.rowids) {
		return
	}
	for c := range s.cols {
		col := &s.cols[c]
		heapEnd := len(col.heap)
		for i := n; i < len(col.typ); i++ {
			switch ValueType(col.typ[i] &^ streamIntReal) {
			case Null:
				col.census.Null--
			case Int:
				col.census.Int--
			case Float:
				col.census.Real--
			case Text, Blob:
				if ValueType(col.typ[i]) == Text {
					col.census.Text--
				} else {
					col.census.Blob--
				}
				heapEnd = min(heapEnd, int(uint32(col.cell[i])))
			}
		}
		col.typ, col.cell, col.heap = col.typ[:n], col.cell[:n], col.heap[:heapEnd]
	}
	s.rowids = s.rowids[:n]
}

// value is row i's value in column c, its payload aliasing the stream.
func (col *streamCol) value(i int) Value {
	t := col.typ[i]
	v := Value{Typ: ValueType(t &^ streamIntReal)}
	switch v.Typ {
	case Int:
		v.I = int64(col.cell[i])
	case Float:
		v.F = bitsFloat(col.cell[i])
		v.intReal = t&streamIntReal != 0
	case Text, Blob:
		off, n := uint32(col.cell[i]), uint32(col.cell[i]>>32)
		v.S = col.heap[off : off+n : off+n]
	}
	return v
}

// finish lays the rows out as a segment.
func (s *segStream) finish() ([]byte, error) {
	nRows, nCols := len(s.rowids), s.nCols
	if nRows == 0 || nCols == 0 {
		return nil, fmt.Errorf("segment: %d rows x %d columns is not a segment", nRows, nCols)
	}
	if nRows > 1<<24 {
		return nil, fmt.Errorf("segment: %d rows exceeds the 2^24 a segment may hold", nRows)
	}
	phys := make([]PhysicalType, nCols)
	off32 := make([]bool, nCols)
	for c := range phys {
		phys[c], _ = PlanColumn(s.cols[c].census)
		if phys[c] == PhysTagged {
			phys[c] = PhysNull // every value an exception, as buildSegmentWith does
		}
		off32[c] = phys[c] == PhysText || phys[c] == PhysBlob
	}
	dense := true
	for i := 1; i < nRows && dense; i++ {
		dense = s.rowids[i] == s.rowids[0]+uint64(i)
	}

	// The heap's size, column after column: a column whose values all fit its
	// type is its stream heap, except a BLOB's 4-byte alignment.
	heapLen := 0
	for c := 0; c < nCols; c++ {
		if !off32[c] {
			continue
		}
		col := &s.cols[c]
		want := Text
		if phys[c] == PhysBlob {
			want = Blob
		}
		for i := range col.typ {
			if ValueType(col.typ[i]) != want {
				continue
			}
			n := int(col.cell[i] >> 32)
			if want == Blob {
				heapLen = segAlignBlob(heapLen) + n
			} else {
				heapLen += n
			}
		}
	}

	nullBytes := (nRows + 7) / 8
	rowidOff := segAlign8(segHeaderSize + nCols*segColDirSize)
	rowidBytes := nRows * segCellWidth
	if dense {
		rowidBytes = segCellWidth
	}
	blocksOff := segAlign8(rowidOff + rowidBytes) // full width: no width block
	colDir := make([]segColumn, nCols)
	off := blocksOff
	for c := 0; c < nCols; c++ {
		colDir[c].phys = phys[c]
		switch {
		case phys[c] == PhysNull:
		case off32[c]:
			colDir[c].blockOff = uint32(off)
			off += segAlign8(4 * (nRows + 1))
		default:
			colDir[c].blockOff = uint32(off)
			off += nRows * segCellWidth
		}
	}
	blockEnd := off
	heapOff := blockEnd
	for c := 0; c < nCols; c++ {
		if s.cols[c].census.Null != 0 {
			heapOff += nullBytes
		}
	}
	heapOff = segAlign8(heapOff)

	out := make([]byte, blockEnd, heapOff+heapLen+64)
	heap := make([]byte, 0, heapLen)
	nulls := make([][]byte, nCols)
	var excBuf []byte
	excBuf = binary.LittleEndian.AppendUint32(excBuf, 0) // the count, filled in below
	nExc := 0
	for c := 0; c < nCols; c++ {
		col := &s.cols[c]
		if col.census.Null != 0 {
			nulls[c] = make([]byte, nullBytes)
		}
		var ends []byte
		if off32[c] {
			ends = out[colDir[c].blockOff:]
			binary.LittleEndian.PutUint32(ends, uint32(len(heap)))
		}
		base := int(colDir[c].blockOff)
		for i := 0; i < nRows; i++ {
			t := ValueType(col.typ[i] &^ streamIntReal)
			switch {
			case t == Null:
				nulls[c][i/8] |= 1 << uint(i%8)
			case phys[c] == PhysInt64 && t == Int, phys[c] == PhysFloat64 && t == Float:
				binary.LittleEndian.PutUint64(out[base+i*segCellWidth:], col.cell[i])
			case phys[c] == PhysText && t == Text, phys[c] == PhysBlob && t == Blob:
				if t == Blob {
					for len(heap) < segAlignBlob(len(heap)) {
						heap = append(heap, 0)
					}
				}
				o, n := uint32(col.cell[i]), uint32(col.cell[i]>>32)
				heap = append(heap, col.heap[o:o+n]...)
			default:
				rec := encodeRecordEnc([]Value{col.value(i)}, UTF8)
				excBuf = binary.LittleEndian.AppendUint32(excBuf, uint32(i))
				excBuf = binary.LittleEndian.AppendUint32(excBuf, uint32(c))
				excBuf = binary.LittleEndian.AppendUint32(excBuf, uint32(len(rec)))
				excBuf = append(excBuf, rec...)
				nExc++
			}
			if ends != nil {
				binary.LittleEndian.PutUint32(ends[4*(i+1):], uint32(len(heap)))
			}
		}
	}
	binary.LittleEndian.PutUint32(excBuf, uint32(nExc))

	if dense {
		binary.LittleEndian.PutUint64(out[rowidOff:], s.rowids[0])
	} else {
		for i, rid := range s.rowids {
			binary.LittleEndian.PutUint64(out[rowidOff+i*segCellWidth:], rid)
		}
	}
	for c := 0; c < nCols; c++ {
		if nulls[c] == nil {
			continue
		}
		colDir[c].nullOff = uint32(len(out))
		out = append(out, nulls[c]...)
	}
	for len(out)%8 != 0 {
		out = append(out, 0)
	}
	if len(out) != heapOff {
		return nil, fmt.Errorf("segment: heap at %d, laid out at %d", len(out), heapOff)
	}
	out = append(out, heap...)
	excOff := uint32(len(out))
	out = slices.Grow(out, len(excBuf))
	out = append(out, excBuf...)

	flags := uint32(segFlagLittleEndian | segFlagFullWidth)
	if dense {
		flags |= segFlagDenseRowids
	}
	copy(out[0:], segMagic)
	binary.LittleEndian.PutUint32(out[4:], flags)
	binary.LittleEndian.PutUint32(out[8:], uint32(nRows))
	binary.LittleEndian.PutUint32(out[12:], uint32(nCols))
	binary.LittleEndian.PutUint32(out[16:], uint32(heapOff))
	binary.LittleEndian.PutUint32(out[20:], uint32(len(heap)))
	binary.LittleEndian.PutUint32(out[24:], excOff)
	binary.LittleEndian.PutUint32(out[28:], uint32(len(excBuf)))
	binary.LittleEndian.PutUint32(out[32:], uint32(rowidOff))
	binary.LittleEndian.PutUint32(out[36:], 0) // no width block
	for c := 0; c < nCols; c++ {
		d := segHeaderSize + c*segColDirSize
		out[d] = byte(colDir[c].phys)
		if off32[c] {
			out[d+1] = segCellBounds
		}
		binary.LittleEndian.PutUint32(out[d+4:], colDir[c].blockOff)
		binary.LittleEndian.PutUint32(out[d+8:], colDir[c].nullOff)
		binary.LittleEndian.PutUint32(out[d+12:], colDir[c].idxOff)
	}
	return out, nil
}
