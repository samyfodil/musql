package engine

import (
	"fmt"
	"slices"
)

// A DATABASE READ OUT WHOLE, for a converter to another format.
//
// The other half of build.go: the catalog in sqlite_master order, every table's
// stored rows in rowid order, and each index's entries as the engine keys them.
// What a converter does with them is its own business; the engine computes
// everything that needs SQL to compute -- an expression index's key, a partial
// index's WHERE -- so no converter evaluates an expression.

// CatalogEntry is one row of sqlite_master.
type CatalogEntry struct {
	Type, Name, TblName string
	// SQL is "" for an automatic index.
	SQL   string
	Rowid int64
	// Storage is whether the entry has rows of its own: an ordinary table or
	// an index. A view, a trigger, a virtual table has none.
	Storage bool
	// AliasOf names the table whose rows this one reads instead of its own --
	// a catalog edit that pointed its storage at another table's
	// (ConvertedCatalog.RootEdits).
	AliasOf string
}

// Reader is a database opened to be read out.
type Reader struct {
	s *Session
}

// OpenReader opens the database at path. Close releases it without writing.
func OpenReader(path string) (*Reader, error) {
	s, err := OpenWrite(path)
	if err != nil {
		return nil, err
	}
	return &Reader{s: s}, nil
}

// Close releases the database.
func (r *Reader) Close() error { return r.s.Discard() }

// Meta is the database's Meta.
func (r *Reader) Meta() Meta {
	db := r.s.DB
	return Meta{
		SchemaVersion: db.schemaCookie, UserVersion: db.userVersion, ApplicationID: db.applicationID,
		Encoding: db.encoding(), PageSize: db.pageSize, AutoVacuum: db.autoVacuum, WAL: db.segWAL,
	}
}

// Catalog is the main database's catalog in rowid order.
func (r *Reader) Catalog() ([]CatalogEntry, error) {
	sp, err := r.s.SnapshotPager()
	if err != nil {
		return nil, err
	}
	all, err := sp.Schema()
	if err != nil {
		return nil, err
	}
	var rows []SchemaRow
	for _, sr := range all {
		if !sr.Temp {
			rows = append(rows, sr)
		}
	}
	if !schemaRowidsUsable(rows) {
		for i := range rows {
			rows[i].Rowid = int64(i + 1)
		}
	}
	slices.SortStableFunc(rows, func(a, b SchemaRow) int { return int(a.Rowid - b.Rowid) })
	out := make([]CatalogEntry, len(rows))
	for i, sr := range rows {
		out[i] = CatalogEntry{Type: sr.Type, Name: sr.Name, TblName: sr.TblName, SQL: sr.SQL, Rowid: sr.Rowid, Storage: catalogRowHasStorage(sr), AliasOf: sr.AliasOf}
	}
	return out, nil
}

func (r *Reader) table(name string) (*tableMeta, error) {
	t := r.s.findTableMetaIn(scopeMain, name)
	if t == nil {
		return nil, fmt.Errorf("engine: no table %s", name)
	}
	if err := r.s.ensureTableLoaded(t); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *Reader) index(name string) (*indexMeta, error) {
	idx := r.s.findIndexMetaIn(scopeMain, name)
	if idx == nil {
		return nil, fmt.Errorf("engine: no index %s", name)
	}
	return idx, nil
}

// Shape is a table's TableShape.
func (r *Reader) Shape(table string) (TableShape, error) {
	t := r.s.findTableMetaIn(scopeMain, table)
	if t == nil {
		return TableShape{}, fmt.Errorf("engine: no table %s", table)
	}
	return TableShapeOf(t.sql)
}

// IndexOrder is how an index's entries are ordered: key, one KeyColumn per
// key field, then trailer, the fields every entry ends with -- the rowid
// (Column -1), or a WITHOUT ROWID table's key columns the index does not
// already hold.
func (r *Reader) IndexOrder(index string) (key, trailer []KeyColumn, err error) {
	idx, err := r.index(index)
	if err != nil {
		return nil, nil, err
	}
	t := r.s.findTableMetaIn(scopeMain, idx.table)
	if t == nil {
		return nil, nil, fmt.Errorf("engine: index %s: no table %s", index, idx.table)
	}
	var keyCols []int
	var coll []string
	var desc []bool
	if !idx.exprOrPartial {
		keyCols, coll, desc = idx.colIdx, idx.colCollation, idx.colDesc
	} else {
		for _, k := range idx.keys {
			keyCols, coll, desc = append(keyCols, k.colIdx), append(coll, k.collation), append(desc, k.desc)
		}
	}
	for i, ci := range keyCols {
		key = append(key, KeyColumn{Column: ci, Collation: coll[i], Desc: desc[i]})
	}
	if !t.withoutRowid {
		return key, []KeyColumn{{Column: -1}}, nil
	}
	trailIdx, trailColl := indexTrailingKeyColsAndCollation(t, keyCols, coll)
	for i, ci := range trailIdx {
		trailer = append(trailer, KeyColumn{Column: ci, Collation: trailColl[i]})
	}
	return key, trailer, nil
}

// Rows calls fn with each row of table in rowid order: its stored row (as
// BuildTable.Add takes one) and its entry in each of indexes, nil where a
// partial index leaves the row out.
func (r *Reader) Rows(table string, indexes []string, fn func(rowid int64, stored []Value, entries [][]Value) error) error {
	t, err := r.table(table)
	if err != nil {
		return err
	}
	idxs := make([]*indexMeta, len(indexes))
	for i, n := range indexes {
		if idxs[i], err = r.index(n); err != nil {
			return err
		}
	}
	entries := make([][]Value, len(idxs))
	t.rows.eachSortedUntil(func(rowid uint64, vals []Value) bool {
		for i, idx := range idxs {
			key, ok, kerr := indexEntryOf(t, idx, rowid, vals)
			if kerr != nil {
				err = kerr
				return false
			}
			entries[i] = nil
			if ok {
				entries[i] = key
			}
		}
		err = fn(int64(rowid), contractToStoredRow(t.cols, vals), entries)
		return err == nil
	})
	if err != nil {
		return fmt.Errorf("engine: %s: %w", table, err)
	}
	return nil
}

// indexEntryOf is one row's entry in idx: its key, then the rowid -- or a
// WITHOUT ROWID table's key columns. ok is false when a partial index's WHERE
// leaves the row out.
func indexEntryOf(tbl *tableMeta, idx *indexMeta, rowid uint64, vals []Value) ([]Value, bool, error) {
	var key []Value
	var keyCols []int
	if !idx.exprOrPartial {
		keyCols = idx.colIdx
		for _, ci := range idx.colIdx {
			key = append(key, indexColumnValue(tbl, rowid, vals, ci))
		}
	} else {
		whereProg, keyProgs := compileIndexExprs(tbl, idx)
		ctx := rowEvalCtx(tbl, rowid, vals, nil, nil, nil)
		if idx.where != nil {
			v, err := whereProg.eval(ctx)
			if err != nil {
				return nil, false, fmt.Errorf("index %s: WHERE: %w", idx.name, err)
			}
			if !isTruthy(v) {
				return nil, false, nil
			}
		}
		for i, k := range idx.keys {
			keyCols = append(keyCols, k.colIdx)
			if k.colIdx >= 0 {
				key = append(key, indexColumnValue(tbl, rowid, vals, k.colIdx))
				continue
			}
			v, err := keyProgs[i].eval(ctx)
			if err != nil {
				return nil, false, fmt.Errorf("index %s: %w", idx.name, err)
			}
			key = append(key, v)
		}
	}
	if tbl.withoutRowid {
		trailIdx, _ := indexTrailingKeyColsAndCollation(tbl, keyCols, nil)
		for _, ci := range trailIdx {
			key = append(key, vals[ci])
		}
	} else {
		key = append(key, Value{Typ: Int, I: int64(rowid)})
	}
	return key, true, nil
}
