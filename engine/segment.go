package engine

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// A columnar segment: each column in its own contiguous fixed-width block.
// Column c of row i lives at blockOff[c] + i*8 -- one shift and one add from
// a base pointer. An int64 column can be handed to Go as a plain []int64.
//
// The layout, all integers little-endian:
//
//	header             40 bytes
//	column directory   16 bytes per column
//	rowid block        8*nRows, the keys these rows are stored under
//	width block        2*nRows, how many columns each row actually STORED
//	blocks             8*nRows each, 8-byte aligned, in column order
//	NULL bitmaps       ceil(nRows/8) each, only for columns that have NULLs
//	heap               TEXT and BLOB bytes, referenced by (offset,length) pairs
//	exceptions         the values that did not fit their column's physical type
//
// Fixed width is 8 bytes for every physical type, deliberately: a uniform
// stride means every block is alignable at the same granularity and generated
// code shifts by a constant instead of multiplying by a loaded width. TEXT and
// BLOB store (uint32 offset, uint32 length) into the heap, so a scan that never
// reads the text never touches its pages.
const (
	segMagic      = "MQS1"
	segHeaderSize = 40
	segColDirSize = 16
	segCellWidth  = 8
)

// segFlagLittleEndian marks the payload's byte order. The format is
// little-endian ALWAYS -- the alternative, native order, makes a file
// non-portable between machines and forfeits the interchange property that is
// the entire reason a converter exists. s390x, the one big-endian target in
// make build_all_targets, therefore byte-swaps and keeps a correct slower path.
const segFlagLittleEndian = 1 << 0

// segLittleEndian reports whether this machine's byte order matches the
// format's, which is what decides whether a block can be reinterpreted as a Go
// slice or has to be read a value at a time.
var segLittleEndian = func() bool {
	var x uint16 = 1
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

type segColumn struct {
	phys     PhysicalType
	blockOff uint32
	nullOff  uint32 // 0 when the column has no NULL
	// idxOff is this column's on-disk equality index, 0 when it has none. It
	// lives in the column directory's spare 4 bytes, which nothing wrote and
	// nothing read, so a file carrying one is still readable by a build that
	// knows nothing about it. See segment_index_ondisk.go.
	idxOff uint32
}

// segment is a built segment, read back.
type segment struct {
	buf   []byte
	nRows int
	cols  []segColumn
	heap     []byte
	rowidOff uint32
	widthOff uint32
	exc      map[uint64]Value // row*nCols+col -> the value that did not fit

	// diskIdx is this segment's equality indexes, read out of the FILE: a view
	// over the mapped bytes, opened once per column and holding no data of its
	// own. There is no in-memory alternative, deliberately -- see
	// segment_eqindex.go.
	eqMu    eqCountMu
	diskIdx map[int]*segDiskIndex

	// Lazily built NULL indicators, one per column: a 0/1 int64 block the JIT
	// can read where the bitmap cannot. See segment_nullidx.go.
	nullMu  eqCountMu
	nullIdx map[int][]int64

	// Lazily built order-preserving prefixes of a TEXT/BLOB column, one uint64
	// per row. See segment_prefix.go.
	prefMu  eqCountMu
	prefIdx map[int][]uint64
	// Lazily built zone maps, one per int64 column. See segment_zone.go.
	zoneMu eqCountMu
	zones  map[int]*segZones
}

// Rowid is the key row i is stored under, which a converter must preserve
// exactly: a rowid is user-visible (SELECT rowid), it is what every index
// entry points at, and it decides the b-tree order a round trip has to
// reproduce byte for byte.
func (s *segment) Rowid(i int) uint64 {
	return binary.LittleEndian.Uint64(s.buf[int(s.rowidOff)+i*segCellWidth:])
}

// Width is how many columns row i actually stored, which is not always the
// table's column count: a row written before an ALTER TABLE ADD COLUMN is
// short, and its missing columns read as the column DEFAULT rather than NULL.
// A scan must hand back exactly this many, as a b-tree walk does.
func (s *segment) Width(i int) int {
	return int(binary.LittleEndian.Uint16(s.buf[int(s.widthOff)+i*2:]))
}

// buildSegment encodes rows into one segment. cols is the table's column list,
// which decides how many columns each row contributes; a row shorter than that
// (written before an ALTER TABLE ADD COLUMN) contributes NULL for the columns
// it predates, which is what reading it back through the b-tree yields.
func buildSegment(cols []columnInfo, rowids []uint64, rows [][]Value) ([]byte, error) {
	nRows, nCols := len(rows), len(cols)
	if len(rowids) != nRows {
		return nil, fmt.Errorf("segment: %d rowids for %d rows", len(rowids), nRows)
	}
	if nRows == 0 || nCols == 0 {
		return nil, fmt.Errorf("segment: %d rows x %d columns is not a segment", nRows, nCols)
	}
	if nRows > 1<<24 {
		return nil, fmt.Errorf("segment: %d rows exceeds the 2^24 a segment may hold", nRows)
	}

	// Census first, then a physical type per column: the same decision
	// PlanColumn makes for the corpus census, made here over this segment's own
	// rows rather than the whole table's.
	census := make([]ColumnTypeCensus, nCols)
	for _, r := range rows {
		for c := 0; c < nCols; c++ {
			v := Value{Typ: Null}
			if c < len(r) {
				v = r[c]
			}
			switch v.Typ {
			case Null:
				census[c].Null++
			case Int:
				census[c].Int++
			case Float:
				census[c].Real++
			case Text:
				census[c].Text++
			case Blob:
				census[c].Blob++
			}
		}
	}
	phys := make([]PhysicalType, nCols)
	for c := range phys {
		phys[c], _ = PlanColumn(census[c])
		if phys[c] == PhysTagged {
			// Stage 2 stores a tagged column as exceptions in their entirety,
			// which is correct and slow -- exactly what the fallback promises.
			// A dedicated tagged encoding is stage 4's problem.
			phys[c] = PhysNull
		}
	}

	// The heap's exact size, so it is one allocation rather than a doubling
	// series of discarded buffers.
	heapLen := 0
	for _, r := range rows {
		for c := 0; c < nCols && c < len(r); c++ {
			if v := r[c]; phys[c] == PhysText && v.Typ == Text {
				heapLen += len(v.S)
			} else if phys[c] == PhysBlob && v.Typ == Blob {
				heapLen = segAlignBlob(heapLen) + len(v.S)
			}
		}
	}

	nullBytes := (nRows + 7) / 8
	rowidOff := segAlign8(segHeaderSize + nCols*segColDirSize)
	widthOff := rowidOff + nRows*segCellWidth
	blocksOff := segAlign8(widthOff + nRows*2)

	var (
		body    []byte
		colDir  = make([]segColumn, nCols)
		nulls   = make([][]byte, nCols)
		heap    []byte
		excRows []uint32
		excCols []uint32
		excVals []Value
	)
	off := blocksOff
	for c := 0; c < nCols; c++ {
		colDir[c].phys = phys[c]
		if phys[c] == PhysNull {
			colDir[c].blockOff = 0
		} else {
			colDir[c].blockOff = uint32(off)
			off += nRows * segCellWidth
		}
		nulls[c] = make([]byte, nullBytes)
	}
	blockBytes := off - blocksOff
	body = make([]byte, blockBytes)
	heap = make([]byte, 0, heapLen)

	put := func(c, i int, x uint64) {
		base := int(colDir[c].blockOff) - blocksOff + i*segCellWidth
		binary.LittleEndian.PutUint64(body[base:], x)
	}
	for i, r := range rows {
		for c := 0; c < nCols; c++ {
			v := Value{Typ: Null}
			if c < len(r) {
				v = r[c]
			}
			if v.Typ == Null {
				nulls[c][i/8] |= 1 << uint(i%8)
				continue
			}
			switch {
			case phys[c] == PhysInt64 && v.Typ == Int:
				put(c, i, uint64(v.I))
			case phys[c] == PhysFloat64 && v.Typ == Float:
				put(c, i, floatBits(v.F))
			case (phys[c] == PhysText && v.Typ == Text) || (phys[c] == PhysBlob && v.Typ == Blob):
				if phys[c] == PhysBlob {
					for len(heap) < segAlignBlob(len(heap)) {
						heap = append(heap, 0)
					}
				}
				o := uint32(len(heap))
				heap = append(heap, v.S...)
				put(c, i, uint64(o)|uint64(len(v.S))<<32)
			default:
				// Did not fit: the value goes to the side list and its cell is
				// left zero. The NULL bit stays CLEAR, so a reader that finds a
				// non-NULL cell with no fixed-width meaning knows to look here.
				excRows = append(excRows, uint32(i))
				excCols = append(excCols, uint32(c))
				excVals = append(excVals, v)
			}
		}
	}

	// THE EXCEPTION LIST AND THE EQUALITY INDEXES are built before the output,
	// from body and heap, so the output is ONE allocation of its exact size. They
	// used to be appended past a capacity sized without them, and each overflow
	// copied the whole segment, payload included: compacting a delta of 10 KB
	// blobs allocated 8x its size in buildSegment, most of a 5 GB peak.
	var excBuf []byte
	excBuf = binary.LittleEndian.AppendUint32(excBuf, uint32(len(excVals)))
	for k, v := range excVals {
		rec := encodeRecordEnc([]Value{v}, UTF8)
		excBuf = binary.LittleEndian.AppendUint32(excBuf, excRows[k])
		excBuf = binary.LittleEndian.AppendUint32(excBuf, excCols[k])
		excBuf = binary.LittleEndian.AppendUint32(excBuf, uint32(len(rec)))
		excBuf = append(excBuf, rec...)
	}

	// THE EQUALITY INDEXES, one per column that can have one, laid out last so
	// every offset before them is already final -- see segment_index_ondisk.go
	// for why they are in the file at all, built here because this is the one
	// moment the whole column is in hand.
	//
	// A column qualifies when every one of its values is a non-NULL fixed-width
	// cell: no NULL bitmap and no exception (a value that did not fit). EVERY
	// FIXED-WIDTH PHYSICAL TYPE, not just the integer one: a TEXT or BLOB cell is
	// a packed (heapOffset, length) uint64 and a float is its bits, so all three
	// are the same 8-byte lane and only the ORDER differs, which is what the
	// codec names. Restricting this to PhysInt64 left TEXT, REAL and BLOB with no
	// index at all, measured against C SQLite at 5360x, 7973x and 7760x.
	excCol := make([]bool, nCols)
	for _, c := range excCols {
		if int(c) < nCols {
			excCol[c] = true
		}
	}
	idx := make([][]byte, nCols)
	for c := 0; c < nCols; c++ {
		if census[c].Null != 0 || excCol[c] || nRows == 0 || phys[c] == PhysNull {
			continue
		}
		base := int(colDir[c].blockOff) - blocksOff
		cells := make([]int64, nRows)
		for i := 0; i < nRows; i++ {
			cells[i] = int64(binary.LittleEndian.Uint64(body[base+i*segCellWidth:]))
		}
		var blk []byte
		var ok bool
		switch phys[c] {
		case PhysInt64:
			blk, ok = buildSegColumnIndex(cells)
		case PhysText, PhysBlob:
			blk, ok = buildSegColumnIndexBytes(cells, heap) // the cells address the heap
		case PhysFloat64:
			blk, ok = buildSegColumnIndexFloat(cells)
		}
		if ok {
			idx[c] = blk
		}
	}

	// The layout after the blocks: NULL bitmaps, the heap, the exception list,
	// then each index 8-ALIGNED, so the record's own int64 value array lands
	// 8-aligned too (its header is 16 bytes) -- a little-endian host can then
	// reinterpret rather than decode.
	size := blocksOff + len(body)
	for c := 0; c < nCols; c++ {
		if census[c].Null != 0 {
			size += nullBytes
		}
	}
	size = segAlign8(size) + len(heap) + len(excBuf)
	for c := 0; c < nCols; c++ {
		if idx[c] != nil {
			size = segAlign8(size) + len(idx[c])
		}
	}
	out := make([]byte, blocksOff, size)
	for i, rid := range rowids {
		binary.LittleEndian.PutUint64(out[rowidOff+i*segCellWidth:], uint64(rid))
		// How many columns this row actually STORED. A row written before an
		// ALTER TABLE ADD COLUMN is short, and the reader pads it with the
		// COLUMN DEFAULTS (padStoredRow), not with NULL -- so a segment that
		// forgot the width would turn a defaulted column into a NULL one.
		w := len(rows[i])
		if w > nCols {
			w = nCols
		}
		binary.LittleEndian.PutUint16(out[widthOff+i*2:], uint16(w))
	}
	out = append(out, body...)
	for c := 0; c < nCols; c++ {
		if census[c].Null == 0 {
			continue
		}
		colDir[c].nullOff = uint32(len(out))
		out = append(out, nulls[c]...)
	}
	for len(out)%8 != 0 { // the heap starts aligned, so its aligned cells are
		out = append(out, 0)
	}
	heapOff := uint32(len(out))
	out = append(out, heap...)
	excOff := uint32(len(out))
	out = append(out, excBuf...)
	for c := 0; c < nCols; c++ {
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
	binary.LittleEndian.PutUint32(out[4:], segFlagLittleEndian)
	binary.LittleEndian.PutUint32(out[8:], uint32(nRows))
	binary.LittleEndian.PutUint32(out[12:], uint32(nCols))
	binary.LittleEndian.PutUint32(out[16:], heapOff)
	binary.LittleEndian.PutUint32(out[20:], uint32(len(heap)))
	binary.LittleEndian.PutUint32(out[24:], excOff)
	binary.LittleEndian.PutUint32(out[28:], uint32(len(excBuf)))
	binary.LittleEndian.PutUint32(out[32:], uint32(rowidOff))
	binary.LittleEndian.PutUint32(out[36:], uint32(widthOff))
	for c := 0; c < nCols; c++ {
		d := segHeaderSize + c*segColDirSize
		out[d] = byte(colDir[c].phys)
		binary.LittleEndian.PutUint32(out[d+4:], colDir[c].blockOff)
		binary.LittleEndian.PutUint32(out[d+8:], colDir[c].nullOff)
		binary.LittleEndian.PutUint32(out[d+12:], colDir[c].idxOff)
	}
	return out, nil
}

func segAlign8(n int) int { return (n + 7) &^ 7 }

func floatBits(f float64) uint64 { return *(*uint64)(unsafe.Pointer(&f)) }
func bitsFloat(u uint64) float64 { return *(*float64)(unsafe.Pointer(&u)) }

// openSegment reads a built segment back. It validates every offset it will
// later index by, so the accessors below can be branch-light: a malformed
// segment is an error here, never a panic in a scan.
func openSegment(buf []byte) (*segment, error) {
	if len(buf) < segHeaderSize || string(buf[:4]) != segMagic {
		return nil, fmt.Errorf("segment: not a segment")
	}
	if binary.LittleEndian.Uint32(buf[4:])&segFlagLittleEndian == 0 {
		return nil, fmt.Errorf("segment: unknown byte order")
	}
	nRows := int(binary.LittleEndian.Uint32(buf[8:]))
	nCols := int(binary.LittleEndian.Uint32(buf[12:]))
	heapOff := binary.LittleEndian.Uint32(buf[16:])
	heapLen := binary.LittleEndian.Uint32(buf[20:])
	excOff := binary.LittleEndian.Uint32(buf[24:])
	excLen := binary.LittleEndian.Uint32(buf[28:])
	rowidOff := binary.LittleEndian.Uint32(buf[32:])
	widthOff := binary.LittleEndian.Uint32(buf[36:])
	if nCols == 0 || len(buf) < segHeaderSize+nCols*segColDirSize {
		return nil, fmt.Errorf("segment: column directory does not fit")
	}
	if int(heapOff)+int(heapLen) > len(buf) || int(excOff)+int(excLen) > len(buf) {
		return nil, fmt.Errorf("segment: heap or exception list overruns the buffer")
	}
	if int(rowidOff)+nRows*segCellWidth > len(buf) || rowidOff%segCellWidth != 0 {
		return nil, fmt.Errorf("segment: rowid block is unaligned or overruns")
	}
	if int(widthOff)+nRows*2 > len(buf) {
		return nil, fmt.Errorf("segment: width block overruns")
	}
	s := &segment{buf: buf, nRows: nRows, cols: make([]segColumn, nCols),
		heap: buf[heapOff : heapOff+heapLen], rowidOff: rowidOff, widthOff: widthOff}
	nullBytes := (nRows + 7) / 8
	for c := 0; c < nCols; c++ {
		d := segHeaderSize + c*segColDirSize
		col := segColumn{
			phys:     PhysicalType(buf[d]),
			blockOff: binary.LittleEndian.Uint32(buf[d+4:]),
			nullOff:  binary.LittleEndian.Uint32(buf[d+8:]),
			idxOff:   binary.LittleEndian.Uint32(buf[d+12:]),
		}
		if col.phys.Width() != 0 {
			if int(col.blockOff)+nRows*segCellWidth > len(buf) || col.blockOff%segCellWidth != 0 {
				return nil, fmt.Errorf("segment: column %d block is unaligned or overruns", c)
			}
		}
		if col.nullOff != 0 && int(col.nullOff)+nullBytes > len(buf) {
			return nil, fmt.Errorf("segment: column %d NULL bitmap overruns", c)
		}
		// The index offset is BOUNDED here, though the record is validated lazily
		// (segment_index_ondisk.go): an offset past the end, or one that cannot
		// hold a header, is a malformed segment rather than a column without an
		// index, and saying so here beats discovering it at the first query.
		if col.idxOff != 0 {
			if col.idxOff%8 != 0 || int(col.idxOff)+segIdxHdr > len(buf) {
				return nil, fmt.Errorf("segment: column %d index offset is unaligned or overruns", c)
			}
			// ...AND THE RECORD'S OWN DECLARED LENGTH FITS. The index is the LAST
			// thing in a segment, so without this a truncated file opens cleanly --
			// every other block's bounds check is satisfied and the damage is only
			// visible to the lazy reader. TestSegmentRejectsMalformed's one-byte
			// truncation caught exactly that. Cheap, because the record carries its
			// length for this reason.
			if n := binary.LittleEndian.Uint32(buf[int(col.idxOff)+12:]); n < segIdxHdr ||
				int(col.idxOff)+int(n) > len(buf) {
				return nil, fmt.Errorf("segment: column %d index record overruns", c)
			}
		}
		s.cols[c] = col
	}
	if excLen >= 4 {
		exc := buf[excOff : excOff+excLen]
		n := int(binary.LittleEndian.Uint32(exc))
		s.exc = make(map[uint64]Value, n)
		p := 4
		for k := 0; k < n; k++ {
			if p+12 > len(exc) {
				return nil, fmt.Errorf("segment: exception %d header overruns", k)
			}
			row := binary.LittleEndian.Uint32(exc[p:])
			col := binary.LittleEndian.Uint32(exc[p+4:])
			ln := int(binary.LittleEndian.Uint32(exc[p+8:]))
			p += 12
			if p+ln > len(exc) {
				return nil, fmt.Errorf("segment: exception %d value overruns", k)
			}
			vals, derr := decodeRecordEnc(exc[p:p+ln], UTF8)
			if derr != nil || len(vals) != 1 {
				return nil, fmt.Errorf("segment: exception %d is not one value: %v", k, derr)
			}
			p += ln
			if int(row) >= nRows || int(col) >= nCols {
				return nil, fmt.Errorf("segment: exception %d names row %d column %d, out of range", k, row, col)
			}
			s.exc[uint64(row)*uint64(nCols)+uint64(col)] = vals[0]
		}
	}
	return s, nil
}

// isNull reports whether column c of row i is NULL.
func (s *segment) isNull(c, i int) bool {
	off := s.cols[c].nullOff
	if off == 0 {
		return s.cols[c].phys == PhysNull && s.exc == nil
	}
	return s.buf[int(off)+i/8]&(1<<uint(i%8)) != 0
}

// Int64Column hands back a fixed-width int64 block as a Go slice with NO decode
// and NO copy -- the whole point of the format. ok is false when the column is
// not stored as int64, when this machine's byte order is not the format's, or
// when the block is not 8-byte aligned in the buffer; every caller must have a
// path for that, because three of the seventeen build targets can hit it.
//
// The returned slice aliases the segment's buffer. Its values are meaningless
// for rows where isNull or an exception says otherwise, which is the caller's
// business: a predicate over a NOT NULL, exception-free column -- the case this
// exists for -- can read it straight through.
func (s *segment) Int64Column(c int) ([]int64, bool) {
	if c < 0 || c >= len(s.cols) || s.cols[c].phys != PhysInt64 || !segLittleEndian {
		return nil, false
	}
	off := int(s.cols[c].blockOff)
	if off == 0 || uintptr(unsafe.Pointer(&s.buf[off]))%8 != 0 {
		return nil, false
	}
	return unsafe.Slice((*int64)(unsafe.Pointer(&s.buf[off])), s.nRows), true
}

// Float64Column is Int64Column for a REAL column, and it needs no decode: a
// PhysFloat64 cell is written by the SAME put() into the SAME 8-byte fixed-width
// slot an integer uses (floatBits(v.F), buildSegment), so the block already IS a
// []float64.
//
// The NULL and exception gates are folded in here rather than left to the caller,
// because a caller reading this slice is reading raw cells and must not see a
// column where a cell does not mean what its physical type says.
func (s *segment) Float64Column(c int) ([]float64, bool) {
	if c < 0 || c >= len(s.cols) || s.cols[c].phys != PhysFloat64 || !segLittleEndian {
		return nil, false
	}
	if s.cols[c].nullOff != 0 || s.hasException(c) {
		return nil, false
	}
	off := int(s.cols[c].blockOff)
	if off == 0 || uintptr(unsafe.Pointer(&s.buf[off]))%8 != 0 {
		return nil, false
	}
	return unsafe.Slice((*float64)(unsafe.Pointer(&s.buf[off])), s.nRows), true
}

// BytesColumn is Int64Column for a TEXT or BLOB column: the packed cells as a
// []uint64 -- heap OFFSET in the low 32 bits, LENGTH in the high 32, exactly what
// buildSegment's put() wrote -- plus the heap they address.
//
// The block is flat and 8-byte just like an integer column's; what is different
// is only that the cell addresses the value instead of being it. That makes a
// scan over it a candidate for the same treatment: one contiguous read of cells,
// and the heap touched only where a decision needs the bytes.
//
// Same NULL and exception gates as the other two accessors, for the same reason.
func (s *segment) BytesColumn(c int) (cells []uint64, heap []byte, ok bool) {
	if c < 0 || c >= len(s.cols) || !segLittleEndian {
		return nil, nil, false
	}
	if ph := s.cols[c].phys; ph != PhysText && ph != PhysBlob {
		return nil, nil, false
	}
	if s.cols[c].nullOff != 0 || s.hasException(c) {
		return nil, nil, false
	}
	off := int(s.cols[c].blockOff)
	if off == 0 || uintptr(unsafe.Pointer(&s.buf[off]))%8 != 0 {
		return nil, nil, false
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&s.buf[off])), s.nRows), s.heap, true
}

// Value is the general accessor: the value of column c in row i, whatever it
// took to store it. Correct for every column and every physical type, and the
// path a scan takes when Int64Column declines.
func (s *segment) Value(c, i int) Value {
	if i < 0 || i >= s.nRows || c < 0 || c >= len(s.cols) {
		return Value{Typ: Null}
	}
	if s.cols[c].nullOff != 0 && s.buf[int(s.cols[c].nullOff)+i/8]&(1<<uint(i%8)) != 0 {
		return Value{Typ: Null}
	}
	if v, ok := s.exc[uint64(i)*uint64(len(s.cols))+uint64(c)]; ok {
		return v
	}
	col := s.cols[c]
	if col.phys.Width() == 0 {
		return Value{Typ: Null}
	}
	raw := binary.LittleEndian.Uint64(s.buf[int(col.blockOff)+i*segCellWidth:])
	switch col.phys {
	case PhysInt64:
		return Value{Typ: Int, I: int64(raw)}
	case PhysFloat64:
		return Value{Typ: Float, F: bitsFloat(raw)}
	default:
		o, n := uint32(raw), uint32(raw>>32)
		if int(o)+int(n) > len(s.heap) {
			return Value{Typ: Null}
		}
		b := s.heap[o : o+n : o+n]
		if col.phys == PhysText {
			return Value{Typ: Text, S: b}
		}
		return Value{Typ: Blob, S: b}
	}
}

// segAlignBlob is where a BLOB cell starts in the heap: 4-aligned, so a vector
// column's cells read as []float32 in place (segment_vector.go). TEXT cells
// stay packed. The heap itself starts 8-aligned.
func segAlignBlob(n int) int { return (n + 3) &^ 3 }
