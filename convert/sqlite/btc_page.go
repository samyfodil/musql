// This file implements btree page primitives: MemPage decoding, cell parsing,
// sizing, and in-page space management. Page layout and operations must match
// SQLite's byte-for-byte, including stale bytes in unallocated space and the
// freeblock chain.
package sqlite

import (
	"encoding/binary"
	"errors"
)

// errBtCorrupt is SQLITE_CORRUPT's message, which every SQLITE_CORRUPT_PAGE
// and SQLITE_CORRUPT_BKPT in the port returns.
var errBtCorrupt = errors.New("database disk image is malformed")

// Page type flag bits.
const (
	ptfIntKey   = 0x01
	ptfZeroData = 0x02
	ptfLeafData = 0x04
	ptfLeaf     = 0x08
)

// btShared holds the shared btree state needed by the page layer.
type btShared struct {
	pageSize        int
	usableSize      int
	maxLocal        int
	minLocal        int
	maxLeaf         int
	minLeaf         int
	max1bytePayload int
	secureDelete    bool // BTS_FAST_SECURE
	autoVacuum      bool
	incrVacuum      bool
	pager           *btPager
	tx              *btTxn      // the write transaction open on it
	cursors         []*btCursor // BtShared.pCursor: every open cursor
}

// newBtShared initializes shared btree state with calculated payload limits.
func newBtShared(pager *btPager, pageSize, reserved int) *btShared {
	bt := &btShared{pageSize: pageSize, usableSize: pageSize - reserved, pager: pager}
	bt.maxLocal = (bt.usableSize-12)*64/255 - 23
	bt.minLocal = (bt.usableSize-12)*32/255 - 23
	bt.maxLeaf = bt.usableSize - 35
	bt.minLeaf = (bt.usableSize-12)*32/255 - 23
	bt.max1bytePayload = min(bt.maxLocal, 127)
	return bt
}

// mxCell returns the maximum number of cells that can fit in a page.
func (bt *btShared) mxCell() int { return (bt.pageSize - 8) / 6 }

// cellKind selects the xCellSize/xParseCell pair decodeFlags installs.
type cellKind uint8

const (
	cellTableLeaf cellKind = iota
	cellTableInterior
	cellIndexLeaf
	cellIndexInterior
)

// memPage represents an in-memory btree page. aData is the page buffer from the
// pager; offsets below index it. Overflow cells (apOvfl) live in separate buffers.
type memPage struct {
	isInit          bool
	intKey          bool
	intKeyLeaf      bool
	pgno            uint32
	leaf            bool
	hdrOffset       int
	childPtrSize    int
	max1bytePayload int
	nOverflow       int
	maxLocal        int
	minLocal        int
	cellOffset      int
	nFree           int // -1 when not yet computed
	nCell           int
	aiOvfl          [4]int
	apOvfl          [4]btCell
	bt              *btShared
	aData           []byte
	kind            cellKind
}

// btCell is a cell pointer: a buffer plus an offset, allowing code to determine
// whether a cell lives on a specific page.
type btCell struct {
	buf []byte
	off int
}

// b is the cell's bytes to the end of its buffer.
func (c btCell) b() []byte { return c.buf[c.off:] }

// within reports whether the cell lies within [lo, hi) of buf.
func (c btCell) within(buf []byte, lo, hi int) bool {
	return sameBuf(c.buf, buf) && c.off >= lo && c.off < hi
}

// sameBuf reports whether a and b are the same buffer, which is the only case
// in which btree.c's pointer comparisons between them mean anything.
func sameBuf(a, b []byte) bool { return len(a) > 0 && len(b) > 0 && &a[0] == &b[0] }

// cellInfo holds parsed cell information. payload is the offset within the cell.
type cellInfo struct {
	nKey     int64
	payload  int
	nPayload uint32
	nLocal   int
	nSize    int
}

func get2byte(b []byte) int { return int(binary.BigEndian.Uint16(b)) }

func put2byte(b []byte, v int) { binary.BigEndian.PutUint16(b, uint16(v)) }

func get4byte(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func put4byte(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

// get2byteNotZero reads a content offset where 0 encodes as 65536.
func get2byteNotZero(b []byte) int { return ((get2byte(b) - 1) & 0xffff) + 1 }

// findCell returns the bytes of the i-th cell on the page.
func (p *memPage) findCell(i int) []byte { return p.aData[p.findCellOff(i):] }

// findCellOff returns the byte offset of the i-th cell in aData.
func (p *memPage) findCellOff(i int) int {
	return get2byte(p.aData[p.cellOffset+2*i:]) & (p.bt.pageSize - 1)
}

// payloadVarint reads a payload-size varint as btreeParseCellPtr's inlined
// loop does: seven bits from each of at most nine bytes, masked to 32 bits.
func payloadVarint(b []byte) (uint32, int) {
	n := uint64(b[0])
	i := 0
	if n >= 0x80 {
		n &= 0x7f
		for {
			i++
			n = n<<7 | uint64(b[i]&0x7f)
			if b[i] < 0x80 || i >= 8 {
				break
			}
		}
	}
	return uint32(n), i + 1
}

// adjustSizeForOverflow computes nLocal and nSize for a cell with overflowed payload.
func (p *memPage) adjustSizeForOverflow(info *cellInfo) {
	surplus := p.minLocal + int((int64(info.nPayload)-int64(p.minLocal))%int64(p.bt.usableSize-4))
	if surplus <= p.maxLocal {
		info.nLocal = surplus
	} else {
		info.nLocal = p.minLocal
	}
	info.nSize = info.payload + info.nLocal + 4
}

// parseCell parses a cell into cellInfo based on page kind.
func (p *memPage) parseCell(cell []byte, info *cellInfo) {
	switch p.kind {
	case cellTableInterior:
		key, n := getVarint(cell[4:])
		*info = cellInfo{nKey: int64(key), nSize: 4 + n}
	case cellTableLeaf:
		nPayload, n := payloadVarint(cell)
		key, m := getVarint(cell[n:])
		info.nKey = int64(key)
		info.nPayload = nPayload
		info.payload = n + m
		if int(nPayload) <= p.maxLocal {
			info.nSize = max(int(nPayload)+info.payload, 4)
			info.nLocal = int(nPayload)
		} else {
			p.adjustSizeForOverflow(info)
		}
	default:
		nPayload, n := payloadVarint(cell[p.childPtrSize:])
		info.nKey = int64(nPayload)
		info.nPayload = nPayload
		info.payload = p.childPtrSize + n
		if int(nPayload) <= p.maxLocal {
			info.nSize = max(int(nPayload)+info.payload, 4)
			info.nLocal = int(nPayload)
		} else {
			p.adjustSizeForOverflow(info)
		}
	}
}

// parseCellAt parses the i-th cell on the page into cellInfo.
func (p *memPage) parseCellAt(i int, info *cellInfo) { p.parseCell(p.findCell(i), info) }

// cellSize returns the byte size of a cell.
func (p *memPage) cellSize(cell []byte) int {
	if p.kind == cellTableInterior {
		i := 4
		for end := 4 + 9; i < end && i < len(cell); {
			c := cell[i]
			i++
			if c&0x80 == 0 {
				break
			}
		}
		return i
	}
	start := p.childPtrSize
	nSize, n := payloadVarint(cell[start:])
	i := start + n
	if p.kind == cellTableLeaf {
		for k := 0; k < 9; k++ {
			c := cell[i]
			i++
			if c&0x80 == 0 || k == 8 {
				break
			}
		}
	}
	if int(nSize) <= p.maxLocal {
		sz := int(nSize) + i
		if p.kind != cellIndexInterior {
			sz = max(sz, 4)
		}
		return sz
	}
	sz := p.minLocal + int((int64(nSize)-int64(p.minLocal))%int64(p.bt.usableSize-4))
	if sz > p.maxLocal {
		sz = p.minLocal
	}
	return sz + 4 + i
}

// defragmentPage reorganizes free space on a page to reduce fragmentation.
func (p *memPage) defragmentPage(nMaxFrag int) error {
	data := p.aData
	hdr := p.hdrOffset
	cellOffset := p.cellOffset
	nCell := p.nCell
	iCellFirst := cellOffset + 2*nCell
	usableSize := p.bt.usableSize
	var cbrk int

	if int(data[hdr+7]) <= nMaxFrag {
		iFree := get2byte(data[hdr+1:])
		if iFree > usableSize-4 {
			return errBtCorrupt
		}
		if iFree != 0 {
			iFree2 := get2byte(data[iFree:])
			if iFree2 > usableSize-4 {
				return errBtCorrupt
			}
			if iFree2 == 0 || (data[iFree2] == 0 && data[iFree2+1] == 0) {
				sz2 := 0
				sz := get2byte(data[iFree+2:])
				top := get2byte(data[hdr+5:])
				if top >= iFree {
					return errBtCorrupt
				}
				if iFree2 != 0 {
					if iFree+sz > iFree2 {
						return errBtCorrupt
					}
					sz2 = get2byte(data[iFree2+2:])
					if iFree2+sz2 > usableSize {
						return errBtCorrupt
					}
					copy(data[iFree+sz+sz2:], data[iFree+sz:iFree2])
					sz += sz2
				} else if iFree+sz > usableSize {
					return errBtCorrupt
				}
				cbrk = top + sz
				copy(data[cbrk:], data[top:iFree])
				for a := cellOffset; a < cellOffset+nCell*2; a += 2 {
					pc := get2byte(data[a:])
					if pc < iFree {
						put2byte(data[a:], pc+sz)
					} else if pc < iFree2 {
						put2byte(data[a:], pc+sz2)
					}
				}
				return p.defragmentOut(cbrk, iCellFirst)
			}
		}
	}

	cbrk = usableSize
	iCellLast := usableSize - 4
	iCellStart := get2byte(data[hdr+5:])
	if nCell > 0 {
		src := make([]byte, usableSize)
		copy(src, data[:usableSize])
		for i := range nCell {
			a := cellOffset + i*2
			pc := get2byte(data[a:])
			if pc > iCellLast {
				return errBtCorrupt
			}
			size := p.cellSize(src[pc:])
			cbrk -= size
			if cbrk < iCellStart || pc+size > usableSize {
				return errBtCorrupt
			}
			put2byte(data[a:], cbrk)
			copy(data[cbrk:cbrk+size], src[pc:pc+size])
		}
	}
	data[hdr+7] = 0
	return p.defragmentOut(cbrk, iCellFirst)
}

// defragmentOut finishes defragmentation by clearing free space.
func (p *memPage) defragmentOut(cbrk, iCellFirst int) error {
	data := p.aData
	hdr := p.hdrOffset
	if int(data[hdr+7])+cbrk-iCellFirst != p.nFree {
		return errBtCorrupt
	}
	put2byte(data[hdr+5:], cbrk)
	data[hdr+1] = 0
	data[hdr+2] = 0
	clear(data[iCellFirst:cbrk])
	return nil
}

// pageFindSlot finds a free slot for nByte bytes, returning the offset or 0.
func (p *memPage) pageFindSlot(nByte int) (int, error) {
	hdr := p.hdrOffset
	data := p.aData
	iAddr := hdr + 1
	pc := get2byte(data[iAddr:])
	maxPC := p.bt.usableSize - nByte
	for pc <= maxPC {
		size := get2byte(data[pc+2:])
		if x := size - nByte; x >= 0 {
			if x < 4 {
				if data[hdr+7] > 57 {
					return 0, nil
				}
				copy(data[iAddr:iAddr+2], data[pc:pc+2])
				data[hdr+7] += byte(x)
				return pc, nil
			} else if x+pc > maxPC {
				return 0, errBtCorrupt
			}
			put2byte(data[pc+2:], x)
			return pc + x, nil
		}
		iAddr = pc
		pc = get2byte(data[pc:])
		if pc <= iAddr {
			if pc != 0 {
				return 0, errBtCorrupt
			}
			return 0, nil
		}
	}
	if pc > maxPC+nByte-4 {
		return 0, errBtCorrupt
	}
	return 0, nil
}

// allocateSpace allocates nByte bytes on a page, defragmenting if needed.
func (p *memPage) allocateSpace(nByte int) (int, error) {
	hdr := p.hdrOffset
	data := p.aData
	gap := p.cellOffset + 2*p.nCell
	top := get2byte(data[hdr+5:])
	if gap > top {
		if top == 0 && p.bt.usableSize == 65536 {
			top = 65536
		} else {
			return 0, errBtCorrupt
		}
	} else if top > p.bt.usableSize {
		return 0, errBtCorrupt
	}

	if (data[hdr+2] != 0 || data[hdr+1] != 0) && gap+2 <= top {
		g2, err := p.pageFindSlot(nByte)
		if err != nil {
			return 0, err
		}
		if g2 != 0 {
			if g2 <= gap {
				return 0, errBtCorrupt
			}
			return g2, nil
		}
	}

	if gap+2+nByte > top {
		if err := p.defragmentPage(min(4, p.nFree-(2+nByte))); err != nil {
			return 0, err
		}
		top = get2byteNotZero(data[hdr+5:])
	}

	top -= nByte
	put2byte(data[hdr+5:], top)
	return top, nil
}

// freeSpace marks a range of bytes as free and updates the freelist.
func (p *memPage) freeSpace(iStart, iSize int) error {
	data := p.aData
	hdr := p.hdrOffset
	iOrigSize := iSize
	iEnd := iStart + iSize
	iPtr := hdr + 1
	iFreeBlk := 0
	if data[iPtr+1] != 0 || data[iPtr] != 0 {
		for {
			iFreeBlk = get2byte(data[iPtr:])
			if iFreeBlk >= iStart {
				break
			}
			if iFreeBlk <= iPtr {
				if iFreeBlk == 0 {
					break
				}
				return errBtCorrupt
			}
			iPtr = iFreeBlk
		}
		if iFreeBlk > p.bt.usableSize-4 {
			return errBtCorrupt
		}
		nFrag := 0
		if iFreeBlk != 0 && iEnd+3 >= iFreeBlk {
			nFrag = iFreeBlk - iEnd
			if iEnd > iFreeBlk {
				return errBtCorrupt
			}
			iEnd = iFreeBlk + get2byte(data[iFreeBlk+2:])
			if iEnd > p.bt.usableSize {
				return errBtCorrupt
			}
			iSize = iEnd - iStart
			iFreeBlk = get2byte(data[iFreeBlk:])
		}
		if iPtr > hdr+1 {
			iPtrEnd := iPtr + get2byte(data[iPtr+2:])
			if iPtrEnd+3 >= iStart {
				if iPtrEnd > iStart {
					return errBtCorrupt
				}
				nFrag += iStart - iPtrEnd
				iSize = iEnd - iPtr
				iStart = iPtr
			}
		}
		if nFrag > int(data[hdr+7]) {
			return errBtCorrupt
		}
		data[hdr+7] -= byte(nFrag)
	}
	x := get2byte(data[hdr+5:])
	if p.bt.secureDelete {
		clear(data[iStart : iStart+iSize])
	}
	if iStart <= x {
		if iStart < x || iPtr != hdr+1 {
			return errBtCorrupt
		}
		put2byte(data[hdr+1:], iFreeBlk)
		put2byte(data[hdr+5:], iEnd)
	} else {
		put2byte(data[iPtr:], iStart)
		put2byte(data[iStart:], iFreeBlk)
		put2byte(data[iStart+2:], iSize)
	}
	p.nFree += iOrigSize
	return nil
}

// decodeFlags sets page properties based on the flag byte.
func (p *memPage) decodeFlags(flagByte int) error {
	bt := p.bt
	p.max1bytePayload = bt.max1bytePayload
	if flagByte >= ptfZeroData|ptfLeaf {
		p.childPtrSize = 0
		p.leaf = true
		switch flagByte {
		case ptfLeafData | ptfIntKey | ptfLeaf:
			p.intKeyLeaf, p.intKey = true, true
			p.kind = cellTableLeaf
			p.maxLocal, p.minLocal = bt.maxLeaf, bt.minLeaf
		case ptfZeroData | ptfLeaf:
			p.intKeyLeaf, p.intKey = false, false
			p.kind = cellIndexLeaf
			p.maxLocal, p.minLocal = bt.maxLocal, bt.minLocal
		default:
			p.intKeyLeaf, p.intKey = false, false
			p.kind = cellIndexLeaf
			return errBtCorrupt
		}
		return nil
	}
	p.childPtrSize = 4
	p.leaf = false
	switch flagByte {
	case ptfZeroData:
		p.intKeyLeaf, p.intKey = false, false
		p.kind = cellIndexInterior
		p.maxLocal, p.minLocal = bt.maxLocal, bt.minLocal
	case ptfLeafData | ptfIntKey:
		p.intKeyLeaf, p.intKey = false, true
		p.kind = cellTableInterior
		p.maxLocal, p.minLocal = bt.maxLeaf, bt.minLeaf
	default:
		p.intKeyLeaf, p.intKey = false, false
		p.kind = cellIndexInterior
		return errBtCorrupt
	}
	return nil
}

// computeFreeSpace calculates the nFree field by walking the freelist.
func (p *memPage) computeFreeSpace() error {
	usableSize := p.bt.usableSize
	hdr := p.hdrOffset
	data := p.aData
	top := get2byteNotZero(data[hdr+5:])
	iCellFirst := hdr + 8 + p.childPtrSize + 2*p.nCell
	iCellLast := usableSize - 4
	pc := get2byte(data[hdr+1:])
	nFree := int(data[hdr+7]) + top
	if pc > 0 {
		if pc < top {
			return errBtCorrupt
		}
		var next, size int
		for {
			if pc > iCellLast {
				return errBtCorrupt
			}
			next = get2byte(data[pc:])
			size = get2byte(data[pc+2:])
			if size < 4 {
				return errBtCorrupt
			}
			nFree += size
			if next < pc+size+4 {
				break
			}
			pc = next
		}
		if next > 0 || pc+size > usableSize {
			return errBtCorrupt
		}
	}
	if nFree > usableSize || nFree < iCellFirst {
		return errBtCorrupt
	}
	p.nFree = nFree - iCellFirst
	return nil
}

// initPage initializes a memPage from its page header.
func (p *memPage) initPage() error {
	data := p.aData[p.hdrOffset:]
	if err := p.decodeFlags(int(data[0])); err != nil {
		return errBtCorrupt
	}
	p.nOverflow = 0
	p.cellOffset = p.hdrOffset + 8 + p.childPtrSize
	p.nCell = get2byte(data[3:])
	if p.nCell > p.bt.mxCell() {
		return errBtCorrupt
	}
	p.nFree = -1
	p.isInit = true
	return nil
}

// zeroPage initializes a blank page with the given flags.
func (p *memPage) zeroPage(flags int) {
	data := p.aData
	bt := p.bt
	hdr := p.hdrOffset
	if bt.secureDelete {
		clear(data[hdr:bt.usableSize])
	}
	data[hdr] = byte(flags)
	first := hdr + 8
	if flags&ptfLeaf == 0 {
		first = hdr + 12
	}
	clear(data[hdr+1 : hdr+5])
	data[hdr+7] = 0
	put2byte(data[hdr+5:], bt.usableSize)
	p.nFree = bt.usableSize - first
	p.decodeFlags(flags)
	p.cellOffset = first
	p.nOverflow = 0
	p.nCell = 0
	p.isInit = true
}
