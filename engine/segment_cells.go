package engine

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// buildSegmentCells is buildSegmentWith for rows whose every column holds
// only INTEGERs or only non-NULL REALs, given as each column's cells (an
// int64, or a double's bits), with the rowids first, first+1, ...: the same
// bytes, without a Value per cell. real[c] says which columns are REAL.
func buildSegmentCells(cols []columnInfo, first uint64, cells [][]uint64, real []bool) ([]byte, error) {
	nCols := len(cols)
	if nCols == 0 || len(cells) != nCols || len(real) != nCols {
		return nil, fmt.Errorf("segment: %d column blocks for %d columns", len(cells), nCols)
	}
	nRows := len(cells[0])
	for _, c := range cells {
		if len(c) != nRows {
			return nil, fmt.Errorf("segment: column blocks of %d and %d rows", nRows, len(c))
		}
	}
	if nRows == 0 || nRows > 1<<24 {
		return nil, fmt.Errorf("segment: %d rows is not a segment", nRows)
	}
	phys := make([]PhysicalType, nCols)
	for c := range phys {
		phys[c] = PhysInt64
		if real[c] {
			phys[c] = PhysFloat64
		}
	}
	rowidOff := segAlign8(segHeaderSize + nCols*segColDirSize)
	blocksOff := segAlign8(rowidOff + segCellWidth)
	colDir := make([]segColumn, nCols)
	off := blocksOff
	for c := range colDir {
		colDir[c].phys = phys[c]
		colDir[c].blockOff = uint32(off)
		off += nRows * segCellWidth
	}
	blockEnd := off
	idx := make([][]byte, nCols)
	size := blockEnd + 4 // the empty exception list
	for c := range idx {
		vals := unsafe.Slice((*int64)(unsafe.Pointer(unsafe.SliceData(cells[c]))), nRows)
		var ok bool
		if real[c] {
			idx[c], ok = buildSegColumnIndexFloat(vals)
		} else {
			idx[c], ok = buildSegColumnIndex(vals)
		}
		if !ok {
			idx[c] = nil
			continue
		}
		size = segAlign8(size) + len(idx[c])
	}
	out := make([]byte, blockEnd, size)
	binary.LittleEndian.PutUint64(out[rowidOff:], first)
	for c := range cells {
		base := int(colDir[c].blockOff)
		for i, x := range cells[c] {
			binary.LittleEndian.PutUint64(out[base+i*segCellWidth:], x)
		}
	}
	excOff := uint32(len(out))
	out = binary.LittleEndian.AppendUint32(out, 0)
	for c := range idx {
		if idx[c] == nil {
			continue
		}
		for len(out)%8 != 0 {
			out = append(out, 0)
		}
		colDir[c].idxOff = uint32(len(out))
		out = append(out, idx[c]...)
	}
	copy(out[0:], segMagic)
	binary.LittleEndian.PutUint32(out[4:], segFlagLittleEndian|segFlagDenseRowids|segFlagFullWidth)
	binary.LittleEndian.PutUint32(out[8:], uint32(nRows))
	binary.LittleEndian.PutUint32(out[12:], uint32(nCols))
	binary.LittleEndian.PutUint32(out[16:], uint32(blockEnd))
	binary.LittleEndian.PutUint32(out[20:], 0)
	binary.LittleEndian.PutUint32(out[24:], excOff)
	binary.LittleEndian.PutUint32(out[28:], 4)
	binary.LittleEndian.PutUint32(out[32:], uint32(rowidOff))
	binary.LittleEndian.PutUint32(out[36:], 0)
	for c := range colDir {
		d := segHeaderSize + c*segColDirSize
		out[d] = byte(colDir[c].phys)
		binary.LittleEndian.PutUint32(out[d+4:], colDir[c].blockOff)
		binary.LittleEndian.PutUint32(out[d+12:], colDir[c].idxOff)
	}
	return out, nil
}
