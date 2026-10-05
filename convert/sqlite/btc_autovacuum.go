package sqlite

// This file is btree.c's auto_vacuum machinery: pointer-map pages that track
// parent pointers, page relocation, and incremental vacuum.

// ptrmapPageno returns the pointer-map page for pgno (btree.c:1036).
func (bt *btShared) ptrmapPageno(pgno uint32) uint32 {
	if pgno < 2 {
		return 0
	}
	perPage := uint32(bt.usableSize/5) + 1
	ret := (pgno-2)/perPage*perPage + 2
	if ret == bt.pendingBytePage() {
		ret++
	}
	return ret
}

// ptrmapIsPage reports whether pgno is a pointer-map page.
func (bt *btShared) ptrmapIsPage(pgno uint32) bool { return bt.ptrmapPageno(pgno) == pgno }

// ptrmapPut writes an entry, marking the page only if it changes (btree.c:1060).
func (tx *btTxn) ptrmapPut(key uint32, eType byte, parent uint32) error {
	bt := tx.bt
	if key == 0 {
		return errBtCorrupt
	}
	iPtrmap := bt.ptrmapPageno(key)
	p, err := bt.getPage(iPtrmap, false)
	if err != nil {
		return err
	}
	if p.isInit {
		return errBtCorrupt
	}
	offset := int(5 * (key - iPtrmap - 1))
	if int(key)-int(iPtrmap)-1 < 0 {
		return errBtCorrupt
	}
	if eType != p.aData[offset] || get4byte(p.aData[offset+1:]) != parent {
		bt.pager.write(p)
		p.aData[offset] = eType
		put4byte(p.aData[offset+1:], parent)
	}
	return nil
}

// ptrmapGet is btree.c:1119.
func (tx *btTxn) ptrmapGet(key uint32) (eType byte, parent uint32, err error) {
	bt := tx.bt
	iPtrmap := bt.ptrmapPageno(key)
	p, err := bt.getPage(iPtrmap, false)
	if err != nil {
		return 0, 0, err
	}
	if int(key)-int(iPtrmap)-1 < 0 {
		return 0, 0, errBtCorrupt
	}
	offset := int(5 * (key - iPtrmap - 1))
	eType = p.aData[offset]
	parent = get4byte(p.aData[offset+1:])
	if eType < 1 || eType > 5 {
		return eType, parent, errBtCorrupt
	}
	return eType, parent, nil
}

// ptrmapPutOvflPtr records the first overflow page for a cell (btree.c:1582).
func (tx *btTxn) ptrmapPutOvflPtr(page *memPage, src []byte, cell btCell) error {
	var info cellInfo
	page.parseCell(cell.b(), &info)
	if uint32(info.nLocal) < info.nPayload {
		if sameBuf(cell.buf, src) && cell.off+info.nLocal > tx.bt.pageSize {
			return errBtCorrupt
		}
		ovfl := get4byte(cell.b()[info.nSize-4:])
		return tx.ptrmapPut(ovfl, ptrmapOverflow1, page.pgno)
	}
	return nil
}

// setChildPtrmaps is btree.c:3835.
func (tx *btTxn) setChildPtrmaps(page *memPage) error {
	if !page.isInit {
		if err := page.initPage(); err != nil {
			return err
		}
	}
	pgno := page.pgno
	for i := range page.nCell {
		cell := btCell{page.aData, page.findCellOff(i)}
		if err := tx.ptrmapPutOvflPtr(page, page.aData, cell); err != nil {
			return err
		}
		if !page.leaf {
			if err := tx.ptrmapPut(get4byte(cell.b()), ptrmapBtree, pgno); err != nil {
				return err
			}
		}
	}
	if !page.leaf {
		return tx.ptrmapPut(get4byte(page.aData[page.hdrOffset+8:]), ptrmapBtree, pgno)
	}
	return nil
}

// modifyPagePointer updates references in a page from iFrom to iTo (btree.c:3880).
func (tx *btTxn) modifyPagePointer(page *memPage, iFrom, iTo uint32, eType byte) error {
	usable := tx.bt.usableSize
	if eType == ptrmapOverflow2 {
		if get4byte(page.aData) != iFrom {
			return errBtCorrupt
		}
		put4byte(page.aData, iTo)
		return nil
	}
	if !page.isInit {
		if err := page.initPage(); err != nil {
			return err
		}
	}
	for i := range page.nCell {
		off := page.findCellOff(i)
		cell := page.aData[off:]
		if eType == ptrmapOverflow1 {
			var info cellInfo
			page.parseCell(cell, &info)
			if uint32(info.nLocal) < info.nPayload {
				if off+info.nSize > usable {
					return errBtCorrupt
				}
				if get4byte(cell[info.nSize-4:]) == iFrom {
					put4byte(cell[info.nSize-4:], iTo)
					return nil
				}
			}
		} else {
			if off+4 > usable {
				return errBtCorrupt
			}
			if get4byte(cell) == iFrom {
				put4byte(cell, iTo)
				return nil
			}
		}
	}
	if eType != ptrmapBtree || get4byte(page.aData[page.hdrOffset+8:]) != iFrom {
		return errBtCorrupt
	}
	put4byte(page.aData[page.hdrOffset+8:], iTo)
	return nil
}

// relocatePage moves a page and updates all references (btree.c:3944).
func (tx *btTxn) relocatePage(page *memPage, eType byte, iPtrPage, iFreePage uint32) error {
	bt := tx.bt
	iDbPage := page.pgno
	if iDbPage < 3 {
		return errBtCorrupt
	}
	bt.pager.movePage(page, iFreePage)

	if eType == ptrmapBtree || eType == ptrmapRootPage {
		if err := tx.setChildPtrmaps(page); err != nil {
			return err
		}
	} else if next := get4byte(page.aData); next != 0 {
		if err := tx.ptrmapPut(next, ptrmapOverflow2, iFreePage); err != nil {
			return err
		}
	}

	if eType != ptrmapRootPage {
		ptrPage, err := bt.getPage(iPtrPage, false)
		if err != nil {
			return err
		}
		bt.pager.write(ptrPage)
		if err := tx.modifyPagePointer(ptrPage, iDbPage, iFreePage, eType); err != nil {
			return err
		}
		return tx.ptrmapPut(iFreePage, eType, iPtrPage)
	}
	return nil
}

// incrVacuumStep performs one incremental vacuum step (btree.c:4038).
func (tx *btTxn) incrVacuumStep(nFin, iLastPg uint32, bCommit bool) (done bool, err error) {
	bt := tx.bt
	if !bt.ptrmapIsPage(iLastPg) && iLastPg != bt.pendingBytePage() {
		if get4byte(tx.page1.aData[36:]) == 0 {
			return true, nil
		}
		eType, iPtrPage, err := tx.ptrmapGet(iLastPg)
		if err != nil {
			return false, err
		}
		if eType == ptrmapRootPage {
			return false, errBtCorrupt
		}
		if eType == ptrmapFreePage {
			if !bCommit {
				_, iFreePg, err := tx.allocateBtreePage(iLastPg, btallocExact)
				if err != nil {
					return false, err
				}
				if iFreePg != iLastPg {
					return false, errBtCorrupt
				}
			}
		} else {
			lastPg, err := bt.getPage(iLastPg, false)
			if err != nil {
				return false, err
			}
			eMode, iNear := btallocAny, uint32(0)
			if !bCommit {
				eMode, iNear = btallocLE, nFin
			}
			var iFreePg uint32
			for {
				dbSize := tx.nPage
				if _, iFreePg, err = tx.allocateBtreePage(iNear, eMode); err != nil {
					return false, err
				}
				if iFreePg > dbSize {
					return false, errBtCorrupt
				}
				if !bCommit || iFreePg <= nFin {
					break
				}
			}
			if err := tx.relocatePage(lastPg, eType, iPtrPage, iFreePg); err != nil {
				return false, err
			}
		}
	}
	if !bCommit {
		for {
			iLastPg--
			if iLastPg != bt.pendingBytePage() && !bt.ptrmapIsPage(iLastPg) {
				break
			}
		}
		tx.doTruncate = true
		tx.nPage = iLastPg
	}
	return false, nil
}

// finalDbSize is btree.c:4139.
func (bt *btShared) finalDbSize(nOrig, nFree uint32) uint32 {
	nEntry := uint32(bt.usableSize / 5)
	nPtrmap := (nFree - nOrig + bt.ptrmapPageno(nOrig) + nEntry) / nEntry
	nFin := nOrig - nFree - nPtrmap
	if nOrig > bt.pendingBytePage() && nFin < bt.pendingBytePage() {
		nFin--
	}
	for bt.ptrmapIsPage(nFin) || nFin == bt.pendingBytePage() {
		nFin--
	}
	return nFin
}

// autoVacuumCommit performs vacuum on commit (btree.c:4202).
func (tx *btTxn) autoVacuumCommit() error {
	bt := tx.bt
	if bt.incrVacuum {
		return nil
	}
	nOrig := tx.nPage
	if bt.ptrmapIsPage(nOrig) || nOrig == bt.pendingBytePage() {
		return errBtCorrupt
	}
	nFree := get4byte(tx.page1.aData[36:])
	nVac := nFree
	nFin := bt.finalDbSize(nOrig, nVac)
	if nFin > nOrig {
		return errBtCorrupt
	}
	if nFin < nOrig {
		if err := bt.saveAllCursors(0, nil); err != nil {
			return err
		}
	}
	for iFree := nOrig; iFree > nFin; iFree-- {
		done, err := tx.incrVacuumStep(nFin, iFree, nVac == nFree)
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	if nFree > 0 {
		bt.pager.write(tx.page1)
		if nVac == nFree {
			put4byte(tx.page1.aData[32:], 0)
			put4byte(tx.page1.aData[36:], 0)
		}
		put4byte(tx.page1.aData[28:], nFin)
		tx.doTruncate = true
		tx.nPage = nFin
	}
	return nil
}
