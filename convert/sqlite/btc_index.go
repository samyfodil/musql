package sqlite

// This file is the index half of btree cursor operations: moveto, cellKey
// lookup, and packed-record comparisons.

// btKeyInfo describes a key's columns: collation, sort order, encoding, and
// field count.
type btKeyInfo struct {
	coll      []string
	desc      []bool
	enc       TextEncoding
	nAllField int
}

// btKey holds an unpacked record for comparisons.
type btKey struct {
	info      *btKeyInfo
	vals      []Value
	nField    int
	defaultRC int
	eqSeen    bool
}

// unpack unpacks a packed record into fields, up to nAllField.
func (ki *btKeyInfo) unpack(rec []byte) (*btKey, error) {
	vals, err := decodeRecordEnc(rec, ki.enc)
	if err != nil {
		return nil, errBtCorrupt
	}
	if len(vals) > ki.nAllField {
		vals = vals[:ki.nAllField]
	}
	return &btKey{info: ki, vals: vals, nField: len(vals)}, nil
}

// recordCompare compares a packed record against an unpacked key. Returns a
// negative value if the record sorts first, positive if the key sorts first.
// Missing fields compare equal; the result is defaultRC when all match.
func (k *btKey) recordCompare(rec []byte) (int, error) {
	cell, err := decodeRecordEnc(rec, k.info.enc)
	if err != nil {
		return 0, errBtCorrupt
	}
	n := min(len(cell), k.nField, len(k.vals))
	for i := range n {
		coll := "BINARY"
		if i < len(k.info.coll) && k.info.coll[i] != "" {
			coll = k.info.coll[i]
		}
		if c := compareValuesCollatedEnc(cell[i], k.vals[i], coll, k.info.enc); c != 0 {
			if i < len(k.info.desc) && k.info.desc[i] {
				c = -c
			}
			return c, nil
		}
	}
	k.eqSeen = true
	return k.defaultRC, nil
}

// indexCellCompare compares a cell's record to a key if the record is wholly
// on the page; otherwise returns 99 (comparison not possible).
func (cur *btCursor) indexCellCompare(page *memPage, idx int, key *btKey) int {
	cell := page.findCell(idx)[page.childPtrSize:]
	n := int(cell[0])
	switch {
	case n <= page.max1bytePayload:
		if page.findCellOff(idx)+page.childPtrSize+n >= cur.tx.bt.pageSize {
			return 99
		}
		c, _ := key.recordCompare(cell[1 : 1+n])
		return c
	case cell[1]&0x80 == 0:
		if n = (n&0x7f)<<7 + int(cell[1]); n <= page.maxLocal {
			if page.findCellOff(idx)+page.childPtrSize+n >= cur.tx.bt.pageSize {
				return 99
			}
			c, _ := key.recordCompare(cell[2 : 2+n])
			return c
		}
	}
	return 99
}

// cursorOnLastPage reports whether the cursor is on the last cell of each page.
func (cur *btCursor) cursorOnLastPage() bool {
	for i := range cur.iPage {
		if cur.aiIdx[i] < cur.apPage[i].nCell {
			return false
		}
	}
	return true
}

// cellKey retrieves the full key from a cell, including local and overflow bytes.
func (cur *btCursor) cellKey(page *memPage, idx int) ([]byte, error) {
	var info cellInfo
	cell := page.findCell(idx)
	page.parseCell(cell, &info)
	n := int(info.nPayload)
	out := cur.keyArena.take(n)
	local := min(info.nLocal, n)
	if info.payload+local > len(cell) {
		return nil, errBtCorrupt
	}
	out = append(out, cell[info.payload:info.payload+local]...)
	if len(out) == n {
		return out, nil
	}
	ovfl := get4byte(cell[info.payload+info.nLocal:])
	for len(out) < n {
		if ovfl < 2 || ovfl > cur.tx.nPage {
			return nil, errBtCorrupt
		}
		p, err := cur.tx.bt.getPage(ovfl, false)
		if err != nil {
			return nil, err
		}
		take := min(n-len(out), cur.tx.bt.usableSize-4)
		out = append(out, p.aData[4:4+take]...)
		ovfl = get4byte(p.aData)
	}
	return out, nil
}

// indexMoveto positions the cursor at the cell matching the given key.
func (cur *btCursor) indexMoveto(key *btKey) (int, error) {
	bypass := false
	if cur.eState == cursorValid && cur.page.leaf && cur.cursorOnLastPage() {
		if cur.ix == cur.page.nCell-1 {
			if c := cur.indexCellCompare(cur.page, cur.ix, key); c <= 0 {
				return c, nil
			}
		}
		if cur.iPage > 0 && cur.indexCellCompare(cur.page, 0, key) <= 0 {
			cur.curFlags &^= btcfAtLast
			if !cur.page.isInit {
				return 0, errBtCorrupt
			}
			bypass = true
		}
	}
	if !bypass {
		empty, err := cur.moveToRoot()
		if err != nil {
			return 0, err
		}
		if empty {
			return -1, nil
		}
	}

	for {
		page := cur.page
		lwr, upr := 0, page.nCell-1
		idx := upr >> 1
		var c int
		for {
			cellOff := page.findCellOff(idx) + page.childPtrSize
			cell := page.aData[cellOff:]
			n := int(cell[0])
			var err error
			switch {
			case n <= page.max1bytePayload:
				if cellOff+n >= cur.tx.bt.pageSize {
					cur.info.nSize = 0
					return 0, errBtCorrupt
				}
				c, err = key.recordCompare(cell[1 : 1+n])
			case cell[1]&0x80 == 0 && (n&0x7f)<<7+int(cell[1]) <= page.maxLocal && cellOff+(n&0x7f)<<7+int(cell[1]) < cur.tx.bt.pageSize:
				n = (n&0x7f)<<7 + int(cell[1])
				c, err = key.recordCompare(cell[2 : 2+n])
			default:
				page.parseCell(page.aData[cellOff-page.childPtrSize:], &cur.info)
				if int(cur.info.nKey) < 2 || int(cur.info.nKey)/cur.tx.bt.usableSize > int(cur.tx.nPage) {
					cur.info.nSize = 0
					return 0, errBtCorrupt
				}
				cur.ix = idx
				var rec []byte
				if rec, err = cur.cellKey(page, idx); err == nil {
					c, err = key.recordCompare(rec)
				}
			}
			if err != nil {
				cur.info.nSize = 0
				return 0, err
			}
			switch {
			case c < 0:
				lwr = idx + 1
			case c > 0:
				upr = idx - 1
			default:
				cur.ix = idx
				cur.info.nSize = 0
				return 0, nil
			}
			if lwr > upr {
				break
			}
			idx = (lwr + upr) >> 1
		}
		if page.leaf {
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

// moveto is btreeMoveto (btree.c:860): a packed key is unpacked and searched
// for in an index; nKey alone is a table's rowid.
func (cur *btCursor) moveto(pKey []byte, nKey int64, bias bool) (int, error) {
	if cur.keyInfo == nil {
		return cur.tableMoveto(nKey, bias)
	}
	key, err := cur.keyInfo.unpack(pKey)
	if err != nil {
		return 0, err
	}
	if key.nField == 0 || key.nField > cur.keyInfo.nAllField {
		return 0, errBtCorrupt
	}
	return cur.indexMoveto(key)
}
