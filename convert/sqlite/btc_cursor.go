package sqlite

// This file is btree.c's cursor: b-tree navigation and modification.
// Index searches are in btc_index.go.

import "slices"

// btcursorMaxDepth limits cursor depth (BTCURSOR_MAX_DEPTH from btreeInt.h).
const btcursorMaxDepth = 20

// Cursor states, btreeInt.h.
const (
	cursorValid = iota
	cursorInvalid
	cursorSkipNext
	cursorRequireSeek
	cursorFault
)

// BtCursor.curFlags bits (btreeInt.h).
const (
	btcfWriteFlag = 0x01
	btcfValidNKey = 0x02
	btcfAtLast    = 0x08
	btcfMultiple  = 0x20
)

// sqlite3BtreeInsert/Delete flag bits (btree.h).
const (
	btreeSavePosition = 0x02
	btreeAppend       = 0x08
)

// btCursor is BtCursor (btreeInt.h) for one b-tree of a transaction.
type btCursor struct {
	tx        *btTxn
	pgnoRoot  uint32
	eState    int
	curFlags  int
	skipNext  int
	iPage     int // -1 before the root is loaded
	ix        int
	aiIdx     [btcursorMaxDepth]int
	apPage    [btcursorMaxDepth]*memPage
	page      *memPage
	info      cellInfo
	curIntKey bool
	faultErr  error      // a CURSOR_FAULT cursor's error (BtCursor.skipNext in C)
	keyInfo   *btKeyInfo // nil for a table b-tree
	nKey      int64      // the saved key of a CURSOR_REQUIRESEEK cursor: a rowid, or pKey's length
	pKey      []byte     // an index cursor's saved key
	bulkLoad  bool       // BTREE_BULKLOAD hint

	// keyArena is where cellKey carves payload copies from; whole-table scans set it.
	keyArena *bump
}

// openCursor opens a write cursor, marking multiple cursors on the same root
// to save positions on write (btree.c:4689).
func (tx *btTxn) openCursor(root uint32, keyInfo *btKeyInfo) *btCursor {
	bt := tx.bt
	if root == 1 && tx.nPage == 0 {
		root = 0
	}
	cur := &btCursor{tx: tx, pgnoRoot: root, eState: cursorInvalid, iPage: -1, keyInfo: keyInfo, curFlags: btcfWriteFlag}
	for _, x := range bt.cursors {
		if x.pgnoRoot == root {
			x.curFlags |= btcfMultiple
			cur.curFlags |= btcfMultiple
		}
	}
	bt.cursors = append(bt.cursors, cur)
	return cur
}

// close closes a cursor (btree.c:4830).
func (cur *btCursor) close() {
	bt := cur.tx.bt
	if i := slices.Index(bt.cursors, cur); i >= 0 {
		bt.cursors = slices.Delete(bt.cursors, i, i+1)
	}
	cur.iPage = -1
	cur.pKey = nil
}

// releaseAllPages releases all pages held by a cursor (btree.c:690).
func (cur *btCursor) releaseAllPages() { cur.iPage = -1 }

// savePosition saves a cursor's key and releases pages (btree.c:756).
func (cur *btCursor) savePosition() error {
	if cur.eState == cursorSkipNext {
		cur.eState = cursorValid
	} else {
		cur.skipNext = 0
	}
	if err := cur.saveCursorKey(); err != nil {
		return err
	}
	cur.releaseAllPages()
	cur.eState = cursorRequireSeek
	cur.curFlags &^= btcfValidNKey | btcfAtLast
	return nil
}

// saveAllCursors saves positions of all cursors on a root except the given one (btree.c:806).
func (bt *btShared) saveAllCursors(root uint32, except *btCursor) error {
	found := false
	for _, p := range bt.cursors {
		if p == except || (root != 0 && p.pgnoRoot != root) {
			continue
		}
		found = true
		if p.eState == cursorValid || p.eState == cursorSkipNext {
			if err := p.savePosition(); err != nil {
				return err
			}
		} else {
			p.releaseAllPages()
		}
	}
	if !found && except != nil {
		except.curFlags &^= btcfMultiple
	}
	return nil
}

// getCellInfo parses the cell at the cursor's position (btree.c).
func (cur *btCursor) getCellInfo() {
	if cur.info.nSize == 0 {
		cur.curFlags |= btcfValidNKey
		cur.page.parseCellAt(cur.ix, &cur.info)
	}
}

// moveToChild navigates to a child page (btree.c:5454).
func (cur *btCursor) moveToChild(newPgno uint32) error {
	if cur.iPage >= btcursorMaxDepth-1 {
		return errBtCorrupt
	}
	cur.info.nSize = 0
	cur.curFlags &^= btcfValidNKey
	cur.aiIdx[cur.iPage] = cur.ix
	cur.apPage[cur.iPage] = cur.page
	cur.ix = 0
	cur.iPage++
	p, err := cur.tx.bt.getAndInitPage(newPgno, cur.tx.nPage)
	if err == nil && (p.nCell < 1 || p.intKey != cur.curIntKey) {
		err = errBtCorrupt
	}
	if err != nil {
		cur.iPage--
		cur.page = cur.apPage[cur.iPage]
		return err
	}
	cur.page = p
	return nil
}

// moveToParent navigates to a parent page (btree.c:5513).
func (cur *btCursor) moveToParent() {
	cur.info.nSize = 0
	cur.curFlags &^= btcfValidNKey
	cur.ix = cur.aiIdx[cur.iPage-1]
	cur.iPage--
	cur.page = cur.apPage[cur.iPage]
}

// moveToRoot navigates to the root page (btree.c:5554).
func (cur *btCursor) moveToRoot() (empty bool, err error) {
	var root *memPage
	switch {
	case cur.iPage > 0:
		cur.iPage = 0
		cur.page = cur.apPage[0]
		root = cur.page
	case cur.iPage == 0:
		root = cur.page
		if !root.isInit || (cur.keyInfo == nil) != root.intKey {
			return false, errBtCorrupt
		}
	case cur.pgnoRoot == 0:
		cur.eState = cursorInvalid
		return true, nil
	default:
		if cur.eState >= cursorRequireSeek {
			if cur.eState == cursorFault {
				return false, cur.faultErr
			}
			cur.pKey = nil
			cur.eState = cursorInvalid
		}
		p, err := cur.tx.bt.getAndInitPage(cur.pgnoRoot, cur.tx.nPage)
		if err != nil {
			cur.eState = cursorInvalid
			return false, err
		}
		cur.page = p
		cur.iPage = 0
		cur.curIntKey = p.intKey
		root = p
		if !root.isInit || (cur.keyInfo == nil) != root.intKey {
			return false, errBtCorrupt
		}
	}

	cur.ix = 0
	cur.info.nSize = 0
	cur.curFlags &^= btcfAtLast | btcfValidNKey
	switch {
	case root.nCell > 0:
		cur.eState = cursorValid
	case !root.leaf:
		if root.pgno != 1 {
			return false, errBtCorrupt
		}
		cur.eState = cursorValid
		return false, cur.moveToChild(get4byte(root.aData[root.hdrOffset+8:]))
	default:
		cur.eState = cursorInvalid
		return true, nil
	}
	return false, nil
}

// moveToLeftmost navigates to the leftmost cell (btree.c:5640).
func (cur *btCursor) moveToLeftmost() error {
	for !cur.page.leaf {
		if err := cur.moveToChild(get4byte(cur.page.findCell(cur.ix))); err != nil {
			return err
		}
	}
	return nil
}

// tableMoveto seeks to a row in a table by rowid (btree.c:5805).
func (cur *btCursor) tableMoveto(intKey int64, biasRight bool) (int, error) {
	if cur.eState == cursorValid && cur.curFlags&btcfValidNKey != 0 {
		if cur.info.nKey == intKey {
			return 0, nil
		}
		if cur.info.nKey < intKey {
			if cur.curFlags&btcfAtLast != 0 {
				return -1, nil
			}
			if cur.info.nKey+1 == intKey {
				done, err := cur.next()
				if err != nil {
					return 0, err
				}
				if !done {
					cur.getCellInfo()
					if cur.info.nKey == intKey {
						return 0, nil
					}
				}
			}
		}
	}

	empty, err := cur.moveToRoot()
	if err != nil {
		return 0, err
	}
	if empty {
		return -1, nil
	}
	for {
		page := cur.page
		lwr, upr := 0, page.nCell-1
		idx := upr / 2
		if biasRight {
			idx = upr
		}
		var c int
		descend := false
		for {
			cell := page.findCell(idx)[page.childPtrSize:]
			if page.intKeyLeaf {
				n := 0
				for cell[n] >= 0x80 {
					n++
					if n >= len(cell) {
						return 0, errBtCorrupt
					}
				}
				cell = cell[n+1:]
			}
			k, _ := getVarint(cell)
			nCellKey := int64(k)
			if nCellKey < intKey {
				lwr = idx + 1
				if lwr > upr {
					c = -1
					break
				}
			} else if nCellKey > intKey {
				upr = idx - 1
				if lwr > upr {
					c = 1
					break
				}
			} else {
				cur.ix = idx
				if !page.leaf {
					lwr = idx
					descend = true
					break
				}
				cur.curFlags |= btcfValidNKey
				cur.info.nKey = nCellKey
				cur.info.nSize = 0
				return 0, nil
			}
			idx = (lwr + upr) >> 1
		}
		if !descend && page.leaf {
			cur.ix = idx
			cur.info.nSize = 0
			return c, nil
		}
		var chldPg uint32
		if lwr >= page.nCell {
			chldPg = get4byte(page.aData[page.hdrOffset+8:])
		} else {
			chldPg = get4byte(page.findCell(lwr))
		}
		cur.ix = lwr
		if err := cur.moveToChild(chldPg); err != nil {
			cur.info.nSize = 0
			return 0, err
		}
	}
}

// restorePosition seeks back to a saved cursor position (btree.c:896).
func (cur *btCursor) restorePosition() error {
	if cur.eState == cursorFault {
		return cur.faultErr
	}
	cur.eState = cursorInvalid
	skip, err := cur.moveto(cur.pKey, cur.nKey, false)
	if err != nil {
		return err
	}
	cur.pKey = nil
	if skip != 0 {
		cur.skipNext = skip
	}
	if cur.skipNext != 0 && cur.eState == cursorValid {
		cur.eState = cursorSkipNext
	}
	return nil
}

// next moves to the next cell in b-tree order (btree.c:6384).
func (cur *btCursor) next() (done bool, err error) {
	cur.info.nSize = 0
	cur.curFlags &^= btcfValidNKey
	if cur.eState != cursorValid {
		if cur.eState >= cursorRequireSeek {
			if err := cur.restorePosition(); err != nil {
				return false, err
			}
		}
		if cur.eState == cursorInvalid {
			return true, nil
		}
		if cur.eState == cursorSkipNext {
			cur.eState = cursorValid
			if cur.skipNext > 0 {
				return false, nil
			}
		}
	}
	page := cur.page
	cur.ix++
	if !page.isInit {
		return false, errBtCorrupt
	}
	if cur.ix >= page.nCell {
		if !page.leaf {
			if err := cur.moveToChild(get4byte(page.aData[page.hdrOffset+8:])); err != nil {
				return false, err
			}
			return false, cur.moveToLeftmost()
		}
		for {
			if cur.iPage == 0 {
				cur.eState = cursorInvalid
				return true, nil
			}
			cur.moveToParent()
			if cur.ix < cur.page.nCell {
				break
			}
		}
		if cur.page.intKey {
			return cur.next()
		}
		return false, nil
	}
	if page.leaf {
		return false, nil
	}
	return false, cur.moveToLeftmost()
}

// overwriteContent writes only the differing bytes to keep pages clean (btree.c:9264).
func (tx *btTxn) overwriteContent(page *memPage, dest []byte, x *btPayload, iOffset, iAmt int) {
	nData := len(x.data) - iOffset
	if nData <= 0 {
		i := 0
		for i < iAmt && dest[i] == 0 {
			i++
		}
		if i < iAmt {
			tx.bt.pager.write(page)
			clear(dest[i:iAmt])
		}
		return
	}
	if nData < iAmt {
		tx.overwriteContent(page, dest[nData:], x, iOffset+nData, iAmt-nData)
		iAmt = nData
	}
	if string(dest[:iAmt]) != string(x.data[iOffset:iOffset+iAmt]) {
		tx.bt.pager.write(page)
		copy(dest[:iAmt], x.data[iOffset:iOffset+iAmt])
	}
}

// overwriteCell overwrites a cell's payload (btree.c:9359).
func (cur *btCursor) overwriteCell(x *btPayload) error {
	tx := cur.tx
	page := cur.page
	cell := page.findCell(cur.ix)
	nTotal := len(x.data) + x.nZero
	payload := cell[cur.info.payload:]
	if cur.info.nLocal > len(payload) {
		return errBtCorrupt
	}
	if cur.info.nLocal == nTotal {
		tx.overwriteContent(page, payload, x, 0, cur.info.nLocal)
		return nil
	}
	tx.overwriteContent(page, payload, x, 0, cur.info.nLocal)
	iOffset := cur.info.nLocal
	ovflPgno := get4byte(payload[iOffset:])
	ovflPageSize := tx.bt.usableSize - 4
	for {
		p, err := tx.bt.getPage(ovflPgno, false)
		if err != nil {
			return err
		}
		if p.isInit {
			return errBtCorrupt
		}
		if iOffset+ovflPageSize < nTotal {
			ovflPgno = get4byte(p.aData)
		} else {
			ovflPageSize = nTotal - iOffset
		}
		tx.overwriteContent(p, p.aData[4:], x, iOffset, ovflPageSize)
		iOffset += ovflPageSize
		if iOffset >= nTotal {
			return nil
		}
	}
}

// insert inserts a cell at the cursor position (btree.c:9409).
func (cur *btCursor) insert(x *btPayload, flags, seekResult int) error {
	tx := cur.tx
	loc := seekResult
	if cur.curFlags&btcfMultiple != 0 {
		if err := tx.bt.saveAllCursors(cur.pgnoRoot, cur); err != nil {
			return err
		}
		if loc != 0 && cur.iPage < 0 {
			return errBtCorrupt
		}
	}
	if cur.eState >= cursorRequireSeek {
		if _, err := cur.moveToRoot(); err != nil {
			return err
		}
	}
	if cur.keyInfo == nil {
		if cur.curFlags&btcfValidNKey != 0 && x.nKey == cur.info.nKey {
			if cur.info.nSize != 0 && cur.info.nPayload == uint32(len(x.data)+x.nZero) {
				return cur.overwriteCell(x)
			}
		} else if loc == 0 {
			var err error
			if loc, err = cur.tableMoveto(x.nKey, flags&btreeAppend != 0); err != nil {
				return err
			}
		}
	} else {
		if loc == 0 && flags&btreeSavePosition == 0 {
			var err error
			if loc, err = cur.moveto(x.key, x.nKey, flags&btreeAppend != 0); err != nil {
				return err
			}
		}
		if loc == 0 {
			cur.getCellInfo()
			if cur.info.nKey == x.nKey {
				return cur.overwriteCell(&btPayload{data: x.key})
			}
		}
	}

	page := cur.page
	if page.nFree < 0 {
		if cur.eState > cursorInvalid {
			return errBtCorrupt
		}
		if err := page.computeFreeSpace(); err != nil {
			return err
		}
	}
	tmp := tx.tmpSpace()
	newCell := tmp.b()
	szNew, err := tx.fillInCell(page, newCell, x)
	if err != nil {
		return err
	}
	idx := cur.ix
	cur.info.nSize = 0
	switch {
	case loc == 0:
		if idx >= page.nCell {
			return errBtCorrupt
		}
		tx.bt.pager.write(page)
		oldOff := page.findCellOff(idx)
		oldCell := page.aData[oldOff:]
		if !page.leaf {
			copy(newCell[:4], oldCell[:4])
		}
		var info cellInfo
		err := tx.clearCell(page, oldCell, &info)
		if info.nSize == szNew && uint32(info.nLocal) == info.nPayload && (!tx.bt.autoVacuum || szNew < page.minLocal) {
			if oldOff < page.hdrOffset+10 || oldOff+szNew > tx.bt.pageSize {
				return errBtCorrupt
			}
			copy(oldCell[:szNew], newCell[:szNew])
			return nil
		}
		if err != nil {
			return err
		}
		if err := page.dropCell(idx, info.nSize); err != nil {
			return err
		}
	case loc < 0 && page.nCell > 0:
		cur.ix++
		idx = cur.ix
		cur.curFlags &^= btcfValidNKey
	}
	if err := page.insertCellFast(idx, tmp, szNew); err != nil {
		return err
	}
	if page.nOverflow > 0 {
		cur.curFlags &^= btcfValidNKey
		err := cur.balance()
		cur.page.nOverflow = 0
		cur.eState = cursorInvalid
		if err != nil {
			return err
		}
		if flags&btreeSavePosition != 0 {
			cur.iPage = -1
			if cur.keyInfo != nil {
				cur.pKey = append([]byte(nil), x.key...)
			}
			cur.eState = cursorRequireSeek
			cur.nKey = x.nKey
		}
	}
	return nil
}

// saveCursorKey saves a cursor's key (rowid or index key) for later restore (btree.c).
func (cur *btCursor) saveCursorKey() error {
	cur.getCellInfo()
	if cur.curIntKey {
		cur.nKey = cur.info.nKey
		return nil
	}
	key, err := cur.cellKey(cur.page, cur.ix)
	if err != nil {
		return err
	}
	cur.nKey = int64(len(key))
	cur.pKey = key
	return nil
}

// tmpSpace returns a scratch buffer for building cells (btree.c:2893-2903).
func (tx *btTxn) tmpSpace() btCell {
	if tx.tmp == nil {
		tx.tmp = make([]byte, tx.bt.pageSize+4)
	}
	return btCell{tx.tmp, 4}
}
