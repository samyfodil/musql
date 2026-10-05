// This file reads the sqlite_schema table (formerly sqlite_master): the
// table b-tree rooted at page 1 that records every table, index, view, and
// trigger in the database. See
// https://www.sqlite.org/fileformat2.html#storage_of_the_sql_database_schema
package engine

import "fmt"

// schemaRootPage is the fixed root page of sqlite_schema.
const schemaRootPage = 1

// SchemaRow is one row of sqlite_schema: Type is "table", "index", "view", or
// "trigger"; Name is the object's own name; TblName is the table it belongs
// to (equal to Name for tables themselves); RootPage is 0 for objects with no
// root page (views, triggers, and WITHOUT ROWID shadow entries can vary); SQL
// is the original CREATE statement, or empty if there isn't one (e.g.
// automatic sqlite_autoindex_* entries).
type SchemaRow struct {
	Type     string
	Name     string
	TblName  string
	RootPage uint32
	SQL      string

	// Rowid is the row's sqlite_schema rowid, 0 where the producer has none.
	// C gives a new object max(rowid)+1 (OP_NewRowid on the schema b-tree), so
	// a DROP leaves a gap and a VACUUM renumbers; on our format each object
	// keeps its own (DB.nextSchemaSeq) and the file records it.
	Rowid int64

	// Temp marks this object as belonging to the TEMP schema rather than main
	// -- derived, not stored as its own column: a "CREATE TEMP ..." stored SQL
	// text, or an index/trigger against a table that has one. See
	// temp_schema.go's markTempSchemaRows, which fills this in for the whole
	// catalog at once (an index's tbl_name can precede its table's own row).
	Temp bool

	// AliasOf names the table whose storage this one reads -- a rootpage a
	// direct sqlite_schema write pointed at another table's (tableMeta.aliasOf).
	// Its rendered rootpage is that table's (catalogRootpages).
	AliasOf string
}

// Schema reads every row of sqlite_schema. The result is memoized on the pager
// (schemaRows/schemaLoaded): a ReadOnlyPager is an immutable read snapshot, so
// the scan is done at most once per pager even though a single query resolves
// its schema several times over (resolveTable, checkIndexExists, view/fts
// lookups) and a warm pager is reused across many autocommit statements.
// Callers must treat the returned slice as read-only (they already do).
func (p *ReadOnlyPager) Schema() ([]SchemaRow, error) {
	if p != nil && p.schemaLoaded {
		return p.schemaRows, nil
	}
	seq, errFn := p.ScanTable(schemaRootPage)
	var rows []SchemaRow
	for rowid, vals := range seq {
		if len(vals) != 5 {
			return nil, fmt.Errorf("engine: sqlite_schema row has %d columns, want 5", len(vals))
		}
		rows = append(rows, SchemaRow{
			Rowid:    int64(rowid),
			Type:     valueText(vals[0]),
			Name:     valueText(vals[1]),
			TblName:  valueText(vals[2]),
			RootPage: uint32(valueInt(vals[3])),
			SQL:      valueText(vals[4]),
		})
	}
	if err := errFn(); err != nil {
		return nil, err
	}
	markTempSchemaRows(rows)
	if p != nil && equalFoldName(p.localSchema, "temp") {
		// Every row of the TEMP database's own catalog IS temp, and its text
		// says nothing about it -- C SQLite stores "CREATE TABLE tt(z)" in
		// sqlite_temp_master, not "CREATE TEMP TABLE" (verified against
		// 3.53.3). WHERE the row lives is what makes it temp (temp_store.go),
		// so markTempSchemaRows' text-and-creation-order inference has nothing
		// to say here.
		for i := range rows {
			rows[i].Temp = true
		}
	}
	if p != nil {
		p.schemaRows = rows
		p.schemaLoaded = true
	}
	return rows, nil
}

// TableRoot returns the root page number of the table named name, as
// recorded in sqlite_schema.
func (p *ReadOnlyPager) TableRoot(name string) (uint32, error) {
	rows, err := p.Schema()
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if r.Type == "table" && r.Name == name {
			return r.RootPage, nil
		}
	}
	return 0, fmt.Errorf("engine: no such table: %s", name)
}

// Rows reads every row of the named table, in ascending rowid order. It is a
// convenience wrapper over TableRoot + ScanTable for tests and callers that
// don't need streaming access.
func (p *ReadOnlyPager) Rows(tableName string) (rowids []uint64, rows [][]Value, err error) {
	root, err := p.TableRoot(tableName)
	if err != nil {
		return nil, nil, err
	}
	return p.RowsOfRoot(root)
}

// RowsOfRoot is Rows addressed by ROOT PAGE rather than by name -- what a
// caller holding a schema row already knows, and the only unambiguous form
// once two catalogs can hold a table of the same name (temp_schema.go).
func (p *ReadOnlyPager) RowsOfRoot(root uint32) (rowids []uint64, rows [][]Value, err error) {
	seq, errFn := p.ScanTable(root)
	for rowid, vals := range seq {
		rowids = append(rowids, rowid)
		rows = append(rows, vals)
	}
	if err := errFn(); err != nil {
		return nil, nil, err
	}
	return rowids, rows, nil
}

// valueText returns v's bytes as a string for Text/Blob values, or "" for
// anything else (in particular NULL, which sqlite_schema's sql column is for
// automatic entries such as sqlite_autoindex_*).
func valueText(v Value) string {
	if v.Typ == Text || v.Typ == Blob {
		return string(v.S)
	}
	return ""
}

// valueInt returns v's integer value, or 0 if v isn't an Int (sqlite_schema's
// rootpage column is 0 or NULL for views and triggers).
func valueInt(v Value) int64 {
	if v.Typ == Int {
		return v.I
	}
	return 0
}
