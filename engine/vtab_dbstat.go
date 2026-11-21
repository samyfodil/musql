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
	// The schema constraint is tested again on each row, which C never does
	// (statBestIndex omits it); the aggregate one is not, which C always does.
	dbstatSchemaRetested = 4
	dbstatAggregateTaken = 8
)

func (dbstatModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) > 1 {
		return nil, nil, fmt.Errorf("engine: dbstat takes at most one argument")
	}
	t := dbstatTable{schema: "main"}
	if len(args) == 1 {
		t.schema = strings.Trim(args[0], "\"'`[] ")
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
	}, t, nil
}

// dbstatTable's schema is the database its argument named (statConnect's
// iDb), main without one.
type dbstatTable struct{ schema string }

// BestIndex is statBestIndex: an equality on schema, then on aggregate, as
// Filter's arguments, the last of each as C keeps ("iSchema = i").
func (dbstatTable) BestIndex(info *VtabIndexInfo) error {
	argv := 0
	for _, in := range []struct{ col, bit int }{{dbstatColSchema, dbstatHasSchema}, {dbstatColAggregate, dbstatHasAggregate}} {
		last := -1
		for i, c := range info.Constraints {
			if c.Usable && c.Op == VtabEQ && c.Column == in.col {
				last = i
			}
		}
		if last < 0 {
			continue
		}
		argv++
		info.Usage[last].ArgvIndex = argv
		info.IdxNum |= in.bit
		c := info.Constraints[last]
		switch {
		case in.col == dbstatColSchema && !c.Omitted:
			info.IdxNum |= dbstatSchemaRetested
		case in.col == dbstatColAggregate && c.Omitted:
			info.IdxNum |= dbstatAggregateTaken
		}
	}
	return nil
}

// dbstatCols is the declared schema's column names, for OmittedConjunct.
var dbstatCols = []columnInfo{
	{Name: "name"}, {Name: "path"}, {Name: "pageno"}, {Name: "pagetype"}, {Name: "ncell"},
	{Name: "payload"}, {Name: "unused"}, {Name: "mx_payload"}, {Name: "pgoffset"},
	{Name: "pgsize"}, {Name: "schema"}, {Name: "aggregate"},
}

// OmittedConjunct implements omittingVtabModule: statBestIndex omits the
// schema equality it takes, so "WHERE schema = 'MAIN'" keeps its rows although
// the column reads "main". BestIndex takes the last schema equality, so this
// names it only when nothing after it could be offered as one and its
// right-hand side is a literal, which is always usable.
func (dbstatModule) OmittedConjunct(conj []Expr, scopeName string) int {
	last := -1
	var lastRHS Expr
	for i, e := range conj {
		if f, ok := e.(FuncExpr); ok && f.Name == vtabInLoopFn {
			last, lastRHS = i, nil
			continue
		}
		if ci, op, rhs, ok := matchVtabConstraint(e, dbstatCols, scopeName); ok && ci == dbstatColSchema && op == VtabEQ {
			last, lastRHS = i, rhs
		}
	}
	if last < 0 || lastRHS == nil || !vtabOmitLiteralRHS(lastRHS) {
		return -1
	}
	return last
}

// Open exists to satisfy VirtualTable: materializeVtab serves dbstat through
// dbstatRows, since its rows come from the database.
func (dbstatTable) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("engine: dbstat rows come from the pager, not a cursor")
}

// dbstatRows is dbstat's rows over p: sqlite_schema first, then each table
// and index in schema order. Only main is served; another database a query
// names declines, and one that does not exist answers as statFilter does: no
// rows for a schema= constraint, "no such database" for the argument
// (statConnect, which C reports at CREATE time).
func (t dbstatTable) dbstatRows(p *ReadOnlyPager, idxNum int, argv []Value) ([][]Value, []int64, error) {
	schema := t.schema
	if !strings.EqualFold(schema, "main") {
		if !dbstatKnownSchema(p, schema) {
			return nil, nil, semanticf("no such database: %s", schema)
		}
		return nil, nil, fmt.Errorf("%w: dbstat of a schema other than main", errVDBEUnsupported)
	}
	isAgg := false
	k := 0
	if idxNum&dbstatHasSchema != 0 {
		schema = valueToText(argv[k])
		k++
		if !strings.EqualFold(schema, "main") {
			if !dbstatKnownSchema(p, schema) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("%w: dbstat of a schema other than main", errVDBEUnsupported)
		}
		// The column reads "main"; a row tested against another spelling of
		// it would be dropped where C keeps it.
		if idxNum&dbstatSchemaRetested != 0 && schema != "main" {
			return nil, nil, fmt.Errorf("%w: dbstat schema constraint spelled %q", errVDBEUnsupported, schema)
		}
	}
	if idxNum&dbstatHasAggregate != 0 {
		v := argv[k]
		isAgg = valueToFloatLoose(v) != 0
		// C tests every row against aggregate=v too, the column's NUMERIC
		// affinity applied to v: dbstat('main', 'yes') has no rows.
		if idxNum&dbstatAggregateTaken != 0 {
			col := Value{Typ: Int}
			if isAgg {
				col.I = 1
			}
			if v.Typ == Null || compareValues(col, applyAffinityToValue(v, affNumeric)) != 0 {
				return nil, nil, nil
			}
		}
	}
	objs, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	main, agg := Value{Typ: Text, S: []byte("main")}, Value{Typ: Int}
	if isAgg {
		agg.I = 1
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
		// Each object is one leaf page here: its path, page number and
		// offset, or with aggregate=1 its page count and NULLs (statColumn).
		path, pageno, pagetype, pgoffset := Value{Typ: Text, S: []byte("/")}, Value{Typ: Int, I: int64(len(rows) + 1)},
			Value{Typ: Text, S: []byte("leaf")}, Value{Typ: Int}
		if isAgg {
			path, pageno, pagetype, pgoffset = Value{Typ: Null}, Value{Typ: Int, I: 1}, Value{Typ: Null}, Value{Typ: Null}
		}
		rows = append(rows, []Value{
			{Typ: Text, S: []byte(name)}, path, pageno, pagetype, {Typ: Int, I: ncell}, {Typ: Int, I: payload},
			{Typ: Int, I: 0}, {Typ: Int, I: mx}, pgoffset, {Typ: Int, I: payload}, main, agg,
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
	// Rows stream from the table's scan, one at a time, so a large database
	// is sized without holding it.
	scan := func(root uint32, project func(rowid uint64, row []Value) []Value) func(func([]Value)) error {
		return func(each func([]Value)) error {
			seq, errFn := p.ScanTable(root)
			for rowid, row := range seq {
				each(project(rowid, row))
			}
			return errFn()
		}
	}
	roots := map[string]uint32{}
	for _, o := range objs {
		if o.Type == "table" {
			roots[strings.ToLower(o.Name)] = o.RootPage
		}
	}
	for _, o := range objs {
		switch o.Type {
		case "table":
			if strings.HasPrefix(strings.ToUpper(o.SQL), "CREATE VIRTUAL") {
				continue // its rows are in its shadow tables, listed on their own
			}
			err = add(o.Name, scan(o.RootPage, func(_ uint64, row []Value) []Value { return row }))
		case "index":
			var cids []int
			cids, err = dbstatIndexColumns(p, o.Name)
			if err == nil {
				key := make([]Value, len(cids))
				err = add(o.Name, scan(roots[strings.ToLower(o.TblName)], func(rowid uint64, row []Value) []Value {
					for i, c := range cids {
						switch {
						case c == -1:
							key[i] = Value{Typ: Int, I: int64(rowid)}
						case c < 0 || c >= len(row):
							key[i] = Value{Typ: Null}
						default:
							key[i] = row[c]
						}
					}
					return key
				}))
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

// dbstatKnownSchema reports whether name is a database this connection has:
// main, temp or an attachment.
func dbstatKnownSchema(p *ReadOnlyPager, name string) bool {
	if strings.EqualFold(name, "main") || strings.EqualFold(name, "temp") {
		return true
	}
	for _, ar := range p.attachedReaders {
		if strings.EqualFold(ar.name, name) {
			return true
		}
	}
	return false
}

// dbstatIndexColumns is the columns of an index's records: its key columns
// and then the table's key (index_xinfo), as table column numbers, -1 for the
// rowid and -2 for an expression (counted as NULL).
func dbstatIndexColumns(p *ReadOnlyPager, index string) ([]int, error) {
	_, rs, err := p.QueryArgs("SELECT cid FROM pragma_index_xinfo(?, 'main')", []Value{{Typ: Text, S: []byte(index)}})
	if err != nil {
		return nil, err
	}
	cids := make([]int, 0, len(rs))
	for _, r := range rs {
		cids = append(cids, int(r[0].I))
	}
	if len(cids) == 0 {
		return []int{-2}, nil
	}
	return cids, nil
}
