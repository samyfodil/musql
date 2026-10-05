package sqlite

// This file implements whole-b-tree operations: creating, clearing, and
// dropping tables. On auto_vacuum databases, root pages stay at the front and
// blocking pages are moved.

// Flags for createTable.
const (
	btreeIntKey  = 1
	btreeBlobKey = 2
)

// btreeLargestRootPage is the index of the meta value tracking the largest root page.
const btreeLargestRootPage = 4

// createTable creates a new table, returning its root page number. The root is
// zeroed as an empty leaf. On auto_vacuum, it's placed after the largest
// existing root, and any blocking page is relocated.
func (tx *btTxn) createTable(flags int) (uint32, error) {
	bt := tx.bt
	var root *memPage
	var pgno uint32
	if bt.autoVacuum {
		pgno = get4byte(tx.page1.aData[36+btreeLargestRootPage*4:])
		if pgno > tx.nPage {
			return 0, errBtCorrupt
		}
		pgno++
		for pgno == bt.ptrmapPageno(pgno) || pgno == bt.pendingBytePage() {
			pgno++
		}
		pageMove, pgnoMove, err := tx.allocateBtreePage(pgno, btallocExact)
		if err != nil {
			return 0, err
		}
		if pgnoMove != pgno {
			if err := bt.saveAllCursors(0, nil); err != nil {
				return 0, err
			}
			moved, err := bt.getPage(pgno, false)
			if err != nil {
				return 0, err
			}
			eType, iPtrPage, err := tx.ptrmapGet(pgno)
			if err == nil && (eType == ptrmapRootPage || eType == ptrmapFreePage) {
				err = errBtCorrupt
			}
			if err == nil {
				err = tx.relocatePage(moved, eType, iPtrPage, pgnoMove)
			}
			if err != nil {
				return 0, err
			}
			if root, err = bt.getPage(pgno, false); err != nil {
				return 0, err
			}
			bt.pager.write(root)
		} else {
			root = pageMove
		}
		if err := tx.ptrmapPut(pgno, ptrmapRootPage, 0); err != nil {
			return 0, err
		}
		tx.updateMeta(btreeLargestRootPage, pgno)
	} else {
		var err error
		if root, pgno, err = tx.allocateBtreePage(1, btallocAny); err != nil {
			return 0, err
		}
	}
	if flags&btreeIntKey != 0 {
		root.zeroPage(ptfIntKey | ptfLeafData | ptfLeaf)
	} else {
		root.zeroPage(ptfZeroData | ptfLeaf)
	}
	return pgno, nil
}

// updateMeta updates a meta value at offset 36+4*idx in the database header.
func (tx *btTxn) updateMeta(idx int, value uint32) {
	tx.bt.pager.write(tx.page1)
	put4byte(tx.page1.aData[36+idx*4:], value)
	if idx == 7 {
		tx.bt.incrVacuum = value != 0
	}
}
