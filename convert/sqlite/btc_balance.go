package sqlite

// This file implements btree page balancing: balance_quick, balance_nonroot
// and its helpers, and balance_deeper. It decides which cells land on which
// page and where.

// nb is the number of pages a balance_nonroot involves.
const nb = 3

// cellArray holds cell data during balancing. apEnd[k] marks the end of the
// page the cells up to ixNx[k] came from.
type cellArray struct {
	nCell  int
	ref    *memPage
	apCell []btCell
	szCell []int
	apEnd  [nb * 2]btCell
	ixNx   [nb * 2]int
}

// cachedCellSize caches the computed size of a cell.
func (b *cellArray) cachedCellSize(n int) int {
	if b.szCell[n] == 0 {
		b.szCell[n] = b.ref.cellSize(b.apCell[n].b())
	}
	return b.szCell[n]
}

// populateCellCache populates the cell size cache for a range of cells.
func (b *cellArray) populateCellCache(idx, n int) {
	for ; n > 0; idx, n = idx+1, n-1 {
		b.cachedCellSize(idx)
	}
}

// crossesEnd is the pSrcEnd corruption test: a cell that starts before the end
// of the page it came from but runs past it.
func crossesEnd(c btCell, sz int, end btCell) bool {
	return sameBuf(c.buf, end.buf) && c.off+sz > end.off && c.off < end.off
}

// rebuildPage rebuilds a page with the given set of cells.
func (b *cellArray) rebuildPage(iFirst, nCell int, pg *memPage) error {
	hdr := pg.hdrOffset
	aData := pg.aData
	usableSize := pg.bt.usableSize
	i := iFirst
	iEnd := i + nCell
	cellptr := pg.cellOffset
	j := get2byte(aData[hdr+5:])
	if j > usableSize {
		j = 0
	}
	tmp := make([]byte, pg.bt.pageSize)
	copy(tmp[j:usableSize], aData[j:usableSize])

	k := 0
	for b.ixNx[k] <= i {
		k++
	}
	srcEnd := b.apEnd[k]

	pData := usableSize
	for {
		cell := b.apCell[i]
		sz := b.szCell[i]
		src := cell.b()
		if cell.within(aData, j, usableSize) {
			if cell.off+sz > usableSize {
				return errBtCorrupt
			}
			src = tmp[cell.off:]
		} else if crossesEnd(cell, sz, srcEnd) {
			return errBtCorrupt
		}
		pData -= sz
		put2byte(aData[cellptr:], pData)
		cellptr += 2
		if pData < cellptr {
			return errBtCorrupt
		}
		copy(aData[pData:pData+sz], src[:sz])
		i++
		if i >= iEnd {
			break
		}
		if b.ixNx[k] <= i {
			k++
			srcEnd = b.apEnd[k]
		}
	}

	pg.nCell = nCell
	pg.nOverflow = 0
	put2byte(aData[hdr+1:], 0)
	put2byte(aData[hdr+3:], pg.nCell)
	put2byte(aData[hdr+5:], pData)
	aData[hdr+7] = 0
	return nil
}

// pageInsertArray inserts cells into a page. Returns false if cells don't fit.
func (b *cellArray) pageInsertArray(pg *memPage, begin int, pData *int, cellptr, iFirst, nCell int) bool {
	aData := pg.aData
	data := *pData
	i := iFirst
	iEnd := iFirst + nCell
	if iEnd <= iFirst {
		return true
	}
	k := 0
	for b.ixNx[k] <= i {
		k++
	}
	end := b.apEnd[k]
	for {
		sz := b.szCell[i]
		slot := 0
		if aData[1] != 0 || aData[2] != 0 {
			slot, _ = pg.pageFindSlot(sz)
		}
		if slot == 0 {
			if data-begin < sz {
				return false
			}
			data -= sz
			slot = data
		}
		if crossesEnd(b.apCell[i], sz, end) {
			return false
		}
		copy(aData[slot:slot+sz], b.apCell[i].b()[:sz])
		put2byte(aData[cellptr:], slot)
		cellptr += 2
		i++
		if i >= iEnd {
			break
		}
		if b.ixNx[k] <= i {
			k++
			end = b.apEnd[k]
		}
	}
	*pData = data
	return true
}

// pageFreeArray marks cells as free. Returns the count of freed cells.
func (b *cellArray) pageFreeArray(pg *memPage, iFirst, nCell int) int {
	aData := pg.aData
	usableSize := pg.bt.usableSize
	start := pg.hdrOffset + 8 + pg.childPtrSize
	nRet := 0
	iEnd := iFirst + nCell
	nFree := 0
	var aOfst, aAfter [10]int
	for i := iFirst; i < iEnd; i++ {
		cell := b.apCell[i]
		if !cell.within(aData, start, usableSize) {
			continue
		}
		sz := b.szCell[i]
		iOfst := cell.off
		iAfter := iOfst + sz
		j := 0
		for ; j < nFree; j++ {
			if aOfst[j] == iAfter {
				aOfst[j] = iOfst
				break
			} else if aAfter[j] == iOfst {
				aAfter[j] = iAfter
				break
			}
		}
		if j >= nFree {
			if nFree >= len(aOfst) {
				for j := range nFree {
					pg.freeSpace(aOfst[j], aAfter[j]-aOfst[j])
				}
				nFree = 0
			}
			aOfst[nFree] = iOfst
			aAfter[nFree] = iAfter
			if iAfter > usableSize {
				return 0
			}
			nFree++
		}
		nRet++
	}
	for j := range nFree {
		pg.freeSpace(aOfst[j], aAfter[j]-aOfst[j])
	}
	return nRet
}

// editPage updates page cell ranges by removing old cells and adding new ones.
func (b *cellArray) editPage(pg *memPage, iOld, iNew, nNew int) error {
	aData := pg.aData
	hdr := pg.hdrOffset
	begin := pg.cellOffset + nNew*2
	nCell := pg.nCell
	iOldEnd := iOld + pg.nCell + pg.nOverflow
	iNewEnd := iNew + nNew

	if !b.editPageInPlace(pg, aData, hdr, begin, nCell, iOld, iNew, nNew, iOldEnd, iNewEnd) {
		if nNew < 1 {
			return errBtCorrupt
		}
		b.populateCellCache(iNew, nNew)
		return b.rebuildPage(iNew, nNew, pg)
	}
	return nil
}

// editPageInPlace performs in-place edits. Returns false to trigger a rebuild.
func (b *cellArray) editPageInPlace(pg *memPage, aData []byte, hdr, begin, nCell, iOld, iNew, nNew, iOldEnd, iNewEnd int) bool {
	if iOld < iNew {
		nShift := b.pageFreeArray(pg, iOld, iNew-iOld)
		if nShift > nCell {
			return false
		}
		copy(aData[pg.cellOffset:], aData[pg.cellOffset+nShift*2:pg.cellOffset+nShift*2+nCell*2])
		nCell -= nShift
	}
	if iNewEnd < iOldEnd {
		nCell -= b.pageFreeArray(pg, iNewEnd, iOldEnd-iNewEnd)
	}

	data := get2byte(aData[hdr+5:])
	if data < begin || data > pg.bt.pageSize {
		return false
	}

	if iNew < iOld {
		nAdd := min(nNew, iOld-iNew)
		cellptr := pg.cellOffset
		copy(aData[cellptr+nAdd*2:], aData[cellptr:cellptr+nCell*2])
		if !b.pageInsertArray(pg, begin, &data, cellptr, iNew, nAdd) {
			return false
		}
		nCell += nAdd
	}

	for i := range pg.nOverflow {
		iCell := (iOld + pg.aiOvfl[i]) - iNew
		if iCell >= 0 && iCell < nNew {
			cellptr := pg.cellOffset + iCell*2
			if nCell > iCell {
				copy(aData[cellptr+2:], aData[cellptr:cellptr+(nCell-iCell)*2])
			}
			nCell++
			b.cachedCellSize(iCell + iNew)
			if !b.pageInsertArray(pg, begin, &data, cellptr, iCell+iNew, 1) {
				return false
			}
		}
	}

	cellptr := pg.cellOffset + nCell*2
	if !b.pageInsertArray(pg, begin, &data, cellptr, iNew+nCell, nNew-nCell) {
		return false
	}

	pg.nCell = nNew
	pg.nOverflow = 0
	put2byte(aData[hdr+3:], pg.nCell)
	put2byte(aData[hdr+5:], data)
	return true
}

// balanceQuick handles overflow by moving one cell to a new page. space holds
// the divider cell until it is inserted into the parent.
func (tx *btTxn) balanceQuick(parent, page *memPage, space []byte) error {
	bt := page.bt
	if page.nCell == 0 {
		return errBtCorrupt
	}
	pNew, pgnoNew, err := tx.allocateBtreePage(0, btallocAny)
	if err != nil {
		return err
	}
	cell := page.apOvfl[0]
	szCell := page.cellSize(cell.b())
	pNew.zeroPage(ptfIntKey | ptfLeafData | ptfLeaf)
	b := cellArray{nCell: 1, ref: page, apCell: []btCell{cell}, szCell: []int{szCell}}
	b.apEnd[0] = btCell{page.aData, bt.pageSize}
	b.ixNx[0] = 2
	b.ixNx[nb*2-1] = 0x7fffffff
	if err := b.rebuildPage(0, 1, pNew); err != nil {
		return err
	}
	pNew.nFree = bt.usableSize - pNew.cellOffset - 2 - szCell
	var rc error
	if bt.autoVacuum {
		rc = tx.ptrmapPut(pgnoNew, ptrmapBtree, parent.pgno)
		if rc == nil && szCell > pNew.minLocal {
			rc = tx.ptrmapPutOvflPtr(pNew, pNew.aData, cell)
		}
	}

	// The divider is the page number of page followed by its largest key.
	last := page.findCell(page.nCell - 1)
	i := 0
	for i < 9 && last[i]&0x80 != 0 {
		i++
	}
	i++
	out := 4
	for stop := i + 9; ; {
		c := last[i]
		space[out] = c
		out++
		i++
		if c&0x80 == 0 || i >= stop {
			break
		}
	}
	if rc == nil {
		rc = parent.insertCell(parent.nCell, btCell{space, 0}, out, nil, page.pgno)
	}
	put4byte(parent.aData[parent.hdrOffset+8:], pgnoNew)
	return rc
}

// copyNodeContent copies the content of one page into another.
func (tx *btTxn) copyNodeContent(from, to *memPage) error {
	bt := from.bt
	aFrom, aTo := from.aData, to.aData
	fromHdr := from.hdrOffset
	toHdr := 0
	if to.pgno == 1 {
		toHdr = HeaderSize
	}
	iData := get2byte(aFrom[fromHdr+5:])
	copy(aTo[iData:bt.usableSize], aFrom[iData:bt.usableSize])
	n := from.cellOffset + 2*from.nCell
	copy(aTo[toHdr:toHdr+n], aFrom[fromHdr:fromHdr+n])
	to.isInit = false
	if err := to.initPage(); err != nil {
		return err
	}
	if err := to.computeFreeSpace(); err != nil {
		return err
	}
	if bt.autoVacuum {
		return tx.setChildPtrmaps(to)
	}
	return nil
}

// balanceNonroot rebalances a non-root page with its siblings and parent.
func (tx *btTxn) balanceNonroot(parent *memPage, iParentIdx int, ovflSpace []byte, isRoot, bulk bool) error {
	bt := parent.bt
	bBulk := 0
	if bulk {
		bBulk = 1
	}
	var (
		apOld          [nb]*memPage
		apNew          [nb + 2]*memPage
		aPgno          [nb + 2]uint32
		apDiv          [nb - 1]btCell
		cntNew, cntOld [nb + 2]int
		szNew          [nb + 2]int
		abDone         [nb + 2]bool
		b              cellArray
		nMaxCells      int
		nNew           int
		nxDiv          int
		iSpace1        int
		iOvflSpace     int
	)
	b.ixNx[nb*2-1] = 0x7fffffff

	i := parent.nOverflow + parent.nCell
	if i < 2 {
		nxDiv = 0
	} else {
		switch iParentIdx {
		case 0:
			nxDiv = 0
		case i:
			nxDiv = i - 2 + bBulk
		default:
			nxDiv = iParentIdx - 1
		}
		i = 2 - bBulk
	}
	nOld := i + 1
	var pRight int
	if i+nxDiv-parent.nOverflow == parent.nCell {
		pRight = parent.hdrOffset + 8
	} else {
		pRight = parent.findCellOff(i + nxDiv - parent.nOverflow)
	}
	pgno := get4byte(parent.aData[pRight:])
	for {
		old, err := bt.getAndInitPage(pgno, tx.nPage)
		if err != nil {
			return err
		}
		apOld[i] = old
		if old.nFree < 0 {
			if err := old.computeFreeSpace(); err != nil {
				return err
			}
		}
		nMaxCells += old.nCell + len(parent.apOvfl)
		i--
		if i < 0 {
			break
		}
		if parent.nOverflow > 0 && i+nxDiv == parent.aiOvfl[0] {
			apDiv[i] = parent.apOvfl[0]
			pgno = get4byte(apDiv[i].b())
			szNew[i] = parent.cellSize(apDiv[i].b())
			parent.nOverflow = 0
		} else {
			off := parent.findCellOff(i + nxDiv - parent.nOverflow)
			apDiv[i] = btCell{parent.aData, off}
			pgno = get4byte(apDiv[i].b())
			szNew[i] = parent.cellSize(apDiv[i].b())
			if bt.secureDelete && off+szNew[i] <= bt.usableSize {
				copy(ovflSpace[off:off+szNew[i]], parent.aData[off:off+szNew[i]])
				apDiv[i] = btCell{ovflSpace, off}
			}
			if err := parent.dropCell(i+nxDiv-parent.nOverflow, szNew[i]); err != nil {
				return err
			}
		}
	}

	nMaxCells = (nMaxCells + 3) &^ 3
	b.apCell = make([]btCell, nMaxCells)
	b.szCell = make([]int, nMaxCells)
	space1 := make([]byte, bt.pageSize)

	b.ref = apOld[0]
	leafCorrection := 0
	if b.ref.leaf {
		leafCorrection = 4
	}
	leafData := b.ref.intKeyLeaf
	// notLeafData is the inverse of leafData as integer (0 or 1).
	notLeafData := 1
	if leafData {
		notLeafData = 0
	}
	for i := range nOld {
		old := apOld[i]
		limit := old.nCell
		piCell := old.cellOffset
		if old.aData[0] != apOld[0].aData[0] {
			return errBtCorrupt
		}
		if old.nOverflow > 0 {
			if limit < old.aiOvfl[0] {
				return errBtCorrupt
			}
			limit = old.aiOvfl[0]
			for range limit {
				b.apCell[b.nCell] = btCell{old.aData, get2byte(old.aData[piCell:]) & (bt.pageSize - 1)}
				piCell += 2
				b.nCell++
			}
			for k := range old.nOverflow {
				b.apCell[b.nCell] = old.apOvfl[k]
				b.nCell++
			}
		}
		for piEnd := old.cellOffset + 2*old.nCell; piCell < piEnd; piCell += 2 {
			b.apCell[b.nCell] = btCell{old.aData, get2byte(old.aData[piCell:]) & (bt.pageSize - 1)}
			b.nCell++
		}

		cntOld[i] = b.nCell
		if i < nOld-1 && !leafData {
			sz := szNew[i]
			b.szCell[b.nCell] = sz
			temp := iSpace1
			iSpace1 += sz
			copy(space1[temp:temp+sz], apDiv[i].b()[:sz])
			b.apCell[b.nCell] = btCell{space1, temp + leafCorrection}
			b.szCell[b.nCell] -= leafCorrection
			if !old.leaf {
				copy(b.apCell[b.nCell].b()[:4], old.aData[8:12])
			} else {
				for b.szCell[b.nCell] < 4 {
					space1[iSpace1] = 0
					iSpace1++
					b.szCell[b.nCell]++
				}
			}
			b.nCell++
		}
	}

	usableSpace := bt.usableSize - 12 + leafCorrection
	k := 0
	for i := 0; i < nOld; i, k = i+1, k+1 {
		p := apOld[i]
		b.apEnd[k] = btCell{p.aData, bt.pageSize}
		b.ixNx[k] = cntOld[i]
		if k > 0 && b.ixNx[k] == b.ixNx[k-1] {
			k--
		}
		if !leafData {
			k++
			b.apEnd[k] = btCell{parent.aData, bt.pageSize}
			b.ixNx[k] = cntOld[i] + 1
		}
		szNew[i] = usableSpace - p.nFree
		for j := range p.nOverflow {
			szNew[i] += 2 + p.cellSize(p.apOvfl[j].b())
		}
		cntNew[i] = cntOld[i]
	}
	k = nOld
	for i := 0; i < k; i++ {
		for szNew[i] > usableSpace {
			if i+1 >= k {
				k = i + 2
				if k > nb+2 {
					return errBtCorrupt
				}
				szNew[k-1] = 0
				cntNew[k-1] = b.nCell
			}
			sz := 2 + b.cachedCellSize(cntNew[i]-1)
			szNew[i] -= sz
			if !leafData {
				if cntNew[i] < b.nCell {
					sz = 2 + b.cachedCellSize(cntNew[i])
				} else {
					sz = 0
				}
			}
			szNew[i+1] += sz
			cntNew[i]--
		}
		for cntNew[i] < b.nCell {
			sz := 2 + b.cachedCellSize(cntNew[i])
			if szNew[i]+sz > usableSpace {
				break
			}
			szNew[i] += sz
			cntNew[i]++
			if !leafData {
				if cntNew[i] < b.nCell {
					sz = 2 + b.cachedCellSize(cntNew[i])
				} else {
					sz = 0
				}
			}
			szNew[i+1] -= sz
		}
		prev := 0
		if i > 0 {
			prev = cntNew[i-1]
		}
		if cntNew[i] >= b.nCell {
			k = i + 1
		} else if cntNew[i] <= prev {
			return errBtCorrupt
		}
	}

	for i := k - 1; i > 0; i-- {
		szRight := szNew[i]
		szLeft := szNew[i-1]
		r := cntNew[i-1] - 1
		d := r + notLeafData
		b.cachedCellSize(d)
		for {
			szR := b.cachedCellSize(r)
			szD := b.szCell[d]
			last := 2
			if i == k-1 {
				last = 0
			}
			if szRight != 0 && (bulk || szRight+szD+2 > szLeft-(szR+last)) {
				break
			}
			szRight += szD + 2
			szLeft -= szR + 2
			cntNew[i-1] = r
			r--
			d--
			if r < 0 {
				break
			}
		}
		szNew[i] = szRight
		szNew[i-1] = szLeft
		prev := 0
		if i > 1 {
			prev = cntNew[i-2]
		}
		if cntNew[i-1] <= prev {
			return errBtCorrupt
		}
	}

	pageFlags := int(apOld[0].aData[0])
	for i := range k {
		if i < nOld {
			apNew[i] = apOld[i]
			apOld[i] = nil
			bt.pager.write(apNew[i])
			nNew++
			continue
		}
		nearby := pgno
		if bulk {
			nearby = 1
		}
		pNew, newPgno, err := tx.allocateBtreePage(nearby, btallocAny)
		if err != nil {
			return err
		}
		pgno = newPgno
		pNew.zeroPage(pageFlags)
		apNew[i] = pNew
		nNew++
		cntOld[i] = b.nCell
		if bt.autoVacuum {
			if err := tx.ptrmapPut(pNew.pgno, ptrmapBtree, parent.pgno); err != nil {
				return err
			}
		}
	}

	for i := range nNew {
		aPgno[i] = apNew[i].pgno
	}

	for i := 0; i < nNew-1; i++ {
		iB := i
		for j := i + 1; j < nNew; j++ {
			if apNew[j].pgno < apNew[iB].pgno {
				iB = j
			}
		}
		if iB != i {
			bt.pager.rekey(apNew[i], apNew[iB])
		}
	}

	put4byte(parent.aData[pRight:], apNew[nNew-1].pgno)

	if pageFlags&ptfLeaf == 0 && nOld != nNew {
		var old *memPage
		if nNew > nOld {
			old = apNew[nOld-1]
		} else {
			old = apOld[nOld-1]
		}
		copy(apNew[nNew-1].aData[8:12], old.aData[8:12])
	}

	if bt.autoVacuum {
		pOld, pNew := apNew[0], apNew[0]
		cntOldNext := pNew.nCell + pNew.nOverflow
		iNew, iOld := 0, 0
		for i := range b.nCell {
			cell := b.apCell[i]
			for i == cntOldNext {
				iOld++
				if iOld < nNew {
					pOld = apNew[iOld]
				} else {
					pOld = apOld[iOld]
				}
				cntOldNext += pOld.nCell + pOld.nOverflow + notLeafData
			}
			if i == cntNew[iNew] {
				iNew++
				pNew = apNew[iNew]
				if !leafData {
					continue
				}
			}
			if iOld >= nNew || pNew.pgno != aPgno[iOld] || !cell.within(pOld.aData, 0, bt.pageSize) {
				if leafCorrection == 0 {
					if err := tx.ptrmapPut(get4byte(cell.b()), ptrmapBtree, pNew.pgno); err != nil {
						return err
					}
				}
				if b.cachedCellSize(i) > pNew.minLocal {
					if err := tx.ptrmapPutOvflPtr(pNew, pOld.aData, cell); err != nil {
						return err
					}
				}
			}
		}
	}

	for i := 0; i < nNew-1; i++ {
		pNew := apNew[i]
		j := cntNew[i]
		cell := b.apCell[j]
		sz := b.szCell[j] + leafCorrection
		temp := &btCell{ovflSpace, iOvflSpace}
		switch {
		case !pNew.leaf:
			copy(pNew.aData[8:12], cell.b()[:4])
		case leafData:
			var info cellInfo
			j--
			pNew.parseCell(b.apCell[j].b(), &info)
			cell = *temp
			sz = 4 + putVarint(cell.b()[4:], uint64(info.nKey))
			temp = nil
		default:
			cell.off -= 4
			if b.szCell[j] == 4 {
				sz = parent.cellSize(cell.b())
			}
		}
		iOvflSpace += sz
		k := 0
		for b.ixNx[k] <= j {
			k++
		}
		srcEnd := b.apEnd[k]
		if sameBuf(cell.buf, srcEnd.buf) && srcEnd.off > cell.off && cell.off+sz > srcEnd.off {
			return errBtCorrupt
		}
		if err := parent.insertCell(nxDiv+i, cell, sz, temp, pNew.pgno); err != nil {
			return err
		}
	}

	for i := 1 - nNew; i < nNew; i++ {
		iPg := i
		if i < 0 {
			iPg = -i
		}
		if abDone[iPg] {
			continue
		}
		if i >= 0 || cntOld[iPg-1] >= cntNew[iPg-1] {
			var iNew, iOld, nNewCell int
			if iPg == 0 {
				nNewCell = cntNew[0]
			} else {
				if iPg < nOld {
					iOld = cntOld[iPg-1] + notLeafData
				} else {
					iOld = b.nCell
				}
				iNew = cntNew[iPg-1] + notLeafData
				nNewCell = cntNew[iPg] - iNew
			}
			if err := b.editPage(apNew[iPg], iOld, iNew, nNewCell); err != nil {
				return err
			}
			abDone[iPg] = true
			apNew[iPg].nFree = usableSpace - szNew[iPg]
		}
	}

	if isRoot && parent.nCell == 0 && parent.hdrOffset <= apNew[0].nFree {
		if err := apNew[0].defragmentPage(-1); err != nil {
			return err
		}
		if err := tx.copyNodeContent(apNew[0], parent); err != nil {
			return err
		}
		if err := tx.freePage2(apNew[0], apNew[0].pgno); err != nil {
			return err
		}
	} else if bt.autoVacuum && leafCorrection == 0 {
		for i := range nNew {
			if err := tx.ptrmapPut(get4byte(apNew[i].aData[8:]), ptrmapBtree, apNew[i].pgno); err != nil {
				return err
			}
		}
	}

	for i := nNew; i < nOld; i++ {
		if err := tx.freePage2(apOld[i], apOld[i].pgno); err != nil {
			return err
		}
	}
	return nil
}

// balanceDeeper handles overflow in the root by allocating a new child page.
func (tx *btTxn) balanceDeeper(root *memPage) (*memPage, error) {
	bt := root.bt
	bt.pager.write(root)
	child, pgnoChild, err := tx.allocateBtreePage(root.pgno, btallocAny)
	if err != nil {
		return nil, err
	}
	if err := tx.copyNodeContent(root, child); err != nil {
		return nil, err
	}
	if bt.autoVacuum {
		if err := tx.ptrmapPut(pgnoChild, ptrmapBtree, root.pgno); err != nil {
			return nil, err
		}
	}
	child.aiOvfl = root.aiOvfl
	child.apOvfl = root.apOvfl
	child.nOverflow = root.nOverflow
	root.zeroPage(int(child.aData[0]) &^ ptfLeaf)
	put4byte(root.aData[root.hdrOffset+8:], pgnoChild)
	return child, nil
}

// balance rebalances pages when a page overflows or is too empty.
func (cur *btCursor) balance() error {
	tx := cur.tx
	var quickSpace [13]byte
	for {
		page := cur.page
		if page.nFree < 0 {
			if err := page.computeFreeSpace(); err != nil {
				return nil
			}
		}
		if page.nOverflow == 0 && page.nFree*3 <= tx.bt.usableSize*2 {
			return nil
		}
		iPage := cur.iPage
		if iPage == 0 {
			if page.nOverflow == 0 {
				return nil
			}
			child, err := tx.balanceDeeper(page)
			if err != nil {
				return err
			}
			cur.iPage = 1
			cur.ix = 0
			cur.aiIdx[0] = 0
			cur.apPage[0] = page
			cur.apPage[1] = child
			cur.page = child
			continue
		}
		parent := cur.apPage[iPage-1]
		iIdx := cur.aiIdx[iPage-1]
		tx.bt.pager.write(parent)
		if parent.nFree < 0 {
			if err := parent.computeFreeSpace(); err != nil {
				return err
			}
		}
		var err error
		if page.intKeyLeaf && page.nOverflow == 1 && page.aiOvfl[0] == page.nCell &&
			parent.pgno != 1 && parent.nCell == iIdx {
			err = tx.balanceQuick(parent, page, quickSpace[:])
		} else {
			err = tx.balanceNonroot(parent, iIdx, make([]byte, tx.bt.pageSize), iPage == 1, cur.bulkLoad)
		}
		page.nOverflow = 0
		if err != nil {
			return err
		}
		cur.iPage--
		cur.page = cur.apPage[cur.iPage]
	}
}
