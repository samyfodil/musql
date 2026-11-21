// The dbstat virtual table (ext/misc/dbstat.c in C SQLite's tree, enabled by
// SQLITE_ENABLE_DBSTAT_VTAB): how much space each table and index takes.
//
// C walks every b-tree page and reports one row per page, or with
// aggregate=1 one per b-tree. This engine has no pages: a table is columnar
// segments plus a delta log, and an index is not stored at all. So dbstat
// answers the question it is asked for, how big each object is, the way
// PRAGMA page_count does (segFilePages): one row per table and index, with
// ncell its rows and payload the bytes those rows take in SQLite's record
// format (recordSize) -- what the object holds, not what this file spends on
// it. pgsize is that payload, so "sum(pgsize) GROUP BY name" ranks objects as
// in C, and the numbers are not C's for the same data. An expression column
// of an index counts as NULL and a partial index counts every row.
//
// It is opt-in, as fts5 is (RegisterDBStat).
package engine

import (
	"fmt"
	"strings"
)

// RegisterDBStat registers the dbstat virtual table. Like RegisterFunction it
// must run before any statement does.
func RegisterDBStat() { RegisterVtabModule("dbstat", dbstatModule{}) }

type dbstatModule struct{}

// The columns of dbstat.c's schema, and the bits of BestIndex's plan.
const (
	dbstatColSchema    = 10
	dbstatColAggregate = 11
	dbstatHasSchema    = 1
	dbstatHasAggregate = 2
)

func (dbstatModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) > 1 {
		return nil, nil, fmt.Errorf("engine: dbstat takes at most one argument")
	}
	return []VtabColumn{
		{Name: "name", Type: "TEXT"},
		{Name: "path", Type: "TEXT"},
		{Name: "pageno", Type: "INTEGER"},
		{Name: "pagetype", Type: "TEXT"},
		{Name: "ncell", Type: "INTEGER"},
		{Name: "payload", Type: "INTEGER"},
		{Name: "unused", Type: "INTEGER"},
		{Name: "mx_payload", Type: "INTEGER"},
		{Name: "pgoffset", Type: "INTEGER"},
		{Name: "pgsize", Type: "INTEGER"},
		{Name: "schema", Type: "TEXT", Hidden: true},
		{Name: "aggregate", Type: "BOOLEAN", Hidden: true},
	}, dbstatTable{}, nil
}

type dbstatTable struct{}

// BestIndex is statBestIndex: an equality on schema, then on aggregate, as
// Filter's arguments.
func (dbstatTable) BestIndex(info *VtabIndexInfo) error {
	argv := 0
	for _, in := range []struct{ col, bit int }{{dbstatColSchema, dbstatHasSchema}, {dbstatColAggregate, dbstatHasAggregate}} {
		for i, c := range info.Constraints {
			if c.Usable && c.Op == VtabEQ && c.Column == in.col && info.Usage[i].ArgvIndex == 0 {
				argv++
				info.Usage[i].ArgvIndex = argv
				info.IdxNum |= in.bit
				break
			}
		}
	}
	return nil
}

// Open exists to satisfy VirtualTable: materializeVtab serves dbstat through
// dbstatRows, since its rows come from the database.
func (dbstatTable) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("engine: dbstat rows come from the pager, not a cursor")
}

// dbstatRows is dbstat's rows over p: sqlite_schema first, then each table
// and index in schema order.
func (dbstatTable) dbstatRows(p *ReadOnlyPager, idxNum int, argv []Value) ([][]Value, []int64, error) {
	schema, agg := Value{Typ: Text, S: []byte("main")}, Value{Typ: Int, I: 0}
	k := 0
	if idxNum&dbstatHasSchema != 0 {
		schema = argv[k]
		k++
		if !strings.EqualFold(valueToText(schema), "main") {
			return nil, nil, fmt.Errorf("%w: dbstat of a schema other than main", errVDBEUnsupported)
		}
	}
	if idxNum&dbstatHasAggregate != 0 {
		agg = argv[k]
	}
	objs, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	var rows [][]Value
	add := func(name string, sizes func(func([]Value)) error) error {
		var ncell, payload, mx int64
		if err := sizes(func(rec []Value) {
			n := int64(recordSize(rec))
			ncell++
			payload += n
			mx = max(mx, n)
		}); err != nil {
			return err
		}
		rows = append(rows, []Value{
			{Typ: Text, S: []byte(name)}, {Typ: Text, S: []byte("/")}, {Typ: Int, I: int64(len(rows) + 1)},
			{Typ: Text, S: []byte("leaf")}, {Typ: Int, I: ncell}, {Typ: Int, I: payload}, {Typ: Int, I: 0},
			{Typ: Int, I: mx}, {Typ: Int, I: 0}, {Typ: Int, I: payload}, schema, agg,
		})
		return nil
	}
	if err := add("sqlite_schema", func(each func([]Value)) error {
		for _, o := range objs {
			each([]Value{
				{Typ: Text, S: []byte(o.Type)}, {Typ: Text, S: []byte(o.Name)}, {Typ: Text, S: []byte(o.TblName)},
				{Typ: Int, I: int64(o.RootPage)}, {Typ: Text, S: []byte(o.SQL)},
			})
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	query := func(q string) func(func([]Value)) error {
		return func(each func([]Value)) error {
			_, rs, err := p.QueryArgs(q, nil)
			if err != nil {
				return err
			}
			for _, r := range rs {
				each(r)
			}
			return nil
		}
	}
	for _, o := range objs {
		switch o.Type {
		case "table":
			if strings.HasPrefix(strings.ToUpper(o.SQL), "CREATE VIRTUAL") {
				continue // its rows are in its shadow tables, listed on their own
			}
			err = add(o.Name, query("SELECT * FROM main."+quoteIdent(o.Name)))
		case "index":
			var cols []string
			cols, err = dbstatIndexColumns(p, o.Name)
			if err == nil {
				err = add(o.Name, query("SELECT "+strings.Join(cols, ", ")+" FROM main."+quoteIdent(o.TblName)))
			}
		}
		if err != nil {
			return nil, nil, err
		}
	}
	rowids := make([]int64, len(rows))
	for i := range rowids {
		rowids[i] = int64(i + 1)
	}
	return rows, rowids, nil
}

// dbstatIndexColumns is the select list of an index's records: its key
// columns and then the table's key (index_xinfo), an expression as NULL.
func dbstatIndexColumns(p *ReadOnlyPager, index string) ([]string, error) {
	_, rs, err := p.QueryArgs("SELECT cid, name FROM pragma_index_xinfo(?, 'main')", []Value{{Typ: Text, S: []byte(index)}})
	if err != nil {
		return nil, err
	}
	cols := make([]string, 0, len(rs))
	for _, r := range rs {
		switch {
		case r[0].I == -1:
			cols = append(cols, "rowid")
		case r[0].I < 0 || r[1].Typ == Null:
			cols = append(cols, "NULL")
		default:
			cols = append(cols, quoteIdent(string(r[1].S)))
		}
	}
	if len(cols) == 0 {
		return []string{"NULL"}, nil
	}
	return cols, nil
}
