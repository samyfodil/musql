// PRAGMA [schema.]table_list[(name)]: one row per table, view, virtual table,
// and shadow table in scope with columns schema, name, type, ncol, wr, strict.
//
// Rules:
// - Max one argument (the table name); NULL or absent lists all databases.
// - sqlite_schema/sqlite_temp_schema always appear unconditionally.
// - Row order follows SQLite's internal schema hash (not reproducible).
// // ncol counts all declared columns including generated ones. A VIEW's ncol
// is its result-column count. VIRTUAL TABLE ncol counts module columns.
// Rtree tables decline (they use private stores with no persisted shadow rows).
// SHADOW tables are typed "shadow" with their column count. TRIGGER tables
// are not listed.
//   - wr is 1 for a WITHOUT ROWID table, else 0; strict is 1 for a STRICT
//     table, else 0; both are always 0 for a view, virtual table or shadow.
//   - an explicit qualifier naming an ATTACHed database (queryPragmaStmt's
//     attachedReaderNamed fallback) restricts the listing to exactly that
//     database -- no temp row, no other attachment -- which falls out for
//     free from routing the WHOLE pragma to that database's own pager: its
//     own "unqualified" scope IS exactly its own main catalog, and
//     tableListCatalogRows/pragmaTableListOne only ever add a temp row or
//     recurse into attachments when p is the PRIMARY session's own pager
//     (localSchemaOr(p.localSchema) == "main").
//   - the single-argument form searches every attached database, main first,
//     exactly like table_info/index_list/foreign_key_list
//     (attach_write.go's pragmaObjectOwner).
package engine

import (
	"fmt"
)

// pragmaTableList implements PRAGMA table_list's dispatch: the whole-database
// listing when no name was given, cross-database object resolution for an
// unqualified single name (pragmaObjectOwner, attach_write.go -- the same rule
// table_info/index_list/foreign_key_list already apply), and the single-name
// lookup otherwise. See this file's package doc comment for every rule.
func (p *ReadOnlyPager) pragmaTableList(scope schemaScope, stmt *PragmaStmt) (cols []string, rows [][]Value, err error) {
	cols = []string{"schema", "name", "type", "ncol", "wr", "strict"}
	if !stmt.HasValue {
		return p.pragmaTableListAll(scope)
	}
	if stmt.Schema == "" {
		// scopeAny, not scopeMain: the owner may be the TEMP database
		// (temp_store.go), whose every row is a temp row -- and an ATTACHed
		// database has no temp row to let through, so the two cases need no
		// telling apart here.
		if owner := p.pragmaObjectOwner(stmt.ValueText); owner != nil {
			return owner.pragmaTableListOne(scopeAny, stmt.ValueText)
		}
	}
	return p.pragmaTableListOne(scope, stmt.ValueText)
}

// tableListSchemaTag is the "schema" column value for a resolved row: "temp"
// when it came from the TEMP catalog (a schema row's own Temp flag, NOT
// p.localSchema -- an unqualified lookup on the primary pager can resolve
// into either catalog), else this pager's own local schema name ("main" for
// the primary connection, or an attached database's own attach name once
// delegated there).
func (p *ReadOnlyPager) tableListSchemaTag(temp bool) string {
	if temp {
		return "temp"
	}
	return localSchemaOr(p.localSchema)
}

// pragmaTableListIsPrimary reports whether p is the PRIMARY connection's own
// pager (as opposed to an attached reader delegated to via
// queryPragmaStmt's attachedReaderNamed fallback, whose localSchema is set to
// its own attach name -- attach.go's SetLocalSchema(ad.name) at ATTACH time).
// Only the primary ever has a TEMP catalog or further attachments of its own
// to fold into a table_list listing.
func (p *ReadOnlyPager) pragmaTableListIsPrimary() bool {
	return equalFoldName(localSchemaOr(p.localSchema), "main")
}

// pragmaTableListAll is the no-argument/NULL form: every table_list row in
// scope. Unqualified (scopeAny) on the primary pager that means main, temp and
// every attached database; scopeMain/scopeTemp (an explicit "main."/"temp."
// qualifier) restrict to just that one; on a delegated attached pager scope is
// always scopeAny (see pragmaTableList) and pragmaTableListIsPrimary keeps it
// to just that one database's own catalog, matching "PRAGMA aux.table_list"
// (verified directly: no temp row, no OTHER attachment's rows).
func (p *ReadOnlyPager) pragmaTableListAll(scope schemaScope) (cols []string, rows [][]Value, err error) {
	cols = []string{"schema", "name", "type", "ncol", "wr", "strict"}
	rows = [][]Value{}
	if equalFoldName(localSchemaOr(p.localSchema), "temp") {
		// This IS the TEMP database (a "PRAGMA temp.table_list" routed here by
		// queryPragmaStmt): every row of its catalog is a temp row, and it has
		// no other database to fold in.
		r, herr := p.tableListCatalogRows(scopeTemp, "temp")
		if herr != nil {
			return nil, nil, herr
		}
		return cols, append(rows, r...), nil
	}
	isPrimary := p.pragmaTableListIsPrimary()
	if scope == scopeAny || scope == scopeMain {
		r, herr := p.tableListCatalogRows(scopeMain, localSchemaOr(p.localSchema))
		if herr != nil {
			return nil, nil, herr
		}
		rows = append(rows, r...)
	}
	if isPrimary && (scope == scopeAny || scope == scopeTemp) {
		// The TEMP database's rows come from ITS pager (temp_store.go): this
		// one's catalog is main's alone. With no temp database open there is
		// still a "temp" row for the catalog itself -- C SQLite lists
		// sqlite_temp_schema whether or not anything is in it.
		src, srcScope := p, scopeTemp
		if tr, found := p.attachedReaderNamed("temp"); found {
			src = tr
		}
		r, herr := src.tableListCatalogRows(srcScope, "temp")
		if herr != nil {
			return nil, nil, herr
		}
		rows = append(rows, r...)
	}
	if isPrimary && scope == scopeAny {
		for _, ar := range p.attachedReaders {
			if ar.pager == nil || ar.name == "temp" {
				continue // temp is listed above, in its own place
			}
			_, r, herr := ar.pager.pragmaTableListAll(scopeMain)
			if herr != nil {
				return nil, nil, herr
			}
			rows = append(rows, r...)
		}
	}
	return cols, rows, nil
}

// tableListCatalogRows builds every table_list row for ONE catalog (main or
// temp, per scope) of p's own schema, tagged with schemaName, plus that
// catalog's own always-present synthetic sqlite_schema/sqlite_temp_schema row.
func (p *ReadOnlyPager) tableListCatalogRows(scope schemaScope, schemaName string) ([][]Value, error) {
	schemaRows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	if mod, has := tableListPrivateStoreVtab(schemaRows, scope); has {
		return nil, fmt.Errorf("%w: PRAGMA table_list: the schema holds a %q virtual table, whose real shadow tables this engine does not create (query.go's vtabPrivateStoreModules) -- the full listing cannot represent it, so this is a decline rather than an undercount", errVDBEUnsupported, mod)
	}
	out := make([][]Value, 0, len(schemaRows)+1)
	for i := range schemaRows {
		r := &schemaRows[i]
		if !scope.accepts(r.Temp) {
			continue
		}
		switch r.Type {
		case "table":
			if isCreateVirtualTableSQL(r.SQL) {
				row, rerr := p.tableListVirtualTableRow(schemaName, r)
				if rerr != nil {
					return nil, rerr
				}
				out = append(out, row)
				continue
			}
			row, rerr := tableListOrdinaryTableRow(schemaName, r)
			if rerr != nil {
				return nil, rerr
			}
			if tableListIsShadowOf(schemaRows, scope, r.Name) {
				row[2] = Value{Typ: Text, S: []byte("shadow")}
			}
			out = append(out, row)
		case "view":
			row, rerr := p.tableListViewRow(schemaName, r)
			if rerr != nil {
				return nil, rerr
			}
			out = append(out, row)
		}
	}
	out = append(out, tableListSyntheticCatalogRow(schemaName, scope == scopeTemp))
	return out, nil
}

// tableListVirtualTableRow is a virtual table's own row. Its "ncol" counts the
// module's DECLARED columns, HIDDEN ones included -- which is the same list
// PRAGMA table_xinfo reports, so it comes from the same mapping
// (vtabDeclaredColumnsAsSQLiteReportsThem, pragma.go) rather than from a
// second derivation that could drift from it.
//
// Verified against 3.53.3, one module per shape:
//
//	rtree(id,x0,x1)              ncol 3   (no hidden columns at all)
//	fts4(aa,bb)                  ncol 5   (2 user + <table>, docid, __langid)
//	fts4(a,b,languageid=l)       ncol 5   (the langid column is named l)
//	fts4aux('vf')                ncol 5   (4 visible + a hidden languageid)
//	fts3tokenize('simple')       ncol 5   (none hidden)
//
// A module whose declared list has not been verified declines, exactly as
// table_xinfo does for it -- the same condition, so the two can never
// disagree about which modules are answerable.
func (p *ReadOnlyPager) tableListVirtualTableRow(schemaName string, r *SchemaRow) ([]Value, error) {
	_, module, args, _, perr := parseCreateVirtualTableStmt(r.SQL)
	if perr != nil {
		return nil, fmt.Errorf("engine: PRAGMA table_list: %s: %w", r.Name, perr)
	}
	mod, ok := lookupVtabModule(module)
	if !ok {
		return nil, fmt.Errorf("engine: no such module: %s", module)
	}
	vcols, _, cerr := vtabConnect(p.fts3Catalog(), mod, args)
	if cerr != nil {
		return nil, fmt.Errorf("engine: PRAGMA table_list: %s: %w", r.Name, cerr)
	}
	declared, known := vtabDeclaredColumnsAsSQLiteReportsThem(module, r.Name, vcols)
	if !known {
		return nil, fmt.Errorf("%w: PRAGMA table_list: virtual table %s's own ncol (which counts its module's hidden columns too) is not verified against the oracle for module %q", errVDBEUnsupported, r.Name, module)
	}
	return []Value{
		{Typ: Text, S: []byte(schemaName)},
		{Typ: Text, S: []byte(r.Name)},
		{Typ: Text, S: []byte("virtual")},
		{Typ: Int, I: int64(len(declared))},
		{Typ: Int, I: 0},
		{Typ: Int, I: 0},
	}, nil
}

// vtabShadowSuffixes is each module's xShadowName set, verbatim: the suffixes
// after "<vtab>_" that C SQLite recognises as that module's own shadow
// tables.
//
//	rtree/rtree_i32   rtree.c:3362-3364
//	fts3/fts4         fts3.c:4004-4006
//	fts5              fts5_main.c:3709-3711
//
// A module with no xShadowName at all (fts4aux, fts3tokenize, and every
// eponymous one) owns no shadow tables, which is why it is simply absent
// here -- sqlite3IsShadowTableOf returns 0 for it (build.c:2526).
var vtabShadowSuffixes = map[string][]string{
	"rtree":     {"node", "parent", "rowid"},
	"rtree_i32": {"node", "parent", "rowid"},
	"fts3":      {"content", "docsize", "segdir", "segments", "stat"},
	"fts4":      {"content", "docsize", "segdir", "segments", "stat"},
	"fts5":      {"config", "content", "data", "docsize", "idx"},
}

// tableListIsShadowOf reports whether name is a shadow table of some VIRTUAL
// table in the same schema, which is what PRAGMA table_list types "shadow"
// rather than "table" (pragma.c:1325).
//
// It is sqlite3IsShadowTableOf (build.c:2515-2528) as written: the name has to
// start with the virtual table's own name, the next character has to be '_',
// and the REMAINDER has to be a suffix that module's xShadowName accepts. The
// last clause is why a plain "CREATE TABLE r_wibble" beside an rtree "r" stays
// an ordinary table -- matching the prefix is not enough.
func tableListIsShadowOf(schemaRows []SchemaRow, scope schemaScope, name string) bool {
	for i := range schemaRows {
		v := &schemaRows[i]
		if v.Type != "table" || !scope.accepts(v.Temp) || !isCreateVirtualTableSQL(v.SQL) {
			continue
		}
		if len(name) <= len(v.Name)+1 || !equalFoldName(name[:len(v.Name)], v.Name) || name[len(v.Name)] != '_' {
			continue
		}
		_, module, _, _, perr := parseCreateVirtualTableStmt(v.SQL)
		if perr != nil {
			continue
		}
		for _, suffix := range vtabShadowSuffixes[r33sFoldIdent(module)] {
			if equalFoldName(name[len(v.Name)+1:], suffix) {
				return true
			}
		}
	}
	return false
}

// pragmaTableListOne is the single-name form: PRAGMA table_list(name), or the
// equivalent "WHERE arg='name'"/call spelling through the wrapper.
func (p *ReadOnlyPager) pragmaTableListOne(scope schemaScope, name string) (cols []string, rows [][]Value, err error) {
	cols = []string{"schema", "name", "type", "ncol", "wr", "strict"}
	rows = [][]Value{}
	if isMasterSchemaCatalogAlias(name) {
		temp := isTempMasterSchemaCatalogAlias(name)
		if temp && !p.pragmaTableListIsPrimary() {
			return cols, rows, nil // an attached database has no temp catalog of its own
		}
		wantScope := scopeMain
		if temp {
			wantScope = scopeTemp
		}
		if scope == wantScope || scope == scopeAny {
			rows = append(rows, tableListSyntheticCatalogRow(p.tableListSchemaTag(temp), temp))
		}
		return cols, rows, nil
	}
	schemaRows, serr := p.Schema()
	if serr != nil {
		return nil, nil, serr
	}
	tr := tableListFindRow(schemaRows, scope, name)
	if tr == nil {
		if mod, has := tableListPrivateStoreVtab(schemaRows, scope); has {
			return nil, nil, fmt.Errorf("%w: PRAGMA table_list(%s): the schema holds a %q virtual table whose real shadow tables this engine does not create, so a name this engine's own schema does not know might still be one of THOSE in C SQLite", errVDBEUnsupported, name, mod)
		}
		return cols, rows, nil
	}
	tag := p.tableListSchemaTag(tr.Temp)
	switch {
	case tr.Type == "view":
		row, rerr := p.tableListViewRow(tag, tr)
		if rerr != nil {
			return nil, nil, rerr
		}
		rows = append(rows, row)
	case isCreateVirtualTableSQL(tr.SQL):
		row, rerr := p.tableListVirtualRow(tag, tr)
		if rerr != nil {
			return nil, nil, rerr
		}
		rows = append(rows, row)
	case tableListNameMatchesAnyShadow(schemaRows, tr.Name):
		return nil, nil, fmt.Errorf("%w: PRAGMA table_list(%s): this name matches a virtual-table module's known shadow-table naming pattern, and classifying it (type='shadow' vs 'table', and content= mode's narrower shadow set) is not verified against the oracle", errVDBEUnsupported, name)
	default:
		row, rerr := tableListOrdinaryTableRow(tag, tr)
		if rerr != nil {
			return nil, nil, rerr
		}
		rows = append(rows, row)
	}
	return cols, rows, nil
}

// tableListFindRow is findSchemaRowIn (pragma.go) widened to match EITHER
// "table" or "view" -- table_list lists both, unlike every other object-scoped
// pragma here, which only ever wants one type at a time.
func tableListFindRow(rows []SchemaRow, scope schemaScope, name string) *SchemaRow {
	var main *SchemaRow
	for i := range rows {
		r := &rows[i]
		if (r.Type != "table" && r.Type != "view") || !equalFoldName(r.Name, name) || !scope.accepts(r.Temp) {
			continue
		}
		if r.Temp {
			return r
		}
		if main == nil {
			main = r
		}
	}
	return main
}

// tableListOrdinaryTableRow builds an ordinary (non-virtual, non-shadow) TABLE
// row: ncol/wr/strict all come straight from parsePragmaTableDef
// (pragma_parse.go), the SAME re-parse table_info/table_xinfo already trust.
func tableListOrdinaryTableRow(schemaName string, r *SchemaRow) ([]Value, error) {
	def, err := parsePragmaTableDef(r.SQL)
	if err != nil {
		return nil, fmt.Errorf("engine: PRAGMA table_list: %w", err)
	}
	return []Value{
		{Typ: Text, S: []byte(schemaName)},
		{Typ: Text, S: []byte(r.Name)},
		{Typ: Text, S: []byte("table")},
		{Typ: Int, I: int64(len(def.cols))},
		{Typ: Int, I: boolToInt(def.withoutRowid)},
		{Typ: Int, I: boolToInt(def.strict)},
	}, nil
}

// tableListViewRow builds a VIEW row: ncol is pragmaViewInfo's (pragma.go) own
// resolved row count, so this inherits its declines (a COMPOUND view, the one
// cached-view-body edge case) rather than re-deriving them.
func (p *ReadOnlyPager) tableListViewRow(schemaName string, r *SchemaRow) ([]Value, error) {
	_, rows, err := p.pragmaViewInfo(r, false)
	if err != nil {
		return nil, err
	}
	return []Value{
		{Typ: Text, S: []byte(schemaName)},
		{Typ: Text, S: []byte(r.Name)},
		{Typ: Text, S: []byte("view")},
		{Typ: Int, I: int64(len(rows))},
		{Typ: Int, I: 0},
		{Typ: Int, I: 0},
	}, nil
}

// tableListVirtualRow builds a VIRTUAL row for a module verified to declare no
// hidden columns C SQLite's own build doesn't also declare -- rtree and
// rtree_i32 only; every other module errors rather than guess (see this file's
// package doc comment).
func (p *ReadOnlyPager) tableListVirtualRow(schemaName string, tr *SchemaRow) ([]Value, error) {
	_, mod, args, _, perr := parseCreateVirtualTableStmt(tr.SQL)
	if perr != nil {
		return nil, fmt.Errorf("engine: PRAGMA table_list: %w", perr)
	}
	lmod := r33sFoldIdent(mod)
	if lmod != "rtree" && lmod != "rtree_i32" {
		return nil, fmt.Errorf("%w: PRAGMA table_list(%s): virtual table module %q's own ncol (which counts its module's hidden columns too) is not verified against the oracle -- only rtree/rtree_i32 are (zero hidden columns, verified directly)", errVDBEUnsupported, tr.Name, mod)
	}
	moduleImpl, ok := lookupVtabModule(mod)
	if !ok {
		return nil, fmt.Errorf("engine: no such module: %s", mod)
	}
	vcols, _, cerr := vtabConnect(p.fts3Catalog(), moduleImpl, args)
	if cerr != nil {
		return nil, fmt.Errorf("engine: PRAGMA table_list: %s: %w", tr.Name, cerr)
	}
	return []Value{
		{Typ: Text, S: []byte(schemaName)},
		{Typ: Text, S: []byte(tr.Name)},
		{Typ: Text, S: []byte("virtual")},
		{Typ: Int, I: int64(len(vcols))},
		{Typ: Int, I: 0},
		{Typ: Int, I: 0},
	}, nil
}

// tableListSyntheticCatalogRow is the always-present sqlite_schema (main) or
// sqlite_temp_schema (temp) row -- type "table", ncol 5 (the fixed
// type/name/tbl_name/rootpage/sql shape, sqliteSchemaCatalogColumns,
// pragma.go), wr/strict both 0. It has no schema row of its own (see
// isMainSchemaCatalogName's doc comment), so this is the one table_list row
// that is never derived from a schema scan.
func tableListSyntheticCatalogRow(schemaName string, temp bool) []Value {
	name := "sqlite_schema"
	if temp {
		name = "sqlite_temp_schema"
	}
	return []Value{
		{Typ: Text, S: []byte(schemaName)},
		{Typ: Text, S: []byte(name)},
		{Typ: Text, S: []byte("table")},
		{Typ: Int, I: 5},
		{Typ: Int, I: 0},
		{Typ: Int, I: 0},
	}
}

// isMasterSchemaCatalogAlias/isTempMasterSchemaCatalogAlias recognize table_list's
// OWN, narrower schema-catalog alias rule: only the "_master" spellings
// resolve to a row here ("PRAGMA table_list('sqlite_master')" answers ONE row
// named "sqlite_schema"), while the modern "_schema" spellings answer ZERO --
// verified directly against mattn/go-sqlite3 3.53.3, both case-insensitively.
// This is deliberately NOT isMainSchemaCatalogName/isTempSchemaCatalogName
// (pragma.go), which match BOTH spellings for table_info's own (different)
// rule.
func isMasterSchemaCatalogAlias(name string) bool {
	return equalFoldName(name, "sqlite_master") || equalFoldName(name, "sqlite_temp_master")
}

func isTempMasterSchemaCatalogAlias(name string) bool {
	return equalFoldName(name, "sqlite_temp_master")
}

// tableListPrivateStoreVtab reports whether schemaRows (filtered to scope)
// holds a virtual table from one of query.go's vtabPrivateStoreModules
// (rtree/rtree_i32) -- the modules whose real shadow tables this engine does
// not persist at all, which is why the WHOLE catalog declines rather than
// just that module's own row (see this file's package doc comment).
func tableListPrivateStoreVtab(schemaRows []SchemaRow, scope schemaScope) (string, bool) {
	for i := range schemaRows {
		r := &schemaRows[i]
		if r.Type != "table" || !scope.accepts(r.Temp) || !isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		if _, mod, _, _, perr := parseCreateVirtualTableStmt(r.SQL); perr == nil && vtabPrivateStoreModules[r33sFoldIdent(mod)] {
			return r33sFoldIdent(mod), true
		}
	}
	return "", false
}

// tableListNameMatchesAnyShadow reports whether name matches one of an fts3/
// fts4/fts5 virtual table's own known shadow-table suffixes (vtab_fts3.go's
// fts3ShadowSuffixes/fts4ShadowSuffixes, fts5_shadow.go's fts5ShadowSuffixes)
// for any such module present in schemaRows -- C SQLite's own shadow-table
// recognition is likewise name-pattern-based (against the CREATING module's
// known suffix set), so matching the identical pattern is not a heuristic
// approximation of that rule, it IS that rule. Used only to DECLINE a match
// (see this file's package doc comment for why classifying one is not
// attempted), so over-matching (fts4's fuller suffix list even under a
// content= table missing "_content") only widens the decline, never answers
// wrong.
func tableListNameMatchesAnyShadow(schemaRows []SchemaRow, name string) bool {
	for i := range schemaRows {
		r := &schemaRows[i]
		if r.Type != "table" || !isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		vtabName, mod, _, _, perr := parseCreateVirtualTableStmt(r.SQL)
		if perr != nil {
			continue
		}
		var suffixes []string
		switch r33sFoldIdent(mod) {
		case "fts3":
			suffixes = fts3ShadowSuffixes
		case "fts4":
			suffixes = fts4ShadowSuffixes
		case "fts5":
			suffixes = fts5ShadowSuffixes
		default:
			continue
		}
		for _, suf := range suffixes {
			if equalFoldName(name, vtabName+suf) {
				return true
			}
		}
	}
	return false
}

// pragmaRoutableToAttached is the set of pragma names an ATTACHed-database
// qualifier may be routed on -- handed to that database's own pager, which
// answers "aux" as its OWN local schema (SetLocalSchema at attach time,
// attach.go), so the recursion resolves locally there and stops.
//
// Membership is exactly "reads that database's CATALOG and nothing else". A
// pragma that answers from CONNECTION state -- locking_mode, the tuning
// getters, integrity_check (which consults
// PRAGMA ignore_check_constraints) -- is NOT here, because an
// attached-reader pager does not mirror that state and routing it would
// answer from the wrong place rather than turn a clean decline into a
// correct answer.
//
// table_list and foreign_key_check were the first two. The six schema-object
// pragmas joined them once measured: "PRAGMA aux.table_info(t)" and
// "PRAGMA aux.index_list(t)" are ordinary answers in the 3.53.3 oracle and
// were "unknown database aux" here, even though "SELECT * FROM aux.t" over
// the very same catalog worked.
var pragmaRoutableToAttached = map[string]bool{
	"table_list":        true,
	"foreign_key_check": true,
	"table_info":        true,
	"table_xinfo":       true,
	"index_list":        true,
	"index_info":        true,
	"index_xinfo":       true,
	"foreign_key_list":  true,
}
