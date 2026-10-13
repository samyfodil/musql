package engine

import (
	"encoding/binary"
	"fmt"
	"slices"
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
//	rowid block        8*nRows, the keys these rows are stored under; only the
//	                   first, 8 bytes, when they are consecutive (segFlagDenseRowids)
//	width block        2*nRows, how many columns each row actually STORED; absent
//	                   when every row stored all of them (segFlagFullWidth)
//	blocks             8*nRows each, 8-byte aligned, in column order -- or, for a
//	                   TEXT or BLOB column marked segCellBounds, nRows+1 uint32
//	                   heap boundaries, its values being laid out one after another
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

// segFlagDenseRowids and segFlagFullWidth say a block was left out because it
// held nothing a reader could not derive (buildSegmentInto). Neither is ever set
// in a segment of a file older than version 6, and a flag this build does not
// know refuses the segment rather than misreading it.
const (
	segFlagDenseRowids = 1 << 1
	segFlagFullWidth   = 1 << 2
	segFlagsKnown      = segFlagLittleEndian | segFlagDenseRowids | segFlagFullWidth
)

// segCellBounds, in a column directory entry's second byte, marks a TEXT or
// BLOB column stored as heap boundaries: row i's value starts at bound i
// (4-aligned first, for a BLOB) and ends at bound i+1. Zero is the 8-byte
// (offset, length) cell every other column and every older file has.
const segCellBounds = 1

// segLittleEndian reports whether this machine's byte order matches the
// format's, which is what decides whether a block can be reinterpreted as a Go
// slice or has to be read a value at a time.
var segLittleEndian = func() bool {
	var x uint16 = 1
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

type segColumn struct {
	phys     PhysicalType
	bounds   bool // segCellBounds: the block is nRows+1 uint32 heap boundaries
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
	// dense is segFlagDenseRowids: row i's rowid is firstRowid+i.
	dense      bool
	firstRowid uint64
	// fullWidth is segFlagFullWidth: every row stored every column.
	fullWidth bool
	exc       map[uint64]Value // row*nCols+col -> the value that did not fit

	// Lazily built 8-byte (offset, length) cells of a segCellBounds column, for
	// the readers that take a column's cells whole (BytesColumn).
	cellMu    eqCountMu
	cellCache map[int][]uint64

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
	// Lazily built int8 copies of vector columns, a nil entry for a column
	// that has none. See segment_vector_bound.go.
	vecMu       eqCountMu
	vecSides    map[int]*segVecSide
	vecSearches map[int]int // searches per column, until it has a sidecar
	vecGraphs   []vecGraphEntry // graphs over the table from this, its first segment
}

// Rowid is the key row i is stored under, which a converter must preserve
// exactly: a rowid is user-visible (SELECT rowid), it is what every index
// entry points at, and it decides the b-tree order a round trip has to
// reproduce byte for byte.
func (s *segment) Rowid(i int) uint64 {
	if s.dense {
		return s.firstRowid + uint64(i)
	}
	return binary.LittleEndian.Uint64(s.buf[int(s.rowidOff)+i*segCellWidth:])
}

// Width is how many columns row i actually stored, which is not always the
// table's column count: a row written before an ALTER TABLE ADD COLUMN is
// short, and its missing columns read as the column DEFAULT rather than NULL.
// A scan must hand back exactly this many, as a b-tree walk does.
func (s *segment) Width(i int) int {
	if s.fullWidth {
		return len(s.cols)
	}
	return int(binary.LittleEndian.Uint16(s.buf[int(s.widthOff)+i*2:]))
}

// buildSegment encodes rows into one segment. cols is the table's column list,
// which decides how many columns each row contributes; a row shorter than that
// (written before an ALTER TABLE ADD COLUMN) contributes NULL for the columns
// it predates, which is what reading it back through the b-tree yields.
func buildSegment(cols []columnInfo, rowids []uint64, rows [][]Value) ([]byte, error) {
	return buildSegmentInto(nil, cols, rowids, rows)
}

// buildSegmentInto is buildSegment building into dst's memory when it is
// large enough, for a caller that is done with the previous segment by the
// time it builds the next (segCutter.reuse).
func buildSegmentInto(dst []byte, cols []columnInfo, rowids []uint64, rows [][]Value) ([]byte, error) {
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

	// THREE THINGS A SEGMENT LEAVES OUT when it can (segment file version 6),
	// each recorded so a reader knows:
	//
	//   - the rowid block, when the rowids are consecutive -- what a load or a
	//     rewrite of one leaves: one 8-byte first rowid instead of 8 per row;
	//   - the width block, when every row stored every column: 2 bytes a row;
	//   - half of a TEXT or BLOB column's block: its values are laid out in the
	//     heap one COLUMN after another, so a value ends where the next row's
	//     begins and nRows+1 uint32 boundaries describe them all, where an 8-byte
	//     (offset, length) cell per row did.
	//
	// Measured on a 10M-row table of one short TEXT column, these and the
	// TEXT index's packed positions (segment_index_ondisk.go) took the file from
	// 47 to 20 bytes a row; C SQLite's b-tree holds the same rows in 23.
	dense := true
	for i := 1; i < nRows && dense; i++ {
		dense = rowids[i] == rowids[0]+uint64(i)
	}
	fullWidth := true
	for _, r := range rows {
		if len(r) < nCols {
			fullWidth = false
			break
		}
	}
	off32 := make([]bool, nCols)
	for c := range phys {
		off32[c] = phys[c] == PhysText || phys[c] == PhysBlob
	}

	// Each column's cells, in memory: the 8-byte cells the indexes are built
	// from (a TEXT cell is its (offset, length) pair), written to the file as
	// they are or, for TEXT and BLOB, as boundaries.
	cells := make([][]uint64, nCols)
	for c := range cells {
		if phys[c] != PhysNull {
			cells[c] = make([]uint64, nRows)
		}
	}
	// The heap's exact size, column after column, so it is one allocation.
	heapLen := 0
	for c := 0; c < nCols; c++ {
		if !off32[c] {
			continue
		}
		for _, r := range rows {
			if c >= len(r) {
				continue
			}
			if v := r[c]; phys[c] == PhysText && v.Typ == Text {
				heapLen += len(v.S)
			} else if phys[c] == PhysBlob && v.Typ == Blob {
				heapLen = segAlignBlob(heapLen) + len(v.S)
			}
		}
	}

	nullBytes := (nRows + 7) / 8
	rowidOff := segAlign8(segHeaderSize + nCols*segColDirSize)
	rowidBytes := nRows * segCellWidth
	if dense {
		rowidBytes = segCellWidth
	}
	widthOff, widthBytes := rowidOff+rowidBytes, nRows*2
	if fullWidth {
		widthBytes = 0
	}
	blocksOff := segAlign8(widthOff + widthBytes)
	if fullWidth {
		widthOff = 0
	}
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
	idxSlack := 0
	for c := 0; c < nCols; c++ {
		if census[c].Null != 0 {
			heapOff += nullBytes
		} else if phys[c] != PhysNull {
			idxSlack += 8 + segIdxHdr + 4*nRows + 8
		}
	}
	heapOff = segAlign8(heapOff)
	// THE HEAP IS WRITTEN WHERE IT ENDS UP: its offset is known before a value
	// is placed, so the output is allocated now and the payload copied into it
	// once. The capacity covers the exception list's and the indexes' worst
	// case too.
	var out []byte
	if want := heapOff + heapLen + 64 + idxSlack; cap(dst) >= want {
		out = dst[:blockEnd]
		clear(out)
	} else {
		out = make([]byte, blockEnd, want)
	}
	heap := out[heapOff : heapOff : heapOff+heapLen]

	var (
		nulls   = make([][]byte, nCols)
		excRows []uint32
		excCols []uint32
		excVals []Value
	)
	for c := 0; c < nCols; c++ {
		if census[c].Null != 0 {
			nulls[c] = make([]byte, nullBytes)
		}
		var ends []byte
		if off32[c] {
			ends = out[colDir[c].blockOff:]
			binary.LittleEndian.PutUint32(ends, uint32(len(heap)))
		}
		for i, r := range rows {
			v := Value{Typ: Null}
			if c < len(r) {
				v = r[c]
			}
			switch {
			case v.Typ == Null:
				nulls[c][i/8] |= 1 << uint(i%8)
			case phys[c] == PhysInt64 && v.Typ == Int:
				cells[c][i] = uint64(v.I)
			case phys[c] == PhysFloat64 && v.Typ == Float:
				cells[c][i] = floatBits(v.F)
			case (phys[c] == PhysText && v.Typ == Text) || (phys[c] == PhysBlob && v.Typ == Blob):
				if phys[c] == PhysBlob {
					for len(heap) < segAlignBlob(len(heap)) {
						heap = append(heap, 0)
					}
				}
				o := uint32(len(heap))
				heap = append(heap, v.S...)
				cells[c][i] = uint64(o) | uint64(len(v.S))<<32
			default:
				// Did not fit: the value goes to the side list and its cell is
				// left zero. The NULL bit stays CLEAR, so a reader that finds a
				// non-NULL cell with no fixed-width meaning knows to look here.
				excRows = append(excRows, uint32(i))
				excCols = append(excCols, uint32(c))
				excVals = append(excVals, v)
			}
			if ends != nil {
				// Where this row's value ends: where the next one starts, but for
				// a BLOB's 4-byte alignment, which the reader re-derives.
				binary.LittleEndian.PutUint32(ends[4*(i+1):], uint32(len(heap)))
			}
		}
		if !off32[c] && cells[c] != nil {
			base := int(colDir[c].blockOff)
			for i, x := range cells[c] {
				binary.LittleEndian.PutUint64(out[base+i*segCellWidth:], x)
			}
		}
	}

	// THE EXCEPTION LIST AND THE EQUALITY INDEXES are built before the rest of
	// the output, so it is ONE allocation of its exact size.
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
	// PHYSICAL TYPE, not just the integer one: restricting this to PhysInt64
	// left TEXT, REAL and BLOB with no index at all, measured against C SQLite
	// at 5360x, 7973x and 7760x.
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
		vals := unsafe.Slice((*int64)(unsafe.Pointer(unsafe.SliceData(cells[c]))), nRows)
		var blk []byte
		var ok bool
		switch phys[c] {
		case PhysInt64:
			blk, ok = buildSegColumnIndex(vals)
		case PhysText, PhysBlob:
			blk, ok = buildSegColumnIndexBytesPerm(vals, heap) // the cells address the heap
		case PhysFloat64:
			blk, ok = buildSegColumnIndexFloat(vals)
		}
		if ok {
			idx[c] = blk
		}
	}

	// The layout after the blocks: NULL bitmaps, the heap, the exception list,
	// then each index 8-ALIGNED.
	size := blockEnd
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
	if cap(out) < size {
		out = slices.Grow(out, size-len(out))
	}
	if dense {
		binary.LittleEndian.PutUint64(out[rowidOff:], rowids[0])
	} else {
		for i, rid := range rowids {
			binary.LittleEndian.PutUint64(out[rowidOff+i*segCellWidth:], rid)
		}
	}
	if !fullWidth {
		for i := range rowids {
			// How many columns this row actually STORED. A row written before an
			// ALTER TABLE ADD COLUMN is short, and the reader pads it with the
			// COLUMN DEFAULTS (padStoredRow), not with NULL -- so a segment that
			// forgot the width would turn a defaulted column into a NULL one.
			binary.LittleEndian.PutUint16(out[widthOff+i*2:], uint16(min(len(rows[i]), nCols)))
		}
	}
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
	if len(out) != heapOff {
		return nil, fmt.Errorf("segment: heap at %d, laid out at %d", len(out), heapOff)
	}
	if len(heap) > 0 && unsafe.SliceData(heap) == unsafe.SliceData(out[heapOff:cap(out)]) {
		out = out[:heapOff+len(heap)] // already in place
	} else {
		out = append(out, heap...)
	}
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

	flags := uint32(segFlagLittleEndian)
	if dense {
		flags |= segFlagDenseRowids
	}
	if fullWidth {
		flags |= segFlagFullWidth
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
	binary.LittleEndian.PutUint32(out[36:], uint32(widthOff))
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
	flags := binary.LittleEndian.Uint32(buf[4:])
	if flags&segFlagLittleEndian == 0 {
		return nil, fmt.Errorf("segment: unknown byte order")
	}
	if flags&^segFlagsKnown != 0 {
		return nil, fmt.Errorf("segment: unknown flags %#x", flags)
	}
	dense, fullWidth := flags&segFlagDenseRowids != 0, flags&segFlagFullWidth != 0
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
	rowidBytes := nRows * segCellWidth
	if dense {
		rowidBytes = segCellWidth
	}
	if int(rowidOff)+rowidBytes > len(buf) || rowidOff%segCellWidth != 0 {
		return nil, fmt.Errorf("segment: rowid block is unaligned or overruns")
	}
	if !fullWidth && int(widthOff)+nRows*2 > len(buf) {
		return nil, fmt.Errorf("segment: width block overruns")
	}
	s := &segment{buf: buf, nRows: nRows, cols: make([]segColumn, nCols),
		heap: buf[heapOff : heapOff+heapLen], rowidOff: rowidOff, widthOff: widthOff,
		dense: dense, fullWidth: fullWidth}
	if dense {
		s.firstRowid = binary.LittleEndian.Uint64(buf[rowidOff:])
	}
	nullBytes := (nRows + 7) / 8
	for c := 0; c < nCols; c++ {
		d := segHeaderSize + c*segColDirSize
		col := segColumn{
			phys:     PhysicalType(buf[d]),
			bounds:   buf[d+1] == segCellBounds,
			blockOff: binary.LittleEndian.Uint32(buf[d+4:]),
			nullOff:  binary.LittleEndian.Uint32(buf[d+8:]),
			idxOff:   binary.LittleEndian.Uint32(buf[d+12:]),
		}
		switch {
		case buf[d+1] > segCellBounds, col.bounds && col.phys != PhysText && col.phys != PhysBlob:
			return nil, fmt.Errorf("segment: column %d has an unknown cell format %d", c, buf[d+1])
		case col.bounds:
			if int(col.blockOff)+4*(nRows+1) > len(buf) || col.blockOff%segCellWidth != 0 {
				return nil, fmt.Errorf("segment: column %d block is unaligned or overruns", c)
			}
		case col.phys.Width() != 0:
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
	if s.cols[c].bounds {
		cells, ok := s.boundsCells(c)
		return cells, s.heap, ok
	}
	off := int(s.cols[c].blockOff)
	if off == 0 || uintptr(unsafe.Pointer(&s.buf[off]))%8 != 0 {
		return nil, nil, false
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&s.buf[off])), s.nRows), s.heap, true
}

// BytesColumnNulls is BytesColumn for a column that may hold NULLs: nulls is
// its bitmap, bit i set where row i is NULL, or nil when no row is. A NULL
// row's cell means nothing.
func (s *segment) BytesColumnNulls(c int) (cells []uint64, heap []byte, nulls []byte, ok bool) {
	if c < 0 || c >= len(s.cols) || !segLittleEndian {
		return nil, nil, nil, false
	}
	if ph := s.cols[c].phys; ph != PhysText && ph != PhysBlob {
		return nil, nil, nil, false
	}
	if s.hasException(c) {
		return nil, nil, nil, false
	}
	off := int(s.cols[c].blockOff)
	if off == 0 || uintptr(unsafe.Pointer(&s.buf[off]))%8 != 0 {
		return nil, nil, nil, false
	}
	if n := int(s.cols[c].nullOff); n != 0 {
		if n+(s.nRows+7)/8 > len(s.buf) {
			return nil, nil, nil, false
		}
		nulls = s.buf[n : n+(s.nRows+7)/8]
	}
	if s.cols[c].bounds {
		cells, ok := s.boundsCells(c)
		return cells, s.heap, nulls, ok
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&s.buf[off])), s.nRows), s.heap, nulls, true
}

// boundCell is row i's (offset, length) cell in a segCellBounds column, or false
// for bounds that do not describe heap bytes -- a damaged file, which reads as a
// decline, never a slice out of range. A NULL or exception row stores nothing:
// its two bounds are equal, and it reads as an empty value at that offset.
func (s *segment) boundCell(c, i int) (uint64, bool) {
	b := s.buf[int(s.cols[c].blockOff)+4*i:]
	from, to := binary.LittleEndian.Uint32(b), binary.LittleEndian.Uint32(b[4:])
	if from == to {
		if int(to) > len(s.heap) {
			return 0, false
		}
		return uint64(to), true
	}
	if s.cols[c].phys == PhysBlob {
		from = uint32(segAlignBlob(int(from)))
	}
	if from > to || int(to) > len(s.heap) {
		return 0, false
	}
	return uint64(from) | uint64(to-from)<<32, true
}

// cellBytes is row i's TEXT or BLOB bytes in column c, read where they are; false
// when the cell names no heap bytes. NULL and exception rows are the caller's
// to exclude (an index is built only over a column with neither).
func (s *segment) cellBytes(c, i int) ([]byte, bool) {
	if i < 0 || i >= s.nRows {
		return nil, false
	}
	var raw uint64
	if s.cols[c].bounds {
		x, ok := s.boundCell(c, i)
		if !ok {
			return nil, false
		}
		raw = x
	} else {
		raw = binary.LittleEndian.Uint64(s.buf[int(s.cols[c].blockOff)+i*segCellWidth:])
	}
	o, n := uint32(raw), uint32(raw>>32)
	if int(o)+int(n) > len(s.heap) {
		return nil, false
	}
	return s.heap[o : o+n : o+n], true
}

// boundsCells is a segCellBounds column's cells as the 8-byte (offset, length)
// pairs BytesColumn hands out, built once per column and kept: 8 bytes a row
// of memory, paid only by a reader that wants the column whole.
func (s *segment) boundsCells(c int) ([]uint64, bool) {
	s.cellMu.Lock()
	defer s.cellMu.Unlock()
	if cells, seen := s.cellCache[c]; seen {
		return cells, cells != nil
	}
	if s.cellCache == nil {
		s.cellCache = map[int][]uint64{}
	}
	cells := make([]uint64, s.nRows)
	for i := range cells {
		x, ok := s.boundCell(c, i)
		if !ok {
			s.cellCache[c] = nil
			return nil, false
		}
		cells[i] = x
	}
	s.cellCache[c] = cells
	return cells, true
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
	var raw uint64
	if col.bounds {
		x, ok := s.boundCell(c, i)
		if !ok {
			return Value{Typ: Null}
		}
		raw = x
	} else {
		raw = binary.LittleEndian.Uint64(s.buf[int(col.blockOff)+i*segCellWidth:])
	}
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
