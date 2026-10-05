// This file is the read-only counterpart of index_btree_write.go, needed for
// exactly one purpose: scanning a WITHOUT ROWID table's own b-tree. Per the
// file format spec (section 4, "storage of the SQL database schema" /
// WITHOUT ROWID tables) and verified directly against C SQLite
// (mattn/go-sqlite3) by inspecting raw page bytes, such a table's root page
// (recorded as an ordinary type='table' sqlite_schema row) is physically an
// INDEX b-tree (page types 0x0a/0x02 -- see btree.go's pageTypeIndexLeaf/
// pageTypeIndexInterior), NOT a table b-tree: every cell (leaf or interior
// alike) carries a full record whose columns are the table's own PRIMARY KEY
// column(s), in PRIMARY KEY clause order, followed by every remaining
// (non-PRIMARY-KEY) column in table-declaration order.
//
// ScanTable cannot read it -- parseBtreePageHeader rejects index pages,
// since a table b-tree's interior cells hold only routing keys while every
// cell of an index b-tree, interior or leaf, carries a payload to yield. The
// scan here is an ordinary in-order traversal (left child, this cell, ...,
// rightmost child).
package sqlite

import (
	"encoding/binary"
	"fmt"
	"iter"
)

// indexBtreePageHeader is parseBtreePageHeader's counterpart for an INDEX
// b-tree page (0x0a leaf, 0x02 interior) -- the page type that
// parseBtreePageHeader itself explicitly rejects. A WITHOUT ROWID table's
// root is never sqlite_schema's own page 1 (schemaRootPage always claims that
// page first), so unlike
// parseBtreePageHeader there is no page-1-header-offset special case here.
type indexBtreePageHeader struct {
	leaf        bool
	numCells    uint16
	rightmost   uint32 // interior only
	headerSize  int    // 8 (leaf) or 12 (interior)
	cellPtrBase int
}

// parseIndexBtreePageHeader parses page's b-tree page header, requiring an
// INDEX page type (0x0a/0x02) -- any other type (including an ordinary TABLE
// page, which would mean this "WITHOUT ROWID table" root is actually
// malformed) is a clear error rather than a silently wrong scan.
func parseIndexBtreePageHeader(page []byte, pgno uint32) (indexBtreePageHeader, error) {
	if len(page) < 8 {
		return indexBtreePageHeader{}, fmt.Errorf("engine: page %d too short for a b-tree page header", pgno)
	}
	pageType := page[0]
	var h indexBtreePageHeader
	switch pageType {
	case pageTypeIndexLeaf:
		h.leaf = true
		h.headerSize = 8
	case pageTypeIndexInterior:
		h.headerSize = 12
	default:
		return indexBtreePageHeader{}, fmt.Errorf("engine: WITHOUT ROWID table page %d: expected an index b-tree page (0x0a/0x02), got type 0x%02x", pgno, pageType)
	}
	if len(page) < h.headerSize {
		return indexBtreePageHeader{}, fmt.Errorf("engine: page %d too short for its b-tree page header", pgno)
	}
	h.numCells = binary.BigEndian.Uint16(page[3:5])
	h.cellPtrBase = h.headerSize
	if !h.leaf {
		h.rightmost = binary.BigEndian.Uint32(page[8:12])
	}
	return h, nil
}

func (h indexBtreePageHeader) cellOffset(page []byte, i int) (int, error) {
	off := h.cellPtrBase + 2*i
	if off+2 > len(page) {
		return 0, fmt.Errorf("engine: cell pointer %d overruns page", i)
	}
	return int(binary.BigEndian.Uint16(page[off : off+2])), nil
}

// readIndexCellRecord decodes the record (payload-length varint, local
// payload bytes, optional overflow chain -- per localPayloadSizeIndex, the
// SAME local/overflow split formula index_btree_write.go's write side uses
// for both leaf and interior index cells) starting at recOff within page --
// recOff is already past a leaf cell's own start, or past an interior
// cell's leading 4-byte child pointer.
func (p *pager) readIndexCellRecord(page []byte, recOff int) ([]Value, error) {
	if recOff < 0 || recOff > len(page) {
		return nil, fmt.Errorf("engine: cell offset %d out of range", recOff)
	}
	b := page[recOff:]
	payloadLen, n := getVarint(b)
	if n == 0 {
		return nil, fmt.Errorf("engine: truncated payload-length varint")
	}
	b = b[n:]

	usable := p.hdr.UsablePageSize()
	local := localPayloadSizeIndex(payloadLen, usable)
	if local > uint64(len(b)) {
		return nil, fmt.Errorf("engine: local payload size %d exceeds %d bytes available in cell", local, len(b))
	}

	var payload []byte
	var err error
	if local == payloadLen {
		payload = append([]byte(nil), b[:local]...)
	} else {
		if uint64(len(b)) < local+4 {
			return nil, fmt.Errorf("engine: cell truncated before overflow page pointer")
		}
		firstOverflow := binary.BigEndian.Uint32(b[local : local+4])
		payload, err = p.readOverflow(b[:local], payloadLen, firstOverflow)
		if err != nil {
			return nil, err
		}
	}
	return decodeRecordEnc(payload, p.encoding())
}

// withoutRowidScanner mirrors btree.go's tableScanner, walking an INDEX
// b-tree in ascending order instead of a TABLE b-tree.
type withoutRowidScanner struct {
	p   *pager
	err error
	n   int // sequence number yielded so far -- the ScanWithoutRowidRows int key, purely positional/opaque
}

// walk visits pgno's subtree in ascending order, yielding each cell's
// decoded record. Unlike btree.go's tableScanner.walk (a table b-tree, where
// only LEAF cells carry a payload), an index b-tree's INTERIOR cells carry a
// payload too and must be yielded in their correct in-order position:
// recurse into the cell's own child (every key less than it), then yield
// the cell itself, then move to the next cell; finally recurse into
// rightmost (every key greater than every cell here).
func (s *withoutRowidScanner) walk(pgno uint32, yield func(int, []Value) bool) bool {
	if s.err != nil {
		return false
	}
	page, err := s.p.page(pgno)
	if err != nil {
		s.err = err
		return false
	}
	hdr, err := parseIndexBtreePageHeader(page, pgno)
	if err != nil {
		s.err = err
		return false
	}
	for i := 0; i < int(hdr.numCells); i++ {
		off, err := hdr.cellOffset(page, i)
		if err != nil {
			s.err = err
			return false
		}
		recOff := off
		if !hdr.leaf {
			if off+4 > len(page) {
				s.err = fmt.Errorf("engine: interior cell %d truncated before child pointer", i)
				return false
			}
			child := binary.BigEndian.Uint32(page[off : off+4])
			recOff = off + 4
			if !s.walk(child, yield) {
				return false
			}
		}
		rec, err := s.p.readIndexCellRecord(page, recOff)
		if err != nil {
			s.err = err
			return false
		}
		n := s.n
		s.n++
		if !yield(n, rec) {
			return false
		}
	}
	if !hdr.leaf {
		return s.walk(hdr.rightmost, yield)
	}
	return true
}

// ScanWithoutRowidRows walks the WITHOUT ROWID table b-tree rooted at
// rootPage in ascending PRIMARY KEY order, yielding each row's raw on-disk
// record (PRIMARY KEY column(s) first, in PRIMARY KEY clause order, then
// every remaining column in table-declaration order -- see this file's
// package doc comment) alongside an opaque, purely positional int (0, 1, 2,
// ... in scan order) that carries NO rowid semantics at all -- a WITHOUT
// ROWID table has no rowid, and callers must never treat this as one (see
// join.go/vdbe_cursor.go's callers, which route it only through a
// tableScope.noRowid-marked scope). Callers must permute the yielded record
// back into table-declared column order themselves -- see
// permuteWithoutRowidRow -- mirroring ScanTable's own schema-agnostic
// contract (this function has no column/PRIMARY-KEY-position knowledge of
// its own). Because iter.Seq2 has no room for an error return, the scan's
// error (if any) is only known once iteration has stopped; call the
// returned errFn afterward to check it.
func (p *pager) ScanWithoutRowidRows(rootPage uint32) (seq iter.Seq2[int, []Value], errFn func() error) {
	s := &withoutRowidScanner{p: p}
	return func(yield func(int, []Value) bool) {
		s.walk(rootPage, yield)
	}, func() error { return s.err }
}


// localPayloadSizeIndex computes how many bytes of a payloadLen-byte index
// record are stored on the b-tree page itself (the rest lives in an overflow
// chain), per the file-format spec's INDEX b-tree page formula -- distinct
// from a table LEAF page's formula (localPayloadSize in btree.go): maxLocal
// = (usable-12)*64/255 - 23, versus a table leaf's usable-35. This same
// formula applies to BOTH index leaf and index interior pages (unlike table
// b-trees, where only leaf pages carry payload at all).
func localPayloadSizeIndex(payloadLen uint64, usable uint32) uint64 {
	u := uint64(usable)
	maxLocal := (u-12)*64/255 - 23
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
