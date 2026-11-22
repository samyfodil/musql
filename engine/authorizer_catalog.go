package engine

import "strings"

// authTable is a table, view or virtual table as the authorizer sees it.
type authTable struct {
	name   string // as declared
	db     string // "main", "temp" or an ATTACH name
	cols   []string
	hidden []bool
	ipk    int
	rowid  bool        // a rowid a query can name
	view   *SelectStmt // a view's SELECT
	vtab   string      // a virtual table's module
	noRow  bool        // WITHOUT ROWID
}

func (t *authTable) colIndex(name string) int {
	for i, c := range t.cols {
		if equalFoldName(c, name) {
			return i
		}
	}
	return -1
}

// rowidName is the name sqlite3AuthRead gives a rowid read: the INTEGER
// PRIMARY KEY column's, else "ROWID".
func (t *authTable) rowidName() string {
	if t.ipk >= 0 {
		return t.cols[t.ipk]
	}
	return "ROWID"
}

// authCatalog is one attached database's objects, read from its schema.
type authCatalog struct {
	name string
	rows []SchemaRow
}

var schemaTableCols = []string{"type", "name", "tbl_name", "rootpage", "sql"}

// lookupTable finds a table, view or virtual table as sqlite3LocateTable
// does: an unqualified name in temp, then main, then each attached database.
func (a *authPass) lookupTable(schema, name string) *authTable {
	switch {
	case schema == "" && isMainSchemaCatalogName(name):
		return a.localTable(false, name)
	case schema == "" && isTempSchemaCatalogName(name):
		return a.localTable(true, name)
	case schema == "":
		if t := a.localTable(true, name); t != nil {
			return t
		}
		if t := a.localTable(false, name); t != nil {
			return t
		}
		for _, x := range a.extra {
			if t := a.catalogTable(x.DB, false, x.Name, name); t != nil {
				return t
			}
		}
		for _, at := range a.db.attached {
			if t := a.attachedTable(at, name); t != nil {
				return t
			}
		}
		return nil
	case equalFoldName(schema, "main"):
		return a.localTable(false, name)
	case equalFoldName(schema, "temp"):
		return a.localTable(true, name)
	}
	for _, x := range a.extra {
		if equalFoldName(x.Name, schema) {
			return a.catalogTable(x.DB, false, x.Name, name)
		}
	}
	for _, at := range a.db.attached {
		if equalFoldName(at.name, schema) {
			return a.attachedTable(at, name)
		}
	}
	return nil
}

func (a *authPass) localTable(temp bool, name string) *authTable {
	dbName := "main"
	if temp {
		dbName = "temp"
	}
	return a.catalogTable(a.db, temp, dbName, name)
}

// catalogTable finds name among db's objects of one schema.
func (a *authPass) catalogTable(db *DB, temp bool, dbName, name string) *authTable {
	if !temp && isMainSchemaCatalogName(name) {
		return &authTable{name: "sqlite_master", db: dbName, cols: schemaTableCols, ipk: -1, rowid: true}
	}
	if temp && (isTempSchemaCatalogName(name) || isMainSchemaCatalogName(name)) {
		return &authTable{name: "sqlite_temp_master", db: dbName, cols: schemaTableCols, ipk: -1, rowid: true}
	}
	if !temp && isTempSchemaCatalogName(name) {
		return nil
	}
	for _, t := range db.tables {
		if t.isTemp == temp && equalFoldName(t.name, name) {
			at := &authTable{name: t.name, db: dbName, ipk: t.ipkIndex, rowid: !t.withoutRowid, noRow: t.withoutRowid}
			for _, c := range t.cols {
				at.cols = append(at.cols, c.Name)
				at.hidden = append(at.hidden, c.Hidden)
			}
			return at
		}
	}
	for _, v := range db.views {
		if v.isTemp == temp && equalFoldName(v.name, name) {
			at := &authTable{name: v.name, db: dbName, ipk: -1, view: v.selectStmt}
			at.cols = v.colNames
			if at.cols == nil {
				at.cols = a.selectColNames(v.selectStmt, nil)
			}
			return at
		}
	}
	for _, vt := range db.vtabs {
		if vt.isTemp == temp && equalFoldName(vt.name, name) {
			at := &authTable{name: vt.name, db: dbName, ipk: -1, rowid: true, vtab: vt.module}
			for _, c := range vt.colInfos {
				at.cols = append(at.cols, c.Name)
				at.hidden = append(at.hidden, c.Hidden)
			}
			return at
		}
	}
	return nil
}

func (a *authPass) attachedTable(at *attachedDB, name string) *authTable {
	if isMainSchemaCatalogName(name) {
		return &authTable{name: "sqlite_master", db: at.name, cols: schemaTableCols, ipk: -1, rowid: true}
	}
	cat := a.attach[strings.ToLower(at.name)]
	if cat == nil {
		cat = &authCatalog{name: at.name}
		if at.pager != nil {
			if rows, err := at.pager.Schema(); err == nil {
				cat.rows = rows
			}
		}
		if a.attach == nil {
			a.attach = map[string]*authCatalog{}
		}
		a.attach[strings.ToLower(at.name)] = cat
	}
	for _, r := range cat.rows {
		if !equalFoldName(r.Name, name) {
			continue
		}
		switch r.Type {
		case "table":
			if isCreateVirtualTableSQL(r.SQL) {
				return &authTable{name: r.Name, db: at.name, ipk: -1, rowid: true, vtab: "?"}
			}
			noRow := sqlTextTableIsWithoutRowid(r.SQL)
			cols, _, _, err := parseCreateTableColumnsAndAutoIndexes(r.SQL, noRow)
			if err != nil {
				return nil
			}
			t := &authTable{name: r.Name, db: at.name, ipk: -1, rowid: !noRow, noRow: noRow}
			for i, c := range cols {
				t.cols = append(t.cols, c.Name)
				t.hidden = append(t.hidden, c.Hidden)
				if c.IsRowidAlias && t.ipk < 0 {
					t.ipk = i
				}
			}
			return t
		case "view":
			v, err := parseCreateViewStmt(r.SQL)
			if err != nil {
				return nil
			}
			t := &authTable{name: r.Name, db: at.name, ipk: -1, view: v.selectStmt, cols: v.colNames}
			if t.cols == nil {
				t.cols = a.selectColNames(v.selectStmt, nil)
			}
			return t
		}
	}
	return nil
}

// tableFuncTable is an eponymous table-valued function, which C reads as a
// table of the main database.
func tableFuncTable(name string) *authTable {
	t := &authTable{name: strings.ToLower(name), db: "main", ipk: -1, rowid: true}
	switch t.name {
	case "json_each", "json_tree", "jsonb_each", "jsonb_tree":
		t.cols = []string{"key", "value", "type", "atom", "id", "parent", "fullkey", "path", "json", "root"}
		t.hidden = []bool{false, false, false, false, false, false, false, false, true, true}
	case "generate_series":
		t.cols = []string{"value", "start", "stop", "step"}
		t.hidden = []bool{false, true, true, true}
	default:
		return nil
	}
	return t
}
