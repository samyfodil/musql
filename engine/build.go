package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// Building a database from rows supplied by a converter.
// The engine derives table schema from CREATE TABLE text,
// so converters only need to provide catalog entries and row data.
//
// The engine knows nothing about where the rows came from. A converter that
// wants the engine to judge them afterwards opens the result like any other
// database (IntegrityCheck, OpenReader).

// Meta is a database's values that are not rows or schema text: the ones the
// PRAGMAs of the same names read.
type Meta struct {
	SchemaVersion uint32
	UserVersion   uint32
	ApplicationID uint32
	// Encoding is the text encoding every Text value is in; 0 when the database
	// has declared none (an empty one).
	Encoding TextEncoding
	// PageSize is 0 for the default.
	PageSize uint32
	// AutoVacuum is 0 (none), 1 (full) or 2 (incremental).
	AutoVacuum int
	// WAL is "journal_mode=wal".
	WAL bool
	// ChangeCounter and PageCount seed the file's change counter and page
	// count (what a delta names its base by, and what PRAGMA data_version and
	// page_count start from). 0 means 1.
	ChangeCounter, PageCount uint32
}

// TableShape is what a table's CREATE TABLE text says about its rows.
type TableShape struct {
	// Columns is every declared column, in declared order.
	Columns []string
	// Virtual marks each VIRTUAL generated column: it holds no value in a
	// stored row, which skips it.
	Virtual []bool
	// IPK is the INTEGER PRIMARY KEY column, or -1. Its value is the rowid,
	// and a stored row holds NULL in its place.
	IPK int
	// WithoutRowid is a WITHOUT ROWID table; PrimaryKey is its key, in key
	// order.
	WithoutRowid bool
	PrimaryKey   []KeyColumn
}

// KeyColumn is one column of an ordering key.
type KeyColumn struct {
	Column    int // declared column index; -1 for an expression
	Collation string
	Desc      bool
}

// TableShapeOf parses a CREATE TABLE statement into its shape.
func TableShapeOf(createSQL string) (TableShape, error) {
	withoutRowid, _, err := parseTableTailClauses(createSQL)
	if err != nil {
		return TableShape{}, err
	}
	cols, _, specs, err := parseCreateTableColumnsAndAutoIndexes(createSQL, withoutRowid)
	if err != nil {
		return TableShape{}, err
	}
	sh := TableShape{IPK: -1, WithoutRowid: withoutRowid}
	for i, c := range cols {
		sh.Columns = append(sh.Columns, c.Name)
		sh.Virtual = append(sh.Virtual, c.IsGenerated() && !c.GeneratedStored)
		if c.IsRowidAlias && sh.IPK < 0 {
			sh.IPK = i
		}
	}
	if withoutRowid {
		_, bySpec, err := buildAutoIndexes("", cols, specs)
		if err != nil {
			return TableShape{}, err
		}
		pk, err := finalizeWithoutRowidPK("", true, cols, specs, bySpec)
		if err != nil {
			return TableShape{}, err
		}
		for i, ci := range pk.colIdx {
			sh.PrimaryKey = append(sh.PrimaryKey, KeyColumn{Column: ci, Collation: pk.colCollation[i], Desc: pk.colDesc[i]})
		}
	}
	return sh, nil
}

// Builder writes a new database file, a table at a time. Nothing exists at
// path until Finish.
type Builder struct {
	path   string
	w      *segFileWriter
	tables []ConvertedTable
	cat    ConvertedCatalog
	rank   uint32
	meta   Meta
	open   *BuildTable
}

// NewBuilder starts a database that Finish writes to path.
func NewBuilder(path string) (*Builder, error) {
	w, err := newSegFileWriter(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return &Builder{path: path, w: w}, nil
}

// SetMeta records the database's Meta.
func (b *Builder) SetMeta(m Meta) { b.meta = m }

func (b *Builder) nextRank() uint32 { b.rank++; return b.rank }

// Table starts a table with storage, whose rows Add then supplies in rowid
// order. Catalog entries -- Table and Object both -- are listed in the order
// they are added; catalogRowid is the entry's rowid in sqlite_master.
func (b *Builder) Table(name, createSQL string, catalogRowid int64) (*BuildTable, error) {
	if err := b.closeTable(); err != nil {
		return nil, err
	}
	sh, err := TableShapeOf(createSQL)
	if err != nil {
		return nil, fmt.Errorf("engine: table %s: %w", name, err)
	}
	ti := len(b.tables)
	b.tables = append(b.tables, ConvertedTable{Name: name, SQL: createSQL, Cols: sh.Columns, IPK: sh.IPK, Rank: b.nextRank(), Rowid: catalogRowid})
	cols := make([]columnInfo, len(sh.Columns))
	for i, n := range sh.Columns {
		cols[i].Name = n
	}
	t := &BuildTable{shape: sh, cut: &segCutter{cols: cols, what: name, emit: func(raw []byte) error { return b.w.addSegment(ti, raw) }}}
	b.open = t
	return t, nil
}

// Object adds a catalog entry without storage of its own: an index (whose
// entries the engine builds from its table's rows), a view, a trigger, a
// virtual table.
func (b *Builder) Object(typ, name, tblName, sql string, catalogRowid int64) {
	b.cat.Objects = append(b.cat.Objects, ConvertedObject{Type: typ, Name: name, TblName: tblName, SQL: sql, Rank: b.nextRank(), Rowid: catalogRowid})
}

// Sequence records an AUTOINCREMENT counter.
func (b *Builder) Sequence(table string, seq int64) {
	b.cat.Sequences = append(b.cat.Sequences, ConvertedSequence{Table: table, Seq: seq})
}

func (b *Builder) closeTable() error {
	if b.open == nil {
		return nil
	}
	t := b.open
	b.open = nil
	return t.cut.flush()
}

// Finish writes the file.
func (b *Builder) Finish() error {
	if err := b.closeTable(); err != nil {
		return err
	}
	m := b.meta
	b.cat.SchemaVersion, b.cat.UserVersion, b.cat.ApplicationID = m.SchemaVersion, m.UserVersion, m.ApplicationID
	b.cat.Encoding, b.cat.PageSize = uint32(m.Encoding), m.PageSize
	b.cat.AutoVacuumPlus1 = uint32(m.AutoVacuum) + 1
	if m.WAL {
		b.cat.JournalWAL = 1
	}
	ctr, pages := max(m.ChangeCounter, 1), max(m.PageCount, 1)
	return b.w.finish(b.path, b.tables, b.cat, ctr, pages)
}

// Discard drops whatever Finish has not written; it is safe after Finish.
func (b *Builder) Discard() { b.w.discard() }

// BuildTable is one table of a Builder.
type BuildTable struct {
	shape TableShape
	cut   *segCutter
	n     uint64
}

// Shape is the table's shape, as its CREATE TABLE text gives it.
func (t *BuildTable) Shape() TableShape { return t.shape }

// Add appends one STORED row: one value per declared column in declared order,
// a VIRTUAL generated column skipped, NULL in the INTEGER PRIMARY KEY's place.
// A row written before an ALTER TABLE ADD COLUMN may be short; its missing
// columns read as their defaults. Rows come in ascending rowid order; a
// WITHOUT ROWID table's rowid is ignored, its identity being its key.
func (t *BuildTable) Add(rowid int64, row []Value) error {
	if t.shape.WithoutRowid {
		t.n++
		rowid = int64(t.n)
	}
	return t.cut.add(uint64(rowid), row)
}

// IntegrityCheckOptions tunes IntegrityCheck.
type IntegrityCheckOptions struct {
	// MaxErrors is PRAGMA integrity_check(N)'s N; 0 means C's default, 100.
	MaxErrors int
	// StoredIndexEntries, when set, supplies an index's entries as another copy
	// of this database stores them, in index order -- ok false for an index it
	// holds none of. The check then holds them against the rows, as C's holds
	// its index b-trees ("row %d missing from index %s", "wrong # of entries in
	// index %s"). Without it an index is derived from the rows and always agrees.
	StoredIndexEntries func(index string) (entries [][]Value, ok bool)
}

// IntegrityCheck runs PRAGMA integrity_check over the database at path and
// returns its findings, nil for "ok".
func IntegrityCheck(path string, opts IntegrityCheckOptions) ([]string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	rp, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer rp.Close()
	rp.storedIndexEntries = opts.StoredIndexEntries
	q := `PRAGMA integrity_check`
	if opts.MaxErrors > 0 {
		q = fmt.Sprintf(`PRAGMA integrity_check(%d)`, opts.MaxErrors)
	}
	_, rows, err := rp.Query(q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 1 && len(rows[0]) == 1 && string(rows[0][0].S) == "ok" {
		return nil, nil
	}
	var found []string
	for _, r := range rows {
		if len(r) > 0 {
			found = append(found, string(r[0].S))
		}
	}
	return found, nil
}
