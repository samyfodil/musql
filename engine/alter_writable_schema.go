// Under PRAGMA writable_schema=ON, ALTER TABLE RENAME does not rewrite
// schema objects that would fail re-parsing (alter.c:1883-1885).
package engine

// renameUnparsableObjects returns triggers and views that cannot be re-parsed,
// which C SQLite leaves untouched during rename under writable_schema=ON.
func (db *DB) renameUnparsableObjects(tbl *tableMeta) (triggers, views map[string]bool) {
	if !db.writableSchema {
		return nil, nil
	}
	// The same lookup checkSchemaObjectsResolve's own `missing` closure uses --
	// a VIRTUAL table counts as resolvable, and an unqualified reference from a
	// TEMP object may reach an attachment. See that function for why both.
	missing := func(names []string, allowAttached bool) bool {
		for _, n := range names {
			if db.findTableMeta(n) != nil || db.findViewMeta(n) != nil || db.findVtabMeta(n) != nil {
				continue
			}
			if allowAttached && db.firstAttachedTable(n) != nil {
				continue
			}
			return true
		}
		return false
	}

	for _, tr := range db.triggers {
		if alterSkipsCatalog(tbl.isTemp, tr.isTemp) {
			continue
		}
		var names []string
		for _, bs := range tr.body {
			switch {
			case bs.insert != nil:
				names = append(names, bs.insert.table)
				collectSelectTables(bs.insert.selectStmt, &names)
			case bs.update != nil:
				names = append(names, bs.update.table)
			case bs.delete != nil:
				names = append(names, bs.delete.table)
			case bs.sel != nil:
				collectSelectTables(bs.sel, &names)
			}
		}
		if missing(names, tr.isTemp) {
			if triggers == nil {
				triggers = map[string]bool{}
			}
			triggers[tr.name] = true
		}
	}

	for _, v := range db.views {
		if alterSkipsCatalog(tbl.isTemp, v.isTemp) {
			continue
		}
		var names []string
		collectSelectTables(v.selectStmt, &names)
		if missing(names, v.isTemp) || db.flatViewNamesMissingColumn(v) {
			if views == nil {
				views = map[string]bool{}
			}
			views[v.name] = true
		}
	}
	return triggers, views
}

// flatViewNamesMissingColumn reports whether v provably selects a column its
// source table does not have -- the half of "does this re-parse" that a
// missing-TABLE scan cannot see, and the half t4v1 fails on.
//
// C reaches the same verdict by preparing the view's SELECT for real
// (sqlite3ViewGetColumnNames -> viewGetColumnNames, build.c:3210). This engine
// has no resolver reachable from the write path -- resolveViewColumns is a
// read-path compiler function needing a *ReadOnlyPager and a *compiler, and the
// write path can legitimately hold a nil pager (never-panic). So this answers
// only the narrow shape it can answer exactly, and returns false -- "cannot
// prove it" -- for everything else:
//
//   - exactly one FROM item, naming a real TABLE in the view's own catalog
//     (a view, virtual table or subquery source is not decided here)
//   - no CTEs and no compound arms
//   - every result column a PLAIN column reference, bare or qualified by that
//     one source's own name or alias
//
// A "*" is skipped rather than refused: it names no column, so it cannot be the
// missing one. A three-part reference, a function call, an expression or a
// qualifier naming something else all return false, because each could resolve
// in a way this scan does not model.
func (db *DB) flatViewNamesMissingColumn(v *viewMeta) bool {
	sel := v.selectStmt
	if sel == nil || len(sel.CTEs) != 0 || len(sel.Compound) != 0 || len(sel.From) != 1 {
		return false
	}
	it := sel.From[0]
	if it.Subquery != nil || it.TableFunc || it.Table == "" {
		return false
	}
	src := db.findTableMetaIn(createScope(v.isTemp), it.Table)
	if src == nil {
		return false // a view/vtab source, or missing entirely: missing() decides that
	}
	for _, sc := range sel.Columns {
		if sc.Star {
			continue
		}
		ce, ok := sc.Expr.(ColumnExpr)
		if !ok || ce.Schema != "" {
			return false
		}
		if ce.Qualifier != "" &&
			!equalFoldName(ce.Qualifier, it.Table) && !equalFoldName(ce.Qualifier, it.Alias) {
			return false
		}
		// A rowid alias is a real column of any rowid table even though it is
		// not in cols (column_scope.go's isRowidAliasName).
		if isRowidAliasName(ce.Name) && !src.withoutRowid {
			continue
		}
		if colIndexByName(src.cols, ce.Name) < 0 {
			return true
		}
	}
	return false
}
