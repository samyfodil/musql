package sqlite

// btPayload represents a btree payload: a table row's rowid with data, or an
// index entry's key.
type btPayload struct {
	key   []byte
	nKey  int64
	data  []byte
	nZero int
}

// pendingBytePage returns the page number where the pending byte lock is located.
func (bt *btShared) pendingBytePage() uint32 { return uint32(0x40000000/bt.pageSize) + 1 }

// getPage retrieves a page by number, optionally without its content.
func (bt *btShared) getPage(pgno uint32, noContent bool) (*memPage, error) {
	return bt.pager.get(bt, pgno, noContent)
}

// getUnusedPage retrieves a page and marks it uninitialized.
func (bt *btShared) getUnusedPage(pgno uint32, noContent bool) (*memPage, error) {
	p, err := bt.getPage(pgno, noContent)
	if err != nil {
		return nil, err
	}
	p.isInit = false
	return p, nil
}

// getAndInitPage retrieves and initializes a page, validating its number.
func (bt *btShared) getAndInitPage(pgno uint32, nPage uint32) (*memPage, error) {
	if pgno > nPage {
		return nil, errBtCorrupt
	}
	p, err := bt.getPage(pgno, false)
	if err != nil {
		return nil, err
	}
	if !p.isInit {
		if err := p.initPage(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// btTxn holds the per-transaction btree state: page count and content map.
type btTxn struct {
	bt          *btShared
	nPage       uint32
	hasContent  map[uint32]bool
	hasContentN uint32 // sqlite3BitvecSize(pHasContent); 0 while the bitvec does not exist
	page1       *memPage
	tmp         []byte // BtShared.pTmpSpace
	doTruncate  bool   // BtShared.bDoTruncate
}

// setHasContent is btreeSetHasContent (btree.c:651).
func (tx *btTxn) setHasContent(pgno uint32) {
	if tx.hasContent == nil {
		tx.hasContent = map[uint32]bool{}
		tx.hasContentN = tx.nPage
	}
	if pgno <= tx.hasContentN {
		tx.hasContent[pgno] = true
	}
}

// getHasContent reports whether a page is marked as having content.
func (tx *btTxn) getHasContent(pgno uint32) bool {
	return tx.hasContent != nil && (pgno > tx.hasContentN || tx.hasContent[pgno])
}

// fillInCell formats a btree cell with payload (local or overflowed).
func (tx *btTxn) fillInCell(p *memPage, cell []byte, x *btPayload) (int, error) {
	nHeader := p.childPtrSize
	var nPayload int
	var src []byte
	if p.intKey {
		nPayload = len(x.data) + x.nZero
		src = x.data
		nHeader += putVarint(cell[nHeader:], uint64(nPayload))
		nHeader += putVarint(cell[nHeader:], uint64(x.nKey))
	} else {
		nPayload = int(x.nKey)
		src = x.key
		nHeader += putVarint(cell[nHeader:], uint64(nPayload))
	}
	payload := cell[nHeader:]
	if nPayload <= p.maxLocal {
		n := nHeader + nPayload
		if n < 4 {
			n = 4
			payload[nPayload] = 0
		}
		copy(payload, src)
		clear(payload[len(src):nPayload])
		return n, nil
	}

	mn := p.minLocal
	n := mn + (nPayload-mn)%(tx.bt.usableSize-4)
	if n > p.maxLocal {
		n = mn
	}
	spaceLeft := n
	size := n + nHeader + 4
	prior := cell[nHeader+n:]
	var pgnoOvfl uint32
	for {
		n = min(nPayload, spaceLeft)
		switch {
		case len(src) >= n:
			copy(payload[:n], src[:n])
		case len(src) > 0:
			n = len(src)
			copy(payload[:n], src)
		default:
			clear(payload[:n])
		}
		nPayload -= n
		if nPayload <= 0 {
			break
		}
		payload = payload[n:]
		src = src[n:]
		spaceLeft -= n
		if spaceLeft == 0 {
			pgnoPtrmap := pgnoOvfl
			if tx.bt.autoVacuum {
				for {
					pgnoOvfl++
					if !tx.bt.ptrmapIsPage(pgnoOvfl) && pgnoOvfl != tx.bt.pendingBytePage() {
						break
					}
				}
			}
			ovfl, pgno, err := tx.allocateBtreePage(pgnoOvfl, btallocAny)
			if err != nil {
				return 0, err
			}
			pgnoOvfl = pgno
			if tx.bt.autoVacuum {
				eType := byte(ptrmapOverflow1)
				if pgnoPtrmap != 0 {
					eType = ptrmapOverflow2
				}
				if err := tx.ptrmapPut(pgnoOvfl, eType, pgnoPtrmap); err != nil {
					return 0, err
				}
			}
			put4byte(prior, pgnoOvfl)
			prior = ovfl.aData
			put4byte(prior, 0)
			payload = ovfl.aData[4:]
			spaceLeft = tx.bt.usableSize - 4
		}
	}
	return size, nil
}

// clearCellOverflow frees the overflow pages of a cell.
func (tx *btTxn) clearCellOverflow(p *memPage, cell []byte, info *cellInfo) error {
	if info.nSize > len(cell) {
		return errBtCorrupt
	}
	ovflPgno := get4byte(cell[info.nSize-4:])
	ovflPageSize := uint32(tx.bt.usableSize - 4)
	nOvfl := (info.nPayload - uint32(info.nLocal) + ovflPageSize - 1) / ovflPageSize
	for ; nOvfl > 0; nOvfl-- {
		if ovflPgno < 2 || ovflPgno > tx.nPage {
			return errBtCorrupt
		}
		var next uint32
		var ovfl *memPage
		if nOvfl > 1 {
			var err error
			if ovfl, next, err = tx.getOverflowPage(ovflPgno); err != nil {
				return err
			}
		}
		if err := tx.freePage2(ovfl, ovflPgno); err != nil {
			return err
		}
		ovflPgno = next
	}
	return nil
}

// clearCell parses and clears a cell and its overflow pages.
func (tx *btTxn) clearCell(p *memPage, cell []byte, info *cellInfo) error {
	p.parseCell(cell, info)
	if uint32(info.nLocal) != info.nPayload {
		return tx.clearCellOverflow(p, cell, info)
	}
	return nil
}

// getOverflowPage retrieves the next overflow page, using the pointer map on
// auto_vacuum databases to avoid reading the current page.
func (tx *btTxn) getOverflowPage(ovfl uint32) (*memPage, uint32, error) {
	if tx.bt.autoVacuum {
		iGuess := ovfl + 1
		for tx.bt.ptrmapIsPage(iGuess) || iGuess == tx.bt.pendingBytePage() {
			iGuess++
		}
		if iGuess <= tx.nPage {
			if eType, pgno, err := tx.ptrmapGet(iGuess); err == nil && eType == ptrmapOverflow2 && pgno == ovfl {
				return nil, iGuess, nil
			}
		}
	}
	p, err := tx.bt.getPage(ovfl, false)
	if err != nil {
		return nil, 0, err
	}
	return p, get4byte(p.aData), nil
}

// dropCell removes a cell from a page and updates the free-space map.
func (p *memPage) dropCell(idx, sz int) error {
	data := p.aData
	ptr := p.cellOffset + 2*idx
	pc := get2byte(data[ptr:])
	hdr := p.hdrOffset
	if pc+sz > p.bt.usableSize {
		return errBtCorrupt
	}
	if err := p.freeSpace(pc, sz); err != nil {
		return err
	}
	p.nCell--
	if p.nCell == 0 {
		clear(data[hdr+1 : hdr+5])
		data[hdr+7] = 0
		put2byte(data[hdr+5:], p.bt.usableSize)
		p.nFree = p.bt.usableSize - p.hdrOffset - p.childPtrSize - 8
	} else {
		copy(data[ptr:], data[ptr+2:ptr+2+2*(p.nCell-idx)])
		put2byte(data[hdr+3:], p.nCell)
		p.nFree += 2
	}
	return nil
}

// insertCell inserts a cell into a page, handling overflow to apOvfl when needed.
func (p *memPage) insertCell(i int, cell btCell, sz int, temp *btCell, iChild uint32) error {
	if p.nOverflow > 0 || sz+2 > p.nFree {
		if temp != nil {
			copy(temp.b()[:sz], cell.b()[:sz])
			cell = *temp
		}
		put4byte(cell.b(), iChild)
		j := p.nOverflow
		p.nOverflow++
		p.apOvfl[j] = cell
		p.aiOvfl[j] = i
		return nil
	}
	p.bt.pager.write(p)
	data := p.aData
	idx, err := p.allocateSpace(sz)
	if err != nil {
		return err
	}
	p.nFree -= 2 + sz
	copy(data[idx+4:idx+sz], cell.b()[4:sz])
	put4byte(data[idx:], iChild)
	p.insertCellPointer(i, idx)
	if p.bt.autoVacuum {
		return p.bt.tx.ptrmapPutOvflPtr(p, p.aData, cell)
	}
	return nil
}

// insertCellFast inserts a cell without temp space or child pointer.
func (p *memPage) insertCellFast(i int, cell btCell, sz int) error {
	if sz+2 > p.nFree {
		j := p.nOverflow
		p.nOverflow++
		p.apOvfl[j] = cell
		p.aiOvfl[j] = i
		return nil
	}
	p.bt.pager.write(p)
	data := p.aData
	idx, err := p.allocateSpace(sz)
	if err != nil {
		return err
	}
	p.nFree -= 2 + sz
	copy(data[idx:idx+sz], cell.b()[:sz])
	p.insertCellPointer(i, idx)
	if p.bt.autoVacuum {
		return p.bt.tx.ptrmapPutOvflPtr(p, p.aData, cell)
	}
	return nil
}

// insertCellPointer is insertCell's tail: open slot i in the cell-pointer array,
// store idx there, and bump the cell count as the header's two bytes.
func (p *memPage) insertCellPointer(i, idx int) {
	data := p.aData
	ins := p.cellOffset + i*2
	copy(data[ins+2:], data[ins:ins+2*(p.nCell-i)])
	put2byte(data[ins:], idx)
	p.nCell++
	data[p.hdrOffset+4]++
	if data[p.hdrOffset+4] == 0 {
		data[p.hdrOffset+3]++
	}
}

// freePage2 returns a page to the freelist (excluding auto_vacuum pointer-map).
func (tx *btTxn) freePage2(mem *memPage, iPage uint32) error {
	bt := tx.bt
	page1 := tx.page1
	if iPage < 2 || iPage > tx.nPage {
		return errBtCorrupt
	}
	pg := mem
	if pg == nil {
		if cached := bt.pager.pages[iPage]; cached != nil && cached.loaded {
			pg = &cached.mem // btreePageLookup
		}
	}
	defer func() {
		if pg != nil {
			pg.isInit = false
		}
	}()

	bt.pager.write(page1)
	nFree := get4byte(page1.aData[36:])
	put4byte(page1.aData[36:], nFree+1)

	if bt.secureDelete {
		if pg == nil {
			var err error
			if pg, err = bt.getPage(iPage, false); err != nil {
				return err
			}
		}
		bt.pager.write(pg)
		clear(pg.aData[:bt.pageSize])
	}
	if bt.autoVacuum {
		if err := tx.ptrmapPut(iPage, ptrmapFreePage, 0); err != nil {
			return err
		}
	}

	var iTrunk uint32
	if nFree != 0 {
		iTrunk = get4byte(page1.aData[32:])
		if iTrunk > tx.nPage {
			return errBtCorrupt
		}
		trunk, err := bt.getPage(iTrunk, false)
		if err != nil {
			return err
		}
		nLeaf := get4byte(trunk.aData[4:])
		if nLeaf > uint32(bt.usableSize/4-2) {
			return errBtCorrupt
		}
		if nLeaf < uint32(bt.usableSize/4-8) {
			bt.pager.write(trunk)
			put4byte(trunk.aData[4:], nLeaf+1)
			put4byte(trunk.aData[8+nLeaf*4:], iPage)
			if pg != nil && !bt.secureDelete {
				bt.pager.dontWrite(pg)
			}
			tx.setHasContent(iPage)
			return nil
		}
	}

	if pg == nil {
		var err error
		if pg, err = bt.getPage(iPage, false); err != nil {
			return err
		}
	}
	bt.pager.write(pg)
	put4byte(pg.aData, iTrunk)
	put4byte(pg.aData[4:], 0)
	put4byte(page1.aData[32:], iPage)
	return nil
}

// Modes for allocateBtreePage freelist search.
const (
	btallocAny   = 0
	btallocExact = 1
	btallocLE    = 2
)

// allocateBtreePage allocates a page from the freelist, searching by eMode.
// BTALLOC_EXACT and BTALLOC_LE search for nearby pages (auto_vacuum only);
// BTALLOC_ANY takes the closest leaf from the first trunk.
func (tx *btTxn) allocateBtreePage(nearby uint32, eMode int) (*memPage, uint32, error) {
	bt := tx.bt
	page1 := tx.page1
	mxPage := tx.nPage
	n := get4byte(page1.aData[36:])
	if n >= mxPage {
		return nil, 0, errBtCorrupt
	}
	if n == 0 {
		return tx.allocateAtEnd()
	}

	searchList := false
	switch eMode {
	case btallocExact:
		if nearby <= mxPage {
			eType, _, err := tx.ptrmapGet(nearby)
			if err != nil {
				return nil, 0, err
			}
			searchList = eType == ptrmapFreePage
		}
	case btallocLE:
		searchList = true
	}

	bt.pager.write(page1)
	put4byte(page1.aData[36:], n-1)

	var trunk, prevTrunk *memPage
	nSearch := uint32(0)
	for {
		prevTrunk = trunk
		var iTrunk uint32
		if prevTrunk != nil {
			iTrunk = get4byte(prevTrunk.aData)
		} else {
			iTrunk = get4byte(page1.aData[32:])
		}
		if iTrunk > mxPage || nSearch > n {
			return nil, 0, errBtCorrupt
		}
		nSearch++
		var err error
		if trunk, err = bt.getUnusedPage(iTrunk, false); err != nil {
			return nil, 0, err
		}
		k := get4byte(trunk.aData[4:])
		switch {
		case k == 0 && !searchList:
			bt.pager.write(trunk)
			copy(page1.aData[32:36], trunk.aData[0:4])
			return trunk, iTrunk, nil
		case k > uint32(bt.usableSize/4-2):
			return nil, 0, errBtCorrupt
		case searchList && (nearby == iTrunk || (iTrunk < nearby && eMode == btallocLE)):
			bt.pager.write(trunk)
			if k == 0 {
				if prevTrunk == nil {
					copy(page1.aData[32:36], trunk.aData[0:4])
				} else {
					bt.pager.write(prevTrunk)
					copy(prevTrunk.aData[0:4], trunk.aData[0:4])
				}
				return trunk, iTrunk, nil
			}
			iNewTrunk := get4byte(trunk.aData[8:])
			if iNewTrunk > mxPage {
				return nil, 0, errBtCorrupt
			}
			newTrunk, err := bt.getUnusedPage(iNewTrunk, false)
			if err != nil {
				return nil, 0, err
			}
			bt.pager.write(newTrunk)
			copy(newTrunk.aData[0:4], trunk.aData[0:4])
			put4byte(newTrunk.aData[4:], k-1)
			copy(newTrunk.aData[8:8+(k-1)*4], trunk.aData[12:12+(k-1)*4])
			if prevTrunk == nil {
				put4byte(page1.aData[32:], iNewTrunk)
			} else {
				bt.pager.write(prevTrunk)
				put4byte(prevTrunk.aData[0:], iNewTrunk)
			}
			return trunk, iTrunk, nil
		case k > 0:
			data := trunk.aData
			closest := uint32(0)
			if nearby > 0 {
				if eMode == btallocLE {
					for i := range k {
						if get4byte(data[8+i*4:]) <= nearby {
							closest = i
							break
						}
					}
				} else {
					dist := absInt32(int32(get4byte(data[8:]) - nearby))
					for i := uint32(1); i < k; i++ {
						if d2 := absInt32(int32(get4byte(data[8+i*4:]) - nearby)); d2 < dist {
							closest, dist = i, d2
						}
					}
				}
			}
			iPage := get4byte(data[8+closest*4:])
			if iPage > mxPage || iPage < 2 {
				return nil, 0, errBtCorrupt
			}
			if !searchList || iPage == nearby || (iPage < nearby && eMode == btallocLE) {
				bt.pager.write(trunk)
				if closest < k-1 {
					copy(data[8+closest*4:12+closest*4], data[4+k*4:8+k*4])
				}
				put4byte(data[4:], k-1)
				pg, err := bt.getUnusedPage(iPage, !tx.getHasContent(iPage))
				if err != nil {
					return nil, 0, err
				}
				bt.pager.write(pg)
				return pg, iPage, nil
			}
		}
		if !searchList {
			return nil, 0, errBtCorrupt
		}
	}
}

// allocateAtEnd allocates a new page at the end of the file, with a
// pointer-map page first on auto_vacuum when needed.
func (tx *btTxn) allocateAtEnd() (*memPage, uint32, error) {
	bt := tx.bt
	noContent := !tx.doTruncate
	bt.pager.write(tx.page1)
	tx.nPage++
	if tx.nPage == bt.pendingBytePage() {
		tx.nPage++
	}
	if bt.autoVacuum && bt.ptrmapIsPage(tx.nPage) {
		pg, err := bt.getUnusedPage(tx.nPage, noContent)
		if err != nil {
			return nil, 0, err
		}
		bt.pager.write(pg)
		tx.nPage++
		if tx.nPage == bt.pendingBytePage() {
			tx.nPage++
		}
	}
	put4byte(tx.page1.aData[28:], tx.nPage)
	pgno := tx.nPage
	pg, err := bt.getUnusedPage(pgno, noContent)
	if err != nil {
		return nil, 0, err
	}
	bt.pager.write(pg)
	return pg, pgno, nil
}

// absInt32 returns the absolute value of x, mapping the smallest int32 to the largest.
func absInt32(x int32) int32 {
	if x >= 0 {
		return x
	}
	if x == -1<<31 {
		return 1<<31 - 1
	}
	return -x
}
