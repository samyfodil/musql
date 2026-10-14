package engine

// updateFromSource is what updateFromDupError needs of one UPDATE ... FROM
// source: its declared columns, the database it lives in, and whether it has a
// rowid. ok is false for anything that is not a plain table (a view, a CTE, a
// subquery), whose diagnostics are not modelled.
type updateFromSource struct {
	cols   []columnInfo
	dbName string
	rowid  bool
	ok     bool
}

// updateFromDupError is the error C raises for an UPDATE ... FROM whose FROM
// list and target share a name, in C's order:
//
//  1. Two or more FROM sources are wrapped in an SF_NestedFrom subquery
//     (parse.y's UPDATE rule) whose "*" expands to "db.name.col" per source
//     column and "db.name._ROWID_" per rowid. That list is resolved before the
//     rest of the statement, so a name two sources share is "ambiguous column
//     name: main.t2.id" -- the first entry two of them answer (a later
//     source's own USING skips its copy, resolve.c:436-447).
//  2. Otherwise the SET expressions and the WHERE resolve (updateFromSelect's
//     select list, then WHERE); a qualified reference both the target and a
//     same-named source answer is "ambiguous column name: t.id".
//  3. Failing both, sqlite3Select's SF_UFSrcCheck rejects the duplicate:
//     "target object/alias may not appear in FROM clause: t".
//
// nil when there is no duplicate; the caller's generic error when a source
// involved is not a plain table.
func updateFromDupError(stmt *updateStmt, tbl *tableMeta, targetName string, lookup func(FromItem) updateFromSource) error {
	itemName := func(it FromItem) string {
		if it.Alias != "" {
			return it.Alias
		}
		if it.Subquery == nil {
			return it.Table
		}
		return ""
	}
	from := stmt.from
	if len(from) >= 2 {
		dup := false
		seen := map[string]bool{}
		for _, it := range from {
			n := r33sFoldIdent(itemName(it))
			if n == "" {
				continue
			}
			if seen[n] {
				dup = true
			}
			seen[n] = true
		}
		if dup {
			srcs := make([]updateFromSource, len(from))
			for i, it := range from {
				if srcs[i] = lookup(it); !srcs[i].ok {
					return semanticf("engine: ambiguous column name: %s", itemName(it))
				}
			}
			for i, it := range from {
				name := itemName(it)
				for _, c := range srcs[i].cols {
					if qualifiedAmbiguous(from, srcs, name, c.Name, itemName) {
						return semanticf("engine: ambiguous column name: %s.%s.%s", srcs[i].dbName, name, c.Name)
					}
				}
				if srcs[i].rowid {
					alias := rowidAliasCandidate(srcs[i].cols)
					n := 0
					for j := range from {
						if equalFoldName(itemName(from[j]), name) && srcs[j].rowid {
							n++
						}
					}
					if alias != "" && n > 1 {
						return semanticf("engine: ambiguous column name: %s.%s.%s", srcs[i].dbName, name, alias)
					}
				}
			}
		}
	}
	var clash *FromItem
	for i := range from {
		if equalFoldName(itemName(from[i]), targetName) {
			clash = &from[i]
			break
		}
	}
	if clash == nil {
		return nil
	}
	src := lookup(*clash)
	if !src.ok {
		return semanticf("engine: ambiguous column name: %s", targetName)
	}
	// sameSrcAlias (select.c:7220) rejects only the same TABLE under the same
	// alias (both none counts as the same). A different table sharing the
	// name is legal: each qualified reference binds to whichever of the two
	// has the column, and one both have is ambiguous at resolution.
	targetDB := "main"
	if tbl.isTemp {
		targetDB = "temp"
	}
	if !equalFoldName(clash.Table, tbl.name) || src.dbName != targetDB || !equalFoldName(clash.Alias, stmt.alias) {
		return nil
	}
	both := func(col string) bool {
		inTarget, inSrc := false, false
		for _, c := range tbl.cols {
			inTarget = inTarget || equalFoldName(c.Name, col)
		}
		for _, c := range src.cols {
			inSrc = inSrc || equalFoldName(c.Name, col)
		}
		return inTarget && inSrc
	}
	var hit string
	visit := func(e Expr) {
		forEachColumnRef(e, func(ce ColumnExpr) {
			if hit == "" && ce.Schema == "" && equalFoldName(ce.Qualifier, targetName) && both(ce.Name) {
				hit = ce.Qualifier + "." + ce.Name
			}
		})
	}
	for _, a := range stmt.sets {
		visit(a.expr)
	}
	visit(stmt.where)
	if hit != "" {
		return semanticf("engine: ambiguous column name: %s", hit)
	}
	return semanticf("engine: target object/alias may not appear in FROM clause: %s", targetName)
}

// qualifiedAmbiguous reports whether "name.col" is answered by two sources
// named name: lookupName counts each, except one after the first whose own
// USING names col.
func qualifiedAmbiguous(from []FromItem, srcs []updateFromSource, name, col string, itemName func(FromItem) string) bool {
	n := 0
	for j, it := range from {
		if !equalFoldName(itemName(it), name) {
			continue
		}
		has := false
		for _, c := range srcs[j].cols {
			has = has || equalFoldName(c.Name, col)
		}
		if !has {
			continue
		}
		if n > 0 {
			skip := false
			for _, u := range it.Using {
				skip = skip || equalFoldName(u, col)
			}
			if it.Natural {
				// The synthesized USING lists the columns an earlier source shares.
				for k := 0; k < j; k++ {
					for _, c := range srcs[k].cols {
						skip = skip || equalFoldName(c.Name, col)
					}
				}
			}
			if skip {
				continue
			}
		}
		n++
	}
	return n > 1
}

// forEachColumnRef calls fn on every ColumnExpr of e outside a subquery.
func forEachColumnRef(e Expr, fn func(ColumnExpr)) {
	if e == nil {
		return
	}
	if ce, ok := e.(ColumnExpr); ok {
		fn(ce)
		return
	}
	switch x := e.(type) {
	case FuncExpr:
		forEachColumnRef(x.Filter, fn)
	}
	walkExprOperands(e, func(op Expr) { forEachColumnRef(op, fn) })
}

// updateFromSourceLookup resolves an UPDATE ... FROM source to a plain table of
// this session's catalog for updateFromDupError: TEMP shadows main when
// unqualified, a CTE name or anything else is not modelled.
func (db *DB) updateFromSourceLookup(ctes []CTEDef) func(FromItem) updateFromSource {
	return func(it FromItem) updateFromSource {
		if it.Subquery != nil || it.TableFunc || it.Table == "" {
			return updateFromSource{}
		}
		scope := scopeAny
		if it.Schema != "" {
			sc, ok := scopeOfQualifier(it.Schema)
			if !ok || sc == scopeAny {
				return updateFromSource{}
			}
			scope = sc
		} else {
			for _, d := range ctes {
				if equalFoldName(d.Name, it.Table) {
					return updateFromSource{}
				}
			}
		}
		t := db.findTableMetaIn(scope, it.Table)
		if t == nil {
			return updateFromSource{}
		}
		dbName := "main"
		if t.isTemp {
			dbName = "temp"
		}
		return updateFromSource{cols: t.cols, dbName: dbName, rowid: !t.withoutRowid, ok: true}
	}
}
