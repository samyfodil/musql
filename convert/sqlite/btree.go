// This file implements read-only traversal of SQLite TABLE b-trees: the
// interior/leaf page layout, cell formats, and payload overflow chains. See
// https://www.sqlite.org/fileformat2.html#b_tree_pages
package sqlite

import (
	"encoding/binary"
	"fmt"
	"iter"
)

// Page type bytes, found at the start of the b-tree page header.
const (
	pageTypeIndexInterior = 0x02
	pageTypeTableInterior = 0x05
	pageTypeIndexLeaf     = 0x0a
	pageTypeTableLeaf     = 0x0d
)

// btreePageHeader is the parsed b-tree page header common to all four page
// types (8 bytes for leaf pages, 12 for interior pages, the last 4 being the
// right-most child pointer).
type btreePageHeader struct {
	pageType    byte
	numCells    uint16
	rightmost   uint32 // interior pages only
	hdrOffset   int    // 0, or 100 on page 1 (past the file header)
	headerSize  int    // 8 (leaf) or 12 (interior)
	cellPtrBase int    // hdrOffset + headerSize: start of the cell pointer array
}

// parseBtreePageHeader reads the b-tree page header out of a raw page. pgno
// is needed because page 1 carries the page header immediately after the
// 100-byte file header instead of at offset 0.
func parseBtreePageHeader(page []byte, pgno uint32) (btreePageHeader, error) {
	hdrOffset := 0
	if pgno == 1 {
		hdrOffset = HeaderSize
	}
	if len(page) < hdrOffset+8 {
		return btreePageHeader{}, fmt.Errorf("engine: page %d too short for a b-tree page header", pgno)
	}
	pageType := page[hdrOffset]

	var headerSize int
	switch pageType {
	case pageTypeTableLeaf, pageTypeTableInterior:
		headerSize = 8
		if pageType == pageTypeTableInterior {
			headerSize = 12
		}
	case pageTypeIndexLeaf, pageTypeIndexInterior:
		return btreePageHeader{}, fmt.Errorf("engine: unsupported: WITHOUT ROWID / index b-tree (page %d, type 0x%02x)", pgno, pageType)
	default:
		return btreePageHeader{}, fmt.Errorf("engine: page %d: unknown b-tree page type 0x%02x", pgno, pageType)
	}
	if len(page) < hdrOffset+headerSize {
		return btreePageHeader{}, fmt.Errorf("engine: page %d too short for its b-tree page header", pgno)
	}

	h := btreePageHeader{
		pageType:    pageType,
		numCells:    binary.BigEndian.Uint16(page[hdrOffset+3 : hdrOffset+5]),
		hdrOffset:   hdrOffset,
		headerSize:  headerSize,
		cellPtrBase: hdrOffset + headerSize,
	}
	if headerSize == 12 {
		h.rightmost = binary.BigEndian.Uint32(page[hdrOffset+8 : hdrOffset+12])
	}
	return h, nil
}

// cellOffset returns the absolute in-page byte offset of the i'th cell, read
// from the page's cell pointer array.
func (h btreePageHeader) cellOffset(page []byte, i int) (int, error) {
	off := h.cellPtrBase + 2*i
	if off+2 > len(page) {
		return 0, fmt.Errorf("engine: cell pointer %d overruns page", i)
	}
	return int(binary.BigEndian.Uint16(page[off : off+2])), nil
}

// localPayloadSize computes how many bytes of a payloadLen-byte record are
// stored on the b-tree page itself (the rest lives in an overflow chain), per
// the table-b-tree-leaf-page formulas in the file format spec: maxLocal =
// usable-35, minLocal = (usable-12)*32/255 - 23.
func localPayloadSize(payloadLen uint64, usable uint32) uint64 {
	u := uint64(usable)
	maxLocal := u - 35
	if payloadLen <= maxLocal {
		return payloadLen
	}
	minLocal := (u-12)*32/255 - 23
	k := minLocal + (payloadLen-minLocal)%(u-4)
	if k <= maxLocal {
		return k
	}
	return minLocal
}

// readOverflow follows an overflow page chain, appending the remaining
// payload bytes onto local (the bytes already read off the b-tree page) and
// returning the assembled payload. Each overflow page begins with a 4-byte
// big-endian pointer to the next overflow page (0 terminates the chain),
// followed by up to usable-4 bytes of payload data.
func (p *pager) readOverflow(local []byte, payloadLen uint64, firstOverflow uint32) ([]byte, error) {
	usable := p.hdr.UsablePageSize()
	buf := make([]byte, len(local), payloadLen)
	copy(buf, local)
	remaining := payloadLen - uint64(len(local))
	pgno := firstOverflow
	for remaining > 0 {
		if pgno == 0 {
			return nil, fmt.Errorf("engine: overflow chain ended with %d bytes still unread", remaining)
		}
		page, err := p.page(pgno)
		if err != nil {
			return nil, err
		}
		if len(page) < 4 {
			return nil, fmt.Errorf("engine: overflow page %d too short", pgno)
		}
		next := binary.BigEndian.Uint32(page[0:4])
		avail := uint64(usable) - 4
		take := remaining
		if take > avail {
			take = avail
		}
		if uint64(len(page)) < 4+take {
			return nil, fmt.Errorf("engine: overflow page %d shorter than its declared usable size", pgno)
		}
		buf = append(buf, page[4:4+take]...)
		remaining -= take
		pgno = next
	}
	return buf, nil
}

// readTableLeafCell decodes one table-b-tree-leaf cell starting at
// cellOffset within page: payloadLen varint, rowid varint, then the payload
// (on-page bytes, plus an overflow chain if the payload didn't fit locally).
// buf is an optional caller-owned []Value to decode the record into (reused
// when large enough, see decodeRecordInto); pass nil to allocate a fresh row.
// Only a single-row-at-a-time streaming caller may reuse a
// buffer -- a caller that retains the returned row must pass nil.
//
// mask names the columns to actually decode (allColumns for everything). The
// raw record is returned alongside the row so a masked caller can re-decode it
// in full later without re-walking the b-tree -- see decodeRecordMaskedInto for
// why that re-decode is what keeps the mask a pure performance hint.
func (p *pager) readTableLeafCell(page []byte, cellOffset int, buf []Value, mask columnMask) (rowid uint64, vals []Value, payload []byte, err error) {
	if cellOffset < 0 || cellOffset > len(page) {
		return 0, nil, nil, fmt.Errorf("engine: cell offset %d out of range", cellOffset)
	}
	b := page[cellOffset:]

	payloadLen, n := getVarint(b)
	if n == 0 {
		return 0, nil, nil, fmt.Errorf("engine: truncated payload-length varint")
	}
	b = b[n:]

	rowid, n = getVarint(b)
	if n == 0 {
		return 0, nil, nil, fmt.Errorf("engine: truncated rowid varint")
	}
	b = b[n:]

	usable := p.hdr.UsablePageSize()
	local := localPayloadSize(payloadLen, usable)
	if local > uint64(len(b)) {
		return 0, nil, nil, fmt.Errorf("engine: local payload size %d exceeds %d bytes available in cell", local, len(b))
	}

	if local == payloadLen {
		// No overflow: the whole payload is already sitting in page (which
		// p.page returned and never mutates in place). Slicing it directly instead of copying saves one
		// full-payload allocation+memcpy per row. The TEXT/BLOB Values that
		// decodeRecord produces now sub-slice this payload (and thus page's
		// backing array) rather than deep-copying -- safe because the page is
		// an immutable snapshot never mutated in place, and the driver
		// boundary copies TEXT/BLOB out before a Value can outlive the pager
		// (see decodeSerialInto's zero-copy note and engineValueToDriver).
		payload = b[:local]
	} else {
		if uint64(len(b)) < local+4 {
			return 0, nil, nil, fmt.Errorf("engine: cell truncated before overflow page pointer")
		}
		firstOverflow := binary.BigEndian.Uint32(b[local : local+4])
		payload, err = p.readOverflow(b[:local], payloadLen, firstOverflow)
		if err != nil {
			return 0, nil, nil, err
		}
	}

	vals, err = decodeRecordMaskedIntoEnc(payload, buf, p.encoding(), mask)
	if err != nil {
		return 0, nil, nil, err
	}
	return rowid, vals, payload, nil
}

// tableScanner walks a table b-tree, tracking the first error encountered so
// it can be surfaced to the caller once range-over-func iteration ends.
type tableScanner struct {
	p   *pager
	err error
}

// walk visits pgno's subtree in ascending-rowid order, calling yield for each
// leaf row. It returns false if iteration should stop, either because yield
// asked to stop or because an error occurred (in which case s.err is set).
func (s *tableScanner) walk(pgno uint32, yield func(uint64, []Value) bool) bool {
	if s.err != nil {
		return false
	}
	page, err := s.p.page(pgno)
	if err != nil {
		s.err = err
		return false
	}
	hdr, err := parseBtreePageHeader(page, pgno)
	if err != nil {
		s.err = err
		return false
	}

	switch hdr.pageType {
	case pageTypeTableLeaf:
		for i := 0; i < int(hdr.numCells); i++ {
			off, err := hdr.cellOffset(page, i)
			if err != nil {
				s.err = err
				return false
			}
			rowid, vals, _, err := s.p.readTableLeafCell(page, off, nil, allColumns)
			if err != nil {
				s.err = err
				return false
			}
			if !yield(rowid, vals) {
				return false
			}
		}
		return true

	case pageTypeTableInterior:
		for i := 0; i < int(hdr.numCells); i++ {
			off, err := hdr.cellOffset(page, i)
			if err != nil {
				s.err = err
				return false
			}
			if off+4 > len(page) {
				s.err = fmt.Errorf("engine: interior cell %d truncated before child pointer", i)
				return false
			}
			child := binary.BigEndian.Uint32(page[off : off+4])
			// The cell's rowid key (a varint following the child pointer) is
			// only needed to bound a search; a full ascending scan doesn't
			// need it; descend left-to-right instead.
			if !s.walk(child, yield) {
				return false
			}
		}
		return s.walk(hdr.rightmost, yield)

	default:
		// parseBtreePageHeader already rejects index pages and unknown
		// types, so this is unreachable.
		s.err = fmt.Errorf("engine: unexpected page type 0x%02x", hdr.pageType)
		return false
	}
}

// ScanTable walks the table b-tree rooted at rootPage in ascending rowid
// order, yielding each row's rowid and decoded column values. Because
// iter.Seq2 has no room for an error return, the scan's error (if any) is
// only known once iteration has stopped; call the returned errFn afterward
// (whether the range loop ran to completion or broke early) to check it.
//
// INTEGER PRIMARY KEY aliasing: when a table has an INTEGER PRIMARY KEY
// column, SQLite stores that column as a NULL in the record and keeps the
// real value only as the b-tree key. ScanTable returns the key separately as
// rowid precisely so callers can substitute it for such a column; this
// package has no column/schema metadata to do that substitution itself.
func (p *pager) ScanTable(rootPage uint32) (seq iter.Seq2[uint64, []Value], errFn func() error) {
	s := &tableScanner{p: p}
	return func(yield func(uint64, []Value) bool) {
		// A database with NO PAGES has no b-tree to walk -- a zero-length file,
		// which C SQLite reads as an empty database (see newPager).
		// Every table in it, sqlite_schema included, is empty.
		if p != nil && p.nPages == 0 {
			return
		}
		s.walk(rootPage, yield)
	}, func() error { return s.err }
}

// tableLeafCellSpan reads a table-b-tree LEAF cell's header (payload-length
// varint, then rowid varint) starting at off within page, and returns its
// rowid key together with the cell's TOTAL on-page byte length (header +
// local payload + a trailing 4-byte overflow pointer if the payload didn't
// fit locally) -- everything loadTableNode needs to slice out the cell
// verbatim without decoding its payload at all (an untouched cell's exact
// bytes are copied through unexamined; see this file's package doc comment).
func tableLeafCellSpan(page []byte, off int, usable uint32) (rowid uint64, length int, err error) {
	if off < 0 || off > len(page) {
		return 0, 0, fmt.Errorf("engine: cell offset %d out of range", off)
	}
	b := page[off:]
	payloadLen, n1 := getVarint(b)
	if n1 == 0 {
		return 0, 0, fmt.Errorf("engine: truncated payload-length varint")
	}
	b = b[n1:]
	rowid, n2 := getVarint(b)
	if n2 == 0 {
		return 0, 0, fmt.Errorf("engine: truncated rowid varint")
	}
	local := localPayloadSize(payloadLen, usable)
	length = n1 + n2 + int(local)
	if local < payloadLen {
		length += 4 // trailing overflow-page pointer
	}
	return rowid, length, nil
}

func (h idxPageHeader) cellOffset(page []byte, i int) (int, error) {
	off := h.headerSize + 2*i
	if off+2 > len(page) {
		return 0, fmt.Errorf("engine: index cell pointer %d overruns page", i)
	}
	return int(binary.BigEndian.Uint16(page[off : off+2])), nil
}
