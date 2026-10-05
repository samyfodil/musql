// This file implements "ALTER TABLE <virtual table> RENAME TO <new>", the one
// ALTER form C SQLite allows on a virtual table. The other three are
// refused there, each with its own wording, and resolveAlterTarget
// (alter_write.go) reproduces those.
//
// The module's xRename callback (e.g., fts3RenameMethod) renames the shadow
// tables by executing nested ALTER TABLE RENAME statements. The new name is
// force-quoted in all rewritten names, module arguments are unchanged, and the
// automatic index follows the renamed shadow table.
//
// Rtree renames are declined (rtree rows live in a private store, not
// in _rowid/_node/_parent shadow tables). Views or triggers naming
// shadow tables are declined to avoid partial renames.
package engine

import (
	"fmt"
)

// locateCreateVirtualTableNameToken is locateCreateTableNameToken
// (alter_write.go) for a "CREATE [TEMP] VIRTUAL TABLE [IF NOT EXISTS]
// [schema.]name USING ..." statement -- the stored text of a vtabMeta, which
// that function rejects outright ("not a CREATE TABLE statement"). The TEMP
// keyword is this engine's own on-disk marker for the temp catalog
// (withTempKeyword, temp_schema.go), so it has to be skipped here even though
// C SQLite never writes one.
func locateCreateVirtualTableNameToken(createSQL string) (token, error) {
	toks, err := lex(createSQL)
	if err != nil {
		return token{}, err
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: not a CREATE VIRTUAL TABLE statement")
	}
	kw("TEMP")
	kw("TEMPORARY")
	if !kw("VIRTUAL") || !kw("TABLE") {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: not a CREATE VIRTUAL TABLE statement")
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: expected virtual table name")
	}
	nameTok := toks[i]
	i++
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i++
		if i >= len(toks) || !isNameToken(toks[i]) {
			return token{}, fmt.Errorf("engine: internal error: ALTER TABLE RENAME TO: expected name after schema qualifier")
		}
		nameTok = toks[i]
	}
	return nameTok, nil
}

// vtabShadowSuffixes returns the shadow-table suffixes vm's module owns, in
// the order the module creates them. Empty for a module with no shadow tables
// of its own (fts4aux, fts3tokenize, generate_series). The list is the
// MODULE's, deliberately: a "<oldname>_" prefix sweep would take a user table
// named "<vtab>_anything" with it.
func (db *DB) vtabShadowSuffixes(vm *vtabMeta) ([]string, error) {
	if _, ok := vm.store.(*rtreeStore); ok {
		// rtree owns three, and renaming them is not optional: rtreeRename
		// (rtree.c:3284-3286) is three "ALTER TABLE %Q.'%q_node' RENAME TO
		// \"%w_node\"" statements, and without them the renamed table cannot
		// be read at all -- its own reader looks for "<newname>_node".
		// This file's header already recorded the behaviour ("rtree's
		// rt -> rt2 moves _rowid/_node/_parent"); it became reachable when
		// rtree stopped keeping a private store (rtree_shadow.go).
		return []string{"_rowid", "_node", "_parent"}, nil
	}
	if fm, ok := fts3ModuleOf(vm); ok {
		sch, err := fm.schemaOf(db, vm)
		if err != nil {
			return nil, fmt.Errorf("%w: ALTER TABLE %s RENAME TO: its module arguments no longer resolve, so which shadow tables it owns cannot be decided: %v", errVDBEUnsupported, vm.name, err)
		}
		return fm.shadowSuffixes(sch), nil
	}
	if fm, ok := fts5ModuleOf(vm); ok {
		// %_docsize exists only on a columnsize=1 table, exactly as
		// createShadowTables decides it (fts5_shadow.go) -- and exactly as
		// C fts5's xRename does, which renames it only when
		// pConfig->bColumnsize is set.
		st, err := fm.buildStore(vm.name, vm.args)
		if err != nil {
			return nil, fmt.Errorf("%w: ALTER TABLE %s RENAME TO: its module arguments no longer resolve, so which shadow tables it owns cannot be decided: %v", errVDBEUnsupported, vm.name, err)
		}
		out := make([]string, 0, len(fts5ShadowSuffixes))
		for _, s := range fts5ShadowSuffixes {
			if s == "_docsize" && !st.columnsize {
				continue
			}
			// ...and %_content only on a NORMAL-content table, which is the
			// same condition sqlite3Fts5StorageRename applies
			// (ext/fts5/fts5_storage.c:298, "if( pConfig->eContent==
			// FTS5_CONTENT_NORMAL ) fts5StorageRenameOne(...,\"content\",...)")
			// and the same one dropShadowTables already applies for DROP: an
			// external-content table never had a %_content of its own, so
			// requiring one made "ALTER TABLE t RENAME TO t2" over
			// "fts5(a, content=src, content_rowid=id)" answer "shadow table
			// t_content is missing" and leave the schema untouched.
			//
			// The test is eContent, NOT "does a %_content exist": a CONTENTLESS
			// table has none either (so requiring one made a plain
			// "fts5(a, content='')" rename fail the same way -- fts5alter.test),
			// and a contentless_unindexed=1 one HAS one that fts5 nonetheless
			// leaves behind under the OLD name. Verified against the oracle: a
			// renamed contentless_unindexed table's schema still holds
			// "t_content" beside "t2_data"/"t2_idx"/..., and every read of t2 is
			// then "no such table: main.t2_content". Renaming it here instead
			// answered the row -- a wrong answer, and the one this option's
			// support exposed.
			if s == "_content" && (st.extContent != "" || st.contentless) {
				continue
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, nil
}

// vtabShadowTables resolves vm's shadow tables, in module order, paired with
// the name each would take under newName.
//
// EVERY suffix the module owns has to be present. C SQLite's xRename issues
// one nested "ALTER TABLE ... RENAME TO" per shadow and the whole statement
// fails if any of them is missing -- verified against 3.53.3 by dropping one
// shadow at a time from an fts4 table ("DROP TABLE ft_content", then _stat,
// then _docsize, then _segdir): every one of them makes
// "ALTER TABLE ft RENAME TO gt" answer "SQL logic error" and leaves the schema
// untouched. Renaming the survivors instead would be an ACCEPT where the
// oracle errors, which is the wrong-answer direction.
func (db *DB) vtabShadowTables(vm *vtabMeta, newName string) (tables []*tableMeta, newNames []string, err error) {
	suffixes, serr := db.vtabShadowSuffixes(vm)
	if serr != nil {
		return nil, nil, serr
	}
	scope := createScope(vm.isTemp)
	for _, suffix := range suffixes {
		tbl := db.findTableMetaIn(scope, vm.name+suffix)
		if tbl == nil {
			return nil, nil, fmt.Errorf("engine: ALTER TABLE %s RENAME TO %s: shadow table %s is missing", vm.name, newName, vm.name+suffix)
		}
		tables = append(tables, tbl)
		newNames = append(newNames, newName+suffix)
	}
	return tables, newNames, nil
}

// renameVtabTo implements "ALTER TABLE <vtab> RENAME TO newName".
//
// The structure mirrors renameTableTo (alter_write.go): every check that can
// fail runs FIRST, with nothing applied, and the cascades that CAN fail
// mid-flight are guarded by the same snapshot-and-restore shape. The shadow
// renames themselves are done here rather than by recursing into
// renameTableTo, because that function's own view/trigger cascade would then
// run once per shadow and a failure in the third one could not be undone
// text-for-text -- see this file's header.
func (db *DB) renameVtabTo(vm *vtabMeta, newName string) error {
	// A vtabMeta is not a tableMeta, but every cascade helper renameTableTo
	// uses reads only the target's NAME and CATALOG. This stand-in carries
	// exactly those two, so the vtab goes through the identical code rather
	// than a second, separately-maintained copy of it.
	standIn := &tableMeta{name: vm.name, isTemp: vm.isTemp}

	// See renameTableTo's identical comment: under PRAGMA legacy_alter_table
	// neither the schema-wide dangling-reference check nor the view/trigger
	// body rewrite below ever runs, for the same alter.c isLegacy gating (a
	// vtab's own CREATE VIRTUAL TABLE row and its shadows' rename are the
	// UNCONDITIONAL half; nothing about xRename or the shadow sweep is gated
	// on the flag at all).
	legacy := db.LegacyAlterTable()
	if !legacy {
		if err := db.checkSchemaObjectsResolve(standIn); err != nil {
			return err
		}
	}
	if err := db.checkReservedObjectName(newName); err != nil {
		return err
	}
	oldName := vm.name
	shadows, newShadowNames, serr := db.vtabShadowTables(vm, newName)
	if serr != nil {
		return serr
	}

	// Every name this rename would produce has to be free, and the check runs
	// before ANY of them moves -- C SQLite refuses the whole statement and
	// changes nothing when a produced shadow name is taken (verified: with a
	// user "occupied_segdir" present, "ALTER TABLE ft RENAME TO occupied"
	// fails and leaves ft and all five shadows exactly as they were).
	moving := map[string]bool{r33sFoldIdent(oldName): true}
	for _, t := range shadows {
		moving[r33sFoldIdent(t.name)] = true
	}
	scope := createScope(vm.isTemp)
	taken := func(name string) bool {
		if moving[r33sFoldIdent(name)] {
			// One of the objects this statement is itself moving out of the
			// way. "ALTER TABLE ft RENAME TO ft_content" is NOT this case --
			// ft_content moves to ft_content_content, not out of the way --
			// and is caught below, because the vtab's own new name is checked
			// against the pre-rename catalog.
			return false
		}
		return db.findTableMetaIn(scope, name) != nil ||
			db.findVtabMetaIn(scope, name) != nil ||
			db.findViewMetaIn(scope, name) != nil ||
			db.findIndexMetaIn(scope, name) != nil
	}
	if db.findTableMetaIn(scope, newName) != nil || db.findVtabMetaIn(scope, newName) != nil ||
		db.findViewMetaIn(scope, newName) != nil || db.findIndexMetaIn(scope, newName) != nil {
		return fmt.Errorf("engine: there is already another table or index with this name: %s", newName)
	}
	for i, n := range newShadowNames {
		if taken(n) {
			return fmt.Errorf("engine: ALTER TABLE %s RENAME TO %s: there is already another table or index named %s, which %s's shadow table %s would have to become", oldName, newName, n, newName, shadows[i].name)
		}
	}

	// A view or trigger naming a SHADOW table is declined outright -- see this
	// file's header for why it is not merely unimplemented.
	for _, t := range shadows {
		for _, v := range db.views {
			if !alterSkipsCatalog(vm.isTemp, v.isTemp) && sqlMentionsIdent(v.sql, t.name) {
				return fmt.Errorf("%w: ALTER TABLE %s RENAME TO %s: view %s names the shadow table %s, which C SQLite would rewrite with it", errVDBEUnsupported, oldName, newName, v.name, t.name)
			}
		}
		for _, tr := range db.triggers {
			if !alterSkipsCatalog(vm.isTemp, tr.isTemp) && (equalFoldName(tr.table, t.name) || sqlMentionsIdent(tr.sql, t.name)) {
				return fmt.Errorf("%w: ALTER TABLE %s RENAME TO %s: trigger %s names the shadow table %s, which C SQLite would rewrite with it", errVDBEUnsupported, oldName, newName, tr.name, t.name)
			}
		}
	}

	// Under legacy neither this check nor the rewrite it guards ever runs --
	// see renameTableTo's identical `if !legacy` on affectedViews.
	var affectedViews []*viewMeta
	if !legacy {
		var verr error
		affectedViews, verr = db.checkViewRenameSafe(standIn, "ALTER TABLE RENAME TO")
		if verr != nil {
			return verr
		}
	}

	// The vtab's own name token, and every shadow's -- all located before any
	// of them is spliced, so a stored row this engine cannot re-locate the
	// name in declines with nothing applied.
	vtabTok, err := locateCreateVirtualTableNameToken(vm.sql)
	if err != nil {
		return fmt.Errorf("engine: ALTER TABLE RENAME TO: %w", err)
	}
	shadowToks := make([]token, len(shadows))
	for i, t := range shadows {
		tok, terr := locateCreateTableNameToken(t.sql)
		if terr != nil {
			return fmt.Errorf("engine: ALTER TABLE RENAME TO: shadow table %s: %w", t.name, terr)
		}
		shadowToks[i] = tok
	}

	// ---- from here on the schema is mutated; every step is undoable ----

	savedVtabName, savedVtabSQL := vm.name, vm.sql
	savedShadowName := make([]string, len(shadows))
	savedShadowSQL := make([]string, len(shadows))
	for i, t := range shadows {
		savedShadowName[i], savedShadowSQL[i] = t.name, t.sql
	}
	savedIndexName := make([]string, len(db.indexes))
	savedIndexTable := make([]string, len(db.indexes))
	savedIndexSQL := make([]string, len(db.indexes))
	for i, ix := range db.indexes {
		savedIndexName[i], savedIndexTable[i], savedIndexSQL[i] = ix.name, ix.table, ix.sql
	}
	savedTableSQL := make(map[*tableMeta]string, len(db.tables))
	for _, t := range db.tables {
		savedTableSQL[t] = t.sql
	}
	savedTriggers := make(map[*triggerMeta]triggerMeta, len(db.triggers))
	for _, tr := range db.triggers {
		savedTriggers[tr] = *tr
	}
	rollback := func() {
		vm.name, vm.sql = savedVtabName, savedVtabSQL
		for i, t := range shadows {
			t.name, t.sql = savedShadowName[i], savedShadowSQL[i]
		}
		for i, ix := range db.indexes {
			ix.name, ix.table, ix.sql = savedIndexName[i], savedIndexTable[i], savedIndexSQL[i]
		}
		for t, sqlText := range savedTableSQL {
			t.sql = sqlText
		}
		for tr, snap := range savedTriggers {
			*tr = snap
		}
	}

	vm.sql = applyEdits(vm.sql, []textEdit{{vtabTok.Start, vtabTok.End, quoteIdent(newName)}})
	vm.name = newName
	for i, t := range shadows {
		db.renameShadowTable(t, shadowToks[i], newShadowNames[i])
	}
	// See renameTableTo's identical trio of comments: FK is gated on this
	// connection's own "PRAGMA foreign_keys", the trigger cascade narrows to
	// its ON-clause token only, and the view rewrite is skipped outright.
	if !legacy || db.ForeignKeys() {
		db.renameForeignKeyReferences(standIn, oldName, newName)
	}

	if legacy {
		db.renameTriggerOnClauseLegacy(standIn, oldName, newName)
	} else if terr := db.renameTriggerReferences(standIn, oldName, newName); terr != nil {
		rollback()
		return terr
	}
	if !legacy {
		if terr := applyViewRename(affectedViews, oldName, identRepl{name: newName, force: true, isTable: true}, "ALTER TABLE RENAME TO", "", nil); terr != nil {
			rollback()
			return terr
		}
	}

	db.bumpSchema(vm.isTemp)
	return nil
}

// renameShadowTable applies one shadow table's half of a vtab rename: its own
// name and stored CREATE TABLE name token, plus the automatic index a shadow
// table's PRIMARY KEY(level, idx) leaves behind. It is renameTableTo's index
// loop restricted to what a shadow table can actually carry -- no CHECK
// qualifiers, no foreign keys, no expression/partial index, and (by the
// caller's own up-front refusal) no view or trigger naming it.
func (db *DB) renameShadowTable(tbl *tableMeta, nameTok token, newName string) {
	oldName := tbl.name
	tbl.sql = applyEdits(tbl.sql, []textEdit{{nameTok.Start, nameTok.End, quoteIdent(newName)}})
	tbl.name = newName
	for _, ix := range db.indexes {
		if ix.isTemp != tbl.isTemp || !equalFoldName(ix.table, oldName) {
			continue
		}
		ix.table = newName
		if ix.sql != "" {
			if onTok, terr := locateIndexOnTableToken(ix.sql); terr == nil {
				ix.sql = applyEdits(ix.sql, []textEdit{{onTok.Start, onTok.End, quoteIdent(newName)}})
			}
			continue
		}
		// An AUTOMATIC index carries the table's name in its OWN name -- real
		// SQLite reports sqlite_autoindex_xyz_segdir_1 after the rename, not
		// sqlite_autoindex_fts_segdir_1 (verified).
		if suffix, ok := autoIndexNameSuffix(ix.name, oldName); ok {
			renamed := "sqlite_autoindex_" + newName + suffix
			ix.name = renamed
		}
	}
}
