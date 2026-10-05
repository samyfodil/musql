package sqlite

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/samyfodil/musql/engine"
)

// EXPORT: A MUSQL DATABASE WRITTEN AS A SQLITE FILE.
//
// The file is built page by page from what the engine reads out: storage roots
// allocated in catalog order, sqlite_schema table, table rows, and index entries.
// Objects are built in catalog order (table, rows, then indexes) to maintain
// compatibility with SQLite's layout.

// Export writes the musql database at src to a SQLite file at dst.
// pageSize is 0 for the database's own page size.
func Export(src, dst string, pageSize int) error {
	r, err := engine.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	m := r.Meta()
	if pageSize <= 0 {
		pageSize = int(m.PageSize)
	}
	if pageSize <= 0 {
		pageSize = 4096
	}
	img, err := image(r, m, pageSize)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, img, 0o644)
}

// autoIndex is one automatic index an export fills alongside its table's rows.
type autoIndex struct {
	name string
	root uint32
	ki   *btKeyInfo
}

// ErrSharedStorage is Export's refusal of a database with a table that reads
// another table's rows: the SQLite form of that is a catalog row naming the
// other's b-tree, which an export does not write.
var ErrSharedStorage = errors.New("sqlite: export: a table shares another table's storage (a rootpage edit), which has no SQLite form an export writes")

func image(r *engine.Reader, m engine.Meta, pageSize int) ([]byte, error) {
	rows, err := r.Catalog()
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		if e.AliasOf != "" {
			return nil, ErrSharedStorage
		}
	}
	pager := newBtPager(pageSize, 0, true, func(uint32, []byte) error { return errBtCorrupt })
	bt := newBtShared(pager, pageSize, 0)
	bt.autoVacuum = m.AutoVacuum != 0
	bt.incrVacuum = m.AutoVacuum == 2
	tx := &btTxn{bt: bt}
	bt.tx = tx
	if tx.page1, err = bt.getPage(1, false); err != nil {
		return nil, err
	}
	tx.newDatabase()
	enc := TextEncoding(m.Encoding)
	if enc != UTF16LE && enc != UTF16BE {
		enc = UTF8
	}
	if len(rows) > 0 {
		tx.updateMeta(2, 4) // file format version
		tx.updateMeta(5, uint32(enc))
	}

	schemaRec := func(e engine.CatalogEntry, root uint32) []byte {
		sql := Value{Typ: Null}
		if e.SQL != "" {
			sql = Value{Typ: Text, S: []byte(e.SQL)}
		}
		return encodeRecordEnc([]Value{
			{Typ: Text, S: []byte(e.Type)}, {Typ: Text, S: []byte(e.Name)}, {Typ: Text, S: []byte(e.TblName)},
			{Typ: Int, I: int64(root)}, sql,
		}, enc)
	}
	// Process objects in catalog order: each table with its rows, then indexes.
	done := make([]bool, len(rows))
	for i, e := range rows {
		if done[i] {
			continue
		}
		switch {
		case e.Type == "table" && e.Storage:
			shape, err := r.Shape(e.Name)
			if err != nil {
				return nil, err
			}
			flags := btreeIntKey
			if shape.WithoutRowid {
				flags = btreeBlobKey
			}
			// Create the b-tree and placeholder catalog row.
			root, err := tx.createTable(flags)
			if err != nil {
				return nil, err
			}
			if err := pagePutRow(tx, 1, e.Rowid, catalogNullRow); err != nil {
				return nil, err
			}
			// Its automatic indexes, created inside the same CREATE TABLE.
			var autos []autoIndex
			var autoNames []string
			for j := i + 1; j < len(rows); j++ {
				ir := rows[j]
				if ir.Type != "index" || ir.SQL != "" || !strings.EqualFold(ir.TblName, e.Name) {
					continue
				}
				ki, err := indexKeyInfo(r, ir.Name, shape, enc)
				if err != nil {
					return nil, err
				}
				ixRoot, err := tx.createTable(btreeBlobKey)
				if err != nil {
					return nil, err
				}
				if err := pagePutRow(tx, 1, ir.Rowid, schemaRec(ir, ixRoot)); err != nil {
					return nil, err
				}
				autos = append(autos, autoIndex{name: ir.Name, root: ixRoot, ki: ki})
				autoNames = append(autoNames, ir.Name)
				done[j] = true
			}
			if err := pagePutRow(tx, 1, e.Rowid, schemaRec(e, root)); err != nil {
				return nil, err
			}
			// The rows, as INSERT writes them: the row, then each automatic
			// index's entry for it.
			sp := storedPositions(shape)
			err = r.Rows(e.Name, autoNames, func(rowid int64, stored []Value, entries [][]Value) error {
				if shape.WithoutRowid {
					rec := wrRecord(shape, sp, stored)
					if err := exportIndexInsert(tx, root, wrKeyInfo(shape, enc, len(rec)), encodeRecordEnc(rec, enc)); err != nil {
						return err
					}
				} else if err := pagePutRow(tx, root, rowid, encodeRecordEnc(stored, enc)); err != nil {
					return err
				}
				for k, a := range autos {
					if entries[k] == nil {
						continue
					}
					a.ki.nAllField = len(entries[k])
					if err := exportIndexInsert(tx, a.root, a.ki, encodeRecordEnc(entries[k], enc)); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("export: %w", err)
			}
		case e.Type == "index" && e.Storage:
			// Create the index b-tree and catalog row, then refill with sorted entries.
			shape, err := r.Shape(e.TblName)
			if err != nil {
				return nil, err
			}
			ki, err := indexKeyInfo(r, e.Name, shape, enc)
			if err != nil {
				return nil, err
			}
			root, err := tx.createTable(btreeBlobKey)
			if err != nil {
				return nil, err
			}
			if err := pagePutRow(tx, 1, e.Rowid, schemaRec(e, root)); err != nil {
				return nil, err
			}
			if err := exportRefillIndex(tx, r, e, root, ki, enc); err != nil {
				return nil, fmt.Errorf("export: index %s: %w", e.Name, err)
			}
		default:
			// A view, a trigger, a virtual table: a catalog row and no b-tree.
			if err := pagePutRow(tx, 1, e.Rowid, schemaRec(e, 0)); err != nil {
				return nil, err
			}
		}
	}
	tx.updateMeta(1, m.SchemaVersion)
	tx.updateMeta(6, m.UserVersion)
	tx.updateMeta(8, m.ApplicationID)
	if bt.autoVacuum {
		if err := tx.autoVacuumCommit(); err != nil {
			return nil, err
		}
	}
	// A fresh file after one commit, as VACUUM INTO writes it.
	bt.pager.write(tx.page1)
	put4byte(tx.page1.aData[24:], 1)
	put4byte(tx.page1.aData[28:], bt.pager.dbSize)
	put4byte(tx.page1.aData[92:], 1)
	put4byte(tx.page1.aData[96:], btSQLiteVersionNumber)
	if m.WAL {
		tx.page1.aData[18], tx.page1.aData[19] = 2, 2 // a WAL database's read/write versions
	}
	return bt.pager.commitInto(nil)
}

// indexKeyInfo returns an index b-tree's KeyInfo with collations and sort orders,
// including trailing PRIMARY KEY columns for WITHOUT ROWID tables.
func indexKeyInfo(r *engine.Reader, index string, shape engine.TableShape, enc TextEncoding) (*btKeyInfo, error) {
	key, trailer, err := r.IndexOrder(index)
	if err != nil {
		return nil, err
	}
	ki := &btKeyInfo{enc: enc}
	for _, k := range key {
		ki.coll = append(ki.coll, k.Collation)
		ki.desc = append(ki.desc, k.Desc)
	}
	if shape.WithoutRowid {
		for _, k := range trailer {
			ki.coll = append(ki.coll, k.Collation)
		}
	}
	return ki, nil
}

// exportRefillIndex fills an index's b-tree with entries in sorted order.
func exportRefillIndex(tx *btTxn, r *engine.Reader, e engine.CatalogEntry, root uint32, ki *btKeyInfo, enc TextEncoding) error {
	var recs [][]byte
	err := r.Rows(e.TblName, []string{e.Name}, func(_ int64, _ []Value, entries [][]Value) error {
		if entries[0] != nil {
			ki.nAllField = len(entries[0])
			recs = append(recs, encodeRecordEnc(entries[0], enc))
		}
		return nil
	})
	if err != nil || len(recs) == 0 {
		return err
	}
	slices.SortStableFunc(recs, func(a, b []byte) int {
		key, uerr := ki.unpack(b)
		if uerr != nil {
			return 0
		}
		c, _ := key.recordCompare(a)
		return c
	})
	for i, rec := range recs {
		if err := exportIndexInsert(tx, root, ki, rec); err != nil {
			return err
		}
		if err := tx.bt.pager.shrink(tx.bt); err != nil {
			return err
		}
		recs[i] = nil
	}
	return nil
}

// exportIndexInsert puts one entry into an index b-tree.
func exportIndexInsert(tx *btTxn, root uint32, ki *btKeyInfo, rec []byte) error {
	cur := tx.openCursor(root, ki)
	defer cur.close()
	loc, err := cur.moveto(rec, int64(len(rec)), false)
	if err != nil {
		return err
	}
	return cur.insert(&btPayload{key: rec, nKey: int64(len(rec))}, 0, loc)
}

// newDatabase initializes page 1 of an empty database file.
func (tx *btTxn) newDatabase() {
	bt := tx.bt
	data := tx.page1.aData
	bt.pager.write(tx.page1)
	copy(data, headerMagic)
	data[16] = byte(bt.pageSize >> 8)
	data[17] = byte(bt.pageSize >> 16)
	data[18], data[19] = 1, 1
	data[20] = byte(bt.pageSize - bt.usableSize)
	data[21], data[22], data[23] = 64, 32, 32
	clear(data[24:100])
	tx.page1.zeroPage(ptfIntKey | ptfLeaf | ptfLeafData)
	if bt.autoVacuum {
		put4byte(data[36+4*4:], 1)
	}
	if bt.incrVacuum {
		put4byte(data[36+7*4:], 1)
	}
	tx.nPage = 1
	data[31] = 1
	bt.pager.dbSize = 1
}

// catalogNullRow is a placeholder record of five NULLs.
var catalogNullRow = []byte{6, 0, 0, 0, 0, 0}

// pagePutRow overwrites or inserts the row under rowid.
func pagePutRow(tx *btTxn, root uint32, rowid int64, rec []byte) error {
	cur := tx.openCursor(root, nil)
	defer cur.close()
	loc, err := cur.tableMoveto(rowid, false)
	if err != nil {
		return err
	}
	if loc == 0 {
		cur.getCellInfo()
	}
	return cur.insert(&btPayload{nKey: rowid, data: rec}, 0, loc)
}

// storedPositions maps each declared column to its position in a stored row,
// -1 for a VIRTUAL generated column, which a stored row skips.
func storedPositions(shape engine.TableShape) []int {
	sp := make([]int, len(shape.Columns))
	n := 0
	for ci := range shape.Columns {
		if shape.Virtual[ci] {
			sp[ci] = -1
			continue
		}
		sp[ci] = n
		n++
	}
	return sp
}

// wrKeyInfo is a WITHOUT ROWID table b-tree's KeyInfo: the key columns'
// collations and sort orders over records of nAll fields.
func wrKeyInfo(shape engine.TableShape, enc TextEncoding, nAll int) *btKeyInfo {
	ki := &btKeyInfo{enc: enc, nAllField: nAll}
	for _, k := range shape.PrimaryKey {
		ki.coll = append(ki.coll, k.Collation)
		ki.desc = append(ki.desc, k.Desc)
	}
	return ki
}

// wrRecord is the b-tree entry for a stored row: the key columns, then the
// rest in table order.
func wrRecord(shape engine.TableShape, sp []int, stored []Value) []Value {
	at := func(ci int) Value {
		if p := sp[ci]; p >= 0 && p < len(stored) {
			return stored[p]
		}
		return Value{Typ: Null}
	}
	isPK := make([]bool, len(shape.Columns))
	rec := make([]Value, 0, len(stored))
	for _, k := range shape.PrimaryKey {
		rec = append(rec, at(k.Column))
		isPK[k.Column] = true
	}
	for ci := range shape.Columns {
		if !isPK[ci] && sp[ci] >= 0 {
			rec = append(rec, at(ci))
		}
	}
	return rec
}

// storedFromWR is wrRecord's inverse: a WITHOUT ROWID b-tree entry put back
// into a stored row. An entry written before an ALTER TABLE ADD COLUMN is
// short, and so is the row: the columns it lacks are the trailing ones.
func storedFromWR(shape engine.TableShape, sp []int, rec []Value) []Value {
	n := 0
	for _, p := range sp {
		if p >= 0 {
			n++
		}
	}
	out := make([]Value, n)
	isPK := make([]bool, len(shape.Columns))
	ri := 0
	for _, k := range shape.PrimaryKey {
		if ri < len(rec) && sp[k.Column] >= 0 {
			out[sp[k.Column]] = rec[ri]
		}
		ri++
		isPK[k.Column] = true
	}
	for ci := range shape.Columns {
		if isPK[ci] || sp[ci] < 0 {
			continue
		}
		if ri < len(rec) {
			out[sp[ci]] = rec[ri]
		}
		ri++
	}
	return out[:min(len(rec), n)]
}
