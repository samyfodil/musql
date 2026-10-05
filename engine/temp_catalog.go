// This file serves the SCHEMA CATALOGS -- sqlite_master / sqlite_schema and
// sqlite_temp_master / sqlite_temp_schema, plus their schema-qualified
// spellings main.sqlite_master and temp.sqlite_master -- in the two states
// where a plain scan of a b-tree cannot answer them.
//
// Both catalogs are REAL b-trees now: main's is page 1 of main's file and
// temp's is page 1 of the TEMP database's own file (temp_store.go, real
// SQLite's db->aDb[1]), so an ordinary resolveTable scan answers either one,
// rootpage and rowid included. This file is what is left over:
//
//   - a temp-catalog name on a connection that never opened a temp database.
//     C SQLite answers zero rows rather than "no such table" ("CREATE TEMP
//     TABLE tt(z); DROP TABLE tt" then "SELECT count(*) FROM
//     sqlite_temp_master" is 0, verified directly), and with no aDb[1] file
//     there is no page 1 to scan: an empty materialized source IS that answer;
//   - a CREATE TABLE ... AS SELECT reading the catalog its own new table
//     belongs to. sqlite3StartTable reserves that table's five-NULL row before
//     the SELECT runs (build.c:1370-1375), and this engine writes catalog rows
//     only when the statement ends, so the row has to be appended here --
//     table.test 19.1, SQLite ticket acd12990885d9276.
//
// A TEMP object's stored "sql" has its TEMP/TEMPORARY keyword STRIPPED:
// "CREATE TEMP TABLE tt(z UNIQUE)" reads back "CREATE TABLE tt(z UNIQUE)", and
// "CREATE   TEMPORARY   TABLE   spaced  (  a  ,  b  )" reads back
// "CREATE TABLE spaced  (  a  ,  b  )" -- SQLite reprints only the
// "CREATE <kind> " prefix and keeps the text from the NAME token verbatim.
// The temp database's own catalog therefore stores the stripped text
// (tempReader, temp_store.go), and withoutTempKeyword below is what strips it.
//
// Which catalog a FROM name means is catalogItemScope's separate question, and
// it is a NAME question only: "main.sqlite_temp_master" is "no such table" in
// C SQLite, "temp.sqlite_master" is the temp catalog, and an unqualified
// "sqlite_master" is always main's, never the temp database's, even though an
// unqualified TABLE name searches temp first (sqlite3FindTable's "search TEMP
// before MAIN" loop).
package engine

import "fmt"

// catalogItemScope reports which schema catalog FROM item it names, if any.
// Purely a NAME question -- whether that catalog can actually be answered in
// the pager's current state is schemaCatalogSourceScope's job.
//
// The spellings, and what C SQLite does with each (verified directly):
//
//	sqlite_master / sqlite_schema, unqualified or "main."  -> main's catalog
//	sqlite_temp_master / sqlite_temp_schema, unqualified   -> temp's catalog
//	temp.sqlite_master / temp.sqlite_schema                -> temp's catalog
//	temp.sqlite_temp_master                                -> temp's catalog
//	main.sqlite_temp_master                                -> "no such table"
//
// A foreign qualifier ("aux.sqlite_master") is deliberately NOT claimed: an
// ATTACHed database is a separate file with a separate pager whose own schema
// b-tree holds no temp objects at all, so the ordinary resolveTable path reads
// it correctly already.
func catalogItemScope(it FromItem) (schemaScope, bool) {
	if it.Subquery != nil || it.TableFunc || it.Table == "" {
		return scopeAny, false
	}
	q, ok := scopeOfQualifier(it.Schema)
	if !ok {
		return scopeAny, false
	}
	switch {
	case isTempSchemaCatalogName(it.Table):
		// "main.sqlite_temp_master" is "no such table" in C SQLite; the
		// unqualified and "temp."-qualified spellings both name temp.
		if q == scopeMain {
			return scopeAny, false
		}
		return scopeTemp, true
	case isMainSchemaCatalogName(it.Table):
		if q == scopeTemp {
			return scopeTemp, true
		}
		return scopeMain, true
	}
	return scopeAny, false
}

// schemaCatalogSourceScope reports whether FROM item it should compile to the
// filtered catalog row source below, and for which catalog. It reports FALSE
// (never an error) for anything this engine cannot split exactly, so the caller
// falls through to resolveTable and the name reports the same "no such table"
// it always did.
//
// The MAIN direction is claimed only while a temp object actually EXISTS.
// Without one there is nothing to filter out, and sqlite_master already
// resolves straight to the live schema b-tree (resolveTable, query.go) -- the
// path every other resolver in this package shares. Leaving that case exactly
// as it was keeps one code path for the overwhelmingly common state instead of
// two that have to agree.
func (p *ReadOnlyPager) schemaCatalogSourceScope(it FromItem) (schemaScope, bool, error) {
	if p != nil && equalFoldName(p.localSchema, "temp") {
		// This IS the TEMP database (temp_store.go): its catalog is its own
		// page 1, read by an ordinary scan like any other database's, with
		// nothing to filter out of it.
		//
		// The exception is a CREATE TEMP TABLE ... AS SELECT in flight, whose
		// new table already has its five-NULL row reserved in THIS catalog
		// (sqlite3StartTable, build.c:1370-1375, before the SELECT runs). A
		// b-tree scan cannot see a row this engine only adds at the end of the
		// statement, so the materialized source serves it instead -- the same
		// answer, plus that row.
		if p.ctasPlaceholder && p.ctasPlaceholderScope == scopeTemp {
			if _, ok := catalogItemScope(it); ok {
				return scopeTemp, true, nil
			}
		}
		return scopeAny, false, nil
	}
	scope, ok := catalogItemScope(it)
	if !ok {
		return scopeAny, false, nil
	}
	if _, isCTE := p.lookupCTE(it.Table); isCTE {
		return scopeAny, false, nil
	}
	// MAIN's catalog is MAIN's alone now: every temp object lives in the temp
	// database's own file (temp_store.go), so there is nothing to filter out of
	// the b-tree this pager scans and sqlite_master resolves straight to it
	// (resolveTable, query.go). Two cases still want the materialized source:
	//
	//   - a temp-scoped name with no temp database open. C SQLite's answer
	//     is an EMPTY catalog -- aDb[1] has no page 1 until something is put in
	//     it -- and an empty filtered source is exactly that;
	//   - a CREATE TABLE ... AS SELECT in flight, whose new table already has a
	//     five-NULL row reserved in the catalog it belongs to
	//     (sqlite3StartTable, build.c:1370-1375, which runs BEFORE the SELECT).
	//     This engine adds that row only when the statement ends, so a b-tree
	//     scan would miss it -- table.test 19.1's whole point (SQLite ticket
	//     acd12990885d9276).
	if p.ctasPlaceholder && p.ctasPlaceholderScope == scope {
		return scope, true, nil
	}
	return scope, scope == scopeTemp, nil
}

// withoutTempKeyword is withTempKeyword's inverse: the stored CREATE text as
// C SQLite writes it into the TEMP catalog, with the TEMP/TEMPORARY keyword
// this engine keeps as its on-disk catalog marker taken back out. Reconstructs
// the "CREATE " prefix rather than splicing the original bytes, which is what
// SQLite itself does ("CREATE %s %.*s" over the text from the NAME token) --
// so "CREATE   TEMPORARY   TABLE   spaced  (a)" renders as
// "CREATE TABLE   spaced  (a)" exactly as normalizeSchemaSQL's canonical
// "CREATE TEMPORARY TABLE spaced  (a)" renders as "CREATE TABLE spaced  (a)".
// Text this cannot confidently re-render is returned unchanged.
func withoutTempKeyword(sqlText string) string {
	if !isTempCreateSQL(sqlText) {
		return sqlText
	}
	toks, err := lex(sqlText)
	if err != nil || len(toks) < 3 || toks[2].kind != tkIdent || toks[2].Start >= len(sqlText) {
		return sqlText
	}
	return "CREATE " + sqlText[toks[2].Start:]
}

// schemaCatalogRows materializes one catalog: the live schema b-tree
// (Schema(), in b-tree order) filtered to scope and rendered into
// sqlite_master's fixed five-column shape. Filtering preserves that order, so
// each catalog comes out in exactly the order a raw scan of the b-tree would
// have given it -- see this file's package comment for what that order is and
// is not.
//
// While a direct sqlite_master write is outstanding, the source is
// p.schemaRowsEdited (pager.go) instead of Schema(): Schema() is deliberately
// seeded with the PRE-edit rows while an edit is active (SnapshotPager,
// writer.go), which is correct for ordinary table resolution but would make
// THIS filtered read -- unlike an unfiltered scan of page 1, which already
// bakes the edit in -- silently show the schema as if the edit had never
// happened. See writableSchemaEditedRows' own doc comment
// (schema_write_direct.go).
//
// An empty sql (the NULL sentinel for an implicitly-created
// sqlite_autoindex_* row) renders as SQL NULL, which is what C SQLite
// stores there.
func (p *ReadOnlyPager) schemaCatalogRows(scope schemaScope) ([][]Value, error) {
	rows := p.schemaRowsEdited
	if rows == nil {
		var err error
		rows, err = p.Schema()
		if err != nil {
			return nil, err
		}
	}
	out := make([][]Value, 0, len(rows))
	for _, r := range rows {
		if !scope.accepts(r.Temp) {
			continue
		}
		sqlText := r.SQL
		if r.Temp {
			sqlText = withoutTempKeyword(sqlText)
		}
		sqlVal := Value{Typ: Null}
		if sqlText != "" {
			sqlVal = Value{Typ: Text, S: []byte(sqlText)}
		}
		out = append(out, []Value{
			{Typ: Text, S: []byte(r.Type)},
			{Typ: Text, S: []byte(r.Name)},
			{Typ: Text, S: []byte(r.TblName)},
			{Typ: Int, I: int64(r.RootPage)},
			sqlVal,
		})
	}
	// The row a CREATE TABLE ... AS SELECT in flight has already reserved for
	// its own new table: all five columns NULL, after every real row, and only
	// in the catalog that table belongs to. See ReadOnlyPager.ctasPlaceholder
	// for the oracle evidence.
	if p.ctasPlaceholder && p.ctasPlaceholderScope == scope {
		out = append(out, []Value{
			{Typ: Null}, {Typ: Null}, {Typ: Null}, {Typ: Null}, {Typ: Null},
		})
	}
	return out, nil
}

// resolveSchemaCatalogSource builds the filtered catalog join source: the fixed
// catalog column shape, with the rows materialized at run time by
// OpOpenDerived's catalogScope branch (vdbe.go). Modelled on resolveVtabSource,
// which is the other materialized-source-with-no-b-tree in this compiler.
func (p *ReadOnlyPager) resolveSchemaCatalogSource(c *compiler, state *joinDesugarState, i int, it FromItem, offset int, scope schemaScope) (joinSource, error) {
	if it.IndexedBy != "" {
		// A catalog has no b-tree, so no index can be hinted against it --
		// the same rule resolveFrom applies (join.go).
		return joinSource{}, fmt.Errorf("%w: INDEXED BY on the schema catalog", errVDBEUnsupported)
	}
	cols := sqliteSchemaCatalogColumns()
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	onExpr, hidden, derr := desugarJoinItem(state, i, it, name, cols)
	if derr != nil {
		return joinSource{}, declineOrSemantic(derr)
	}
	ts := tableScope{name: name, tableName: it.Table, cols: cols, colIndex: buildColIndex(cols), offset: offset, coalesced: hidden}
	return joinSource{
		tbl:          &resolvedTable{cols: cols, ipkIndex: -1},
		scope:        compileScope{tableScope: ts, cursor: c.allocCursor()},
		left:         it.Join == JoinLeft || it.Join == JoinFull,
		rightOuter:   it.Join == JoinRight || it.Join == JoinFull,
		on:           onExpr,
		derivedSlot:  c.allocSub(),
		catalogScope: scope,
	}, nil
}
