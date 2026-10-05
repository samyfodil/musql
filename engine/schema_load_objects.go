// This file loads database schema from sqlite_schema rows into live objects
// (tables, indexes, views, triggers, virtual tables). It is called on the
// first schema read and on mid-session RELOAD after schema and live objects diverge.
// The two callers differ in one parameter: eagerAll forces all table rows to be
// read now (reload), while OpenWrite leaves ordinary tables lazy.
package engine

import "fmt"

// loadSchemaObjects registers every object schemaRows describes into db,
// reading each table's rows through rp. db.tables/indexes/views/triggers/
// vtabs must be empty on entry; on error db is left partially filled and the
// caller must discard or restore it (OpenWrite discards the whole *DB;
// reloadSchemaFromSegmentCatalog restores the saved lists).
//
// See this file's package doc comment for what what/eagerAll are for.
func (db *DB) loadSchemaObjects(rp *ReadOnlyPager, schemaRows []SchemaRow, what string, eagerAll bool) error {
	// First pass: register every table, and re-derive its automatic indexes
	// (if any) straight from its own CREATE TABLE text (see OpenWrite's doc
	// comment above) -- so the second pass, recovering explicit indexes,
	// can resolve each one's target table by name regardless of the schema
	// rows' relative order, and db.indexes already carries every automatic
	// index before that second pass cross-checks the file's own sql=NULL
	// index-row count against it.
	wantAutoIdx := map[string]int{}           // table name -> sql=NULL index rows seen in schemaRows (second pass fills this in)
	gotAutoIdx := map[string]int{}            // table name -> auto-indexes this pass actually re-derived
	autoIdxOrder := map[string][]*indexMeta{} // table name -> its non-isTablePK automatic indexMeta pointers, in schema-row order (see the loop below that fills it in)
	// A recovered object's CREATION rank is its ROW POSITION in the catalog --
	// that IS the order the objects were created in, since C SQLite (and now
	// this writer) appends each new row -- so both passes carry the index and
	// stamp schemaSeq from it. Taking a fresh rank per registration instead
	// would renumber views/triggers/indexes after every table and rebuild the
	// grouped-by-kind order this exists to remove. db.schemaSeq is lifted past
	// the last row afterwards so a NEW object created in this session sorts
	// after everything the file already held.
	db.schemaSeq = uint64(len(schemaRows))
	for i, sr := range schemaRows {
		if sr.Type != "table" {
			continue
		}
		rowSeq := schemaRowSeq(sr, i)
		// A virtual table's sqlite_schema row also has type "table" (rootpage 0,
		// verbatim CREATE VIRTUAL TABLE text). It has no ordinary column schema
		// or b-tree to recover -- register its vtabMeta (validating the module
		// still exists / accepts its args) and skip the table-recovery below.
		if isCreateVirtualTableSQL(sr.SQL) {
			name, module, args, _, perr := parseCreateVirtualTableStmt(sr.SQL)
			if perr != nil {
				return fmt.Errorf("engine: "+what+": virtual table %s: %w", sr.Name, perr)
			}
			mod, ok := lookupVtabModule(module)
			if !ok {
				return fmt.Errorf("engine: "+what+": no such module: %s", module)
			}
			// sr.Temp is already correctly classified (markTempSchemaRows,
			// via isTempCreateSQL on sr.SQL's own on-disk TEMP marker --
			// vtab.go's CreateVirtualTable writes it exactly like an ordinary
			// TABLE/VIEW/TRIGGER does): carry it onto the recovered vtabMeta
			// too, mirroring the automatic index's identical "ix.isTemp =
			// sr.Temp" a few lines below for a table. Without this a session
			// recovered from disk would forget which vtabs are TEMP, and
			// DropTempObjects (the driver Conn.Close path that makes a
			// TEMP object live no longer than its connection) would silently
			// drop none of them.
			vm := &vtabMeta{schemaSeq: rowSeq, name: name, sql: sr.SQL, module: module, args: args, isTemp: sr.Temp}
			// A WRITABLE module (rtree): rebuild the backing store and reload its
			// rows from its shadow tables. A read-only module leaves store nil.
			if wm, isWritable := mod.(writableVtabModule); isWritable {
				st, serr := wm.newWritableStore(name, args)
				if serr != nil {
					// Module instance creation deferred to first use, so a
					// refused argument leaves the row loaded with a loadErr.
					vm.loadErr = serr
					db.vtabs = append(db.vtabs, vm)
					continue
				}
				if fst, isFts5 := st.(*fts5Store); isFts5 {
					// fts5 stores rows in the %_content shadow table, not a rootpage.
					if rerr := fts5LoadStore(rp, fst, sr.Name); rerr != nil {
						vm.loadErr = fmt.Errorf("engine: "+what+": reading rows of fts5 table %s: %w", sr.Name, rerr)
					}
				} else if rst, isRtree := st.(*rtreeStore); isRtree {
					// rtree stores rows in %_node, not under a rootpage.
					if rerr := rtreeLoadStore(rp, rst, sr.Name); rerr != nil {
						vm.loadErr = fmt.Errorf("engine: "+what+": reading rows of rtree table %s: %w", sr.Name, rerr)
					}
				} else if sr.RootPage != 0 {
					vm.rootPage = sr.RootPage
					rowids, records, rerr := rp.Rows(sr.Name)
					if rerr != nil {
						return fmt.Errorf("engine: "+what+": reading rows of virtual table %s: %w", sr.Name, rerr)
					}
					for i, rid := range rowids {
						st.vtabLoadRow(int64(rid), records[i])
					}
				}
				vm.store = st
			}
			db.vtabs = append(db.vtabs, vm)
			continue
		}
		withoutRowid, strict, err := parseTableTailClauses(sr.SQL)
		if err != nil {
			return fmt.Errorf("engine: "+what+": table %s: %w", sr.Name, err)
		}
		cols, checks, specs, err := parseCreateTableColumnsAndAutoIndexes(sr.SQL, withoutRowid)
		if err != nil {
			return fmt.Errorf("engine: "+what+": table %s: %w", sr.Name, err)
		}
		checks, err = finalizeCheckConstraints(sr.Name, cols, checks, withoutRowid)
		if err != nil {
			return fmt.Errorf("engine: "+what+": table %s: %w", sr.Name, err)
		}
		ipk := -1
		for i, c := range cols {
			if c.IsRowidAlias {
				ipk = i
				break
			}
		}
		autoIdx, autoIdxBySpec, err := buildAutoIndexes(sr.Name, cols, specs)
		if err != nil {
			return fmt.Errorf("engine: "+what+": table %s: %w", sr.Name, err)
		}
		// An automatic index belongs to its table's catalog (temp_schema.go),
		// exactly like the explicit ones the second pass recovers.
		for _, ix := range autoIdx {
			ix.isTemp = sr.Temp
		}
		pkIndex, err := finalizeWithoutRowidPK(sr.Name, withoutRowid, cols, specs, autoIdxBySpec)
		if err != nil {
			return fmt.Errorf("engine: "+what+": table %s: %w", sr.Name, err)
		}
		autoIncrement, err := finalizeAutoIncrement(sr.Name, sr.SQL, withoutRowid, cols, ipk)
		if err != nil {
			return fmt.Errorf("engine: "+what+": table %s: %w", sr.Name, err)
		}

		// rows are populated HERE, eagerly, for one
		// kind of table this pass cannot afford to defer, and left
		// nil/loaded=false for every other (ordinary) table -- see
		// table_load.go's package doc comment for the full lazy-load design:
		//
		//   - sqlite_stat1 is eagerly loaded because multiple callsites read it
		//     directly and it is small (one row per table/index).
		//
		// Every other table's rows stay nil here; ensureTableLoaded reads them
		// the first time this table is actually touched by the write path.
		eagerLoad := equalFoldName(sr.Name, "sqlite_stat1")

		// rows' own map key is a rowid table's real rowid, and an opaque,
		// never-exposed identifier for a WITHOUT ROWID table (see
		// tableMeta.withoutRowid's doc comment).
		var rows map[uint64][]Value
		if eagerLoad {
			// By ROOT PAGE, not by name: a temp and a main table may share a
			// name (temp_schema.go), and rp.Rows would hand both this session's
			// tableMetas whichever schema row it matched first.
			rowids, vals, rerr := rp.RowsOfRoot(sr.RootPage)
			if rerr != nil {
				return fmt.Errorf("engine: "+what+": reading rows of %s: %w", sr.Name, rerr)
			}
			rows = make(map[uint64][]Value, len(rowids))
			for i, rowid := range rowids {
				row := vals[i]
				full, ferr := fullStoredRowOf(sr.Name, cols, ipk, rowid, row)
				if ferr != nil {
					return fmt.Errorf("engine: "+what+": %s: %w", sr.Name, ferr)
				}
				rows[rowid] = full
			}
		}

		var tblLoadErr error
		if sr.RootPage == 0 {
			// See resolveTableIn (query.go) for the C and the measurement:
			// the row is kept, every use of it is "database disk image is
			// malformed".
			tblLoadErr = fmt.Errorf("engine: database disk image is malformed")
		}
		db.tables = append(db.tables, &tableMeta{
			inFile:        !sr.Temp,
			loadErr:       tblLoadErr,
			schemaSeq:     rowSeq,
			isTemp:        sr.Temp,
			name:          sr.Name,
			sql:           sr.SQL,
			cols:          cols,
			ipkIndex:      ipk,
			rows:          newRowStore(rows, nil),
			loaded:        eagerLoad,
			checks:        checks,
			withoutRowid:  withoutRowid,
			strict:        strict,
			pkIndex:       pkIndex,
			autoIncrement: autoIncrement,
			rootPage:      sr.RootPage,
		})
		db.indexes = append(db.indexes, autoIdx...)
		// The WITHOUT ROWID isTablePK entry (exactly one of autoIdx, when
		// withoutRowid -- see finalizeWithoutRowidPK) never has its own
		// sql=NULL sqlite_schema row at all (C SQLite doesn't write one
		// either -- segmentSchemaRows skips it for exactly this reason), so it
		// must not count toward the cross-check against
		// wantAutoIdx below.
		got := len(autoIdx)
		if withoutRowid {
			got--
		}
		// ACCUMULATED, not assigned: a temp and a main table may share a name
		// (temp_schema.go), and an index row records only its tbl_name, so
		// both catalogs' automatic indexes for that name are counted -- and
		// matched by POSITION -- together. segmentSchemaRows renders them in
		// db.indexes order, which is this same db.tables order, so the
		// positional match below still lines each row up with its own table's
		// indexMeta.
		gotAutoIdx[sr.Name] += got
		// autoIdxOrder records, per table, every NON-isTablePK automatic
		// index in the same order buildAutoIndexes just produced them --
		// exactly the order their sqlite_schema rows are rendered in (one
		// loop, over db.indexes, appending this table's autoIdx verbatim) --
		// so the second pass below can
		// match each sql=NULL schema row it encounters, by POSITION, back to
		// the indexMeta it belongs to and recover its rootPage. (The
		// isTablePK entry is excluded for the same reason gotAutoIdx excludes
		// it: it never has a schema row of its own to match against.)
		for _, ix := range autoIdx {
			if ix.isTablePK {
				continue
			}
			autoIdxOrder[sr.Name] = append(autoIdxOrder[sr.Name], ix)
		}
	}

	// Second pass: recover every explicit index (CREATE INDEX-produced,
	// sql non-empty) against its now-registered table, and every view
	// (independent of any table -- see view.go's package doc comment: a
	// view's own SELECT is resolved lazily, at query time, so recovering it
	// here needs nothing but its own stored SQL text). An automatic index
	// (sql=NULL) was already re-derived above from its table's own CREATE
	// TABLE text -- it is not re-read here, only counted, so that a
	// mismatch against what the first pass actually derived (a gap in this
	// parser's constraint recognition) is caught as a clear error instead
	// of silently reopening the database with an automatic index's
	// enforcement quietly missing. Anything else -- WITHOUT ROWID shadow
	// entries -- is rejected; see OpenWrite's doc comment above.
	for i, sr := range schemaRows {
		rowSeq := schemaRowSeq(sr, i)
		switch sr.Type {
		case "table":
			continue
		case "view":
			pcv, verr := viewDefFromSchemaRow(sr, db.localSchema)
			if verr != nil {
				return fmt.Errorf("engine: "+what+": %w", verr)
			}
			db.views = append(db.views, &viewMeta{
				schemaSeq:  rowSeq,
				isTemp:     sr.Temp,
				name:       sr.Name,
				sql:        sr.SQL,
				selectStmt: pcv.selectStmt,
				colNames:   pcv.colNames,
			})
		case "trigger":
			pct, terr := parseCreateTriggerStmt(sr.SQL)
			if terr != nil {
				return fmt.Errorf("engine: "+what+": trigger %s: %w", sr.Name, terr)
			}
			// tableIsTemp: the catalog of the table this trigger fires on,
			// which is NOT its own (a TEMP trigger may target a main table).
			// The stored text's own "main."/"temp." qualifier decides it when
			// there is one; otherwise it is re-resolved exactly the way
			// CreateTrigger resolved it -- main-only for a main trigger,
			// temp-first for a temp one. See triggerMeta.tableIsTemp.
			// The NAME's own qualifier does not survive into the stored text
			// (normalizeSchemaSQL drops it, exactly as C SQLite does), so
			// this stands in for it: a MAIN trigger targets main, a TEMP one
			// resolves temp-first. An explicit ON-clause qualifier, which the
			// stored text DOES keep, still wins.
			nameScope := scopeMain
			if sr.Temp {
				nameScope = scopeTemp
			}
			targetScope := triggerTargetScope(nameScope, pct.tableScope)
			trTableIsTemp := targetScope == scopeTemp
			var trTableAttachName string
			if pct.tableSchemaText != "" {
				// The stored ON-clause names a database this session doesn't
				// recognize as main/temp -- i.e. it was accepted by
				// CreateTrigger's own resolveCreateTriggerAttachedTarget,
				// which only ever accepts this for a TEMP trigger (real
				// SQLite: attach.c:498-501, "trigger %s cannot reference
				// objects in database %s" for anything else). db.attached is
				// always empty this early -- OpenWrite runs before any ATTACH
				// statement in a fresh session could have -- so there is
				// nothing to validate against yet; trust the stored text and
				// defer, exactly like tkt3810.test's own "resolved at
				// statement compile time, not trigger installation time"
				// case just below. A LATER ATTACH under this same name, and
				// only that, makes it live again (matchingTriggers's own
				// tableAttachName guard re-resolves it fresh every time).
				if !sr.Temp {
					return fmt.Errorf("engine: "+what+": trigger %s: trigger %s cannot reference objects in database %s", sr.Name, pct.name, pct.tableSchemaText)
				}
				trTableAttachName = pct.tableSchemaText
			} else if tt := db.findTableMetaIn(targetScope, pct.table); tt != nil {
				trTableIsTemp = tt.isTemp
			} else if vv := db.findViewMetaIn(targetScope, pct.table); vv != nil {
				trTableIsTemp = vv.isTemp
			}
			db.triggers = append(db.triggers, &triggerMeta{
				schemaSeq:       rowSeq,
				isTemp:          sr.Temp,
				tableIsTemp:     trTableIsTemp,
				tableAttachName: trTableAttachName,
				name:            pct.name,
				sql:             sr.SQL,
				table:           pct.table,
				timing:          pct.timing,
				event:           pct.event,
				insteadOf:       pct.insteadOf,
				updateCols:      pct.updateCols,
				when:            pct.when,
				body:            pct.body,
			})
		case "index":
			if sr.SQL == "" {
				wantAutoIdx[sr.TblName]++
				// Recover this automatic index's rootPage by position (see
				// autoIdxOrder's own doc comment above for why this
				// ordering can be relied on). Silently ignored if this is
				// an extra row beyond what the first pass derived -- the
				// cross-check below already turns THAT mismatch into a
				// clear error.
				if list := autoIdxOrder[sr.TblName]; len(list) >= wantAutoIdx[sr.TblName] {
					list[wantAutoIdx[sr.TblName]-1].schemaSeq = rowSeq
				}
				continue
			}
			stmt, err := parseCreateIndexStmt(sr.SQL)
			if err != nil {
				return fmt.Errorf("engine: "+what+": index %s: %w", sr.Name, err)
			}
			// An index always lives in the SAME catalog as its table (unlike a
			// trigger, which can be TEMP over a MAIN table -- see the trigger
			// case above), so sr.Temp names the catalog to resolve tbl_name in.
			// Resolving it scope-blind searched TEMP FIRST, which bound a MAIN
			// index to a temp table that merely SHADOWS its name: after
			// "CREATE TABLE early(a); CREATE INDEX early_i ON early(a);
			// CREATE TEMP TABLE early(b)" the very next reopen failed outright
			// with "index early_i: table early has no column named a".
			// The scope-blind lookup stays as a fallback so a catalog whose
			// temp classification is unavailable still opens exactly as before.
			idxScope := scopeMain
			if sr.Temp {
				idxScope = scopeTemp
			}
			tbl := db.findTableMetaIn(idxScope, sr.TblName)
			if tbl == nil {
				tbl = db.findTableMetaIn(idxScope, stmt.table)
			}
			if tbl == nil {
				tbl = db.findTableMeta(sr.TblName)
			}
			if tbl == nil {
				tbl = db.findTableMeta(stmt.table)
			}
			if tbl == nil {
				return fmt.Errorf("engine: "+what+": index %s refers to unknown table %s", sr.Name, sr.TblName)
			}
			// The SAME double-quoted-string demotion CreateIndex applies, and
			// shared for the same reason resolvePlainIndexColumns is: an index
			// whose key is a "..."-spelled non-column was CREATED as an
			// expression index, so it has to be RECOVERED as one.
			demoteDoubleQuotedIndexColumns(stmt, tbl)
			if stmt.exprOrPartial {
				// An expression/partial index: rebuild exactly the indexMeta
				// createExprIndex would have -- its ordered key list plus the
				// referenced-column SET cols/colIdx carry for it (see
				// indexMeta.exprOrPartial). Reusing createExprIndex's own
				// helpers here is what keeps the reopened index byte-identical
				// to the one this session created.
				im, ierr := buildExprIndexMeta(sr.Name, tbl, stmt, sr.SQL)
				if ierr != nil {
					return fmt.Errorf("engine: "+what+": index %s: %w", sr.Name, ierr)
				}
				im.isTemp, im.schemaSeq = sr.Temp, rowSeq
				db.indexes = append(db.indexes, im)
				continue
			}
			// The SAME derivation CreateIndex uses, so a recovered index's key
			// ORDER (collations, and colDesc just below) is the order it was
			// actually written in -- getting either wrong makes the next
			// rebuild lay the b-tree out differently from how C SQLite
			// reads it back. See resolvePlainIndexColumns.
			colIdx, colColl, rerr := resolvePlainIndexColumns(tbl, stmt)
			if rerr != nil {
				return fmt.Errorf("engine: "+what+": index %s: %w", sr.Name, rerr)
			}
			db.indexes = append(db.indexes, &indexMeta{
				schemaSeq:    rowSeq,
				isTemp:       sr.Temp,
				name:         sr.Name,
				table:        tbl.name,
				cols:         stmt.cols,
				colIdx:       colIdx,
				colCollation: colColl,
				colDesc:      append([]bool(nil), stmt.desc...),
				unique:       stmt.unique,
				sql:          sr.SQL,
			})
		default:
			return fmt.Errorf("engine: "+what+": schema object %s %q is not supported by this write path (only ordinary tables and explicit indexes are)", sr.Type, sr.Name)
		}
	}

	// Cross-check: every table's re-derived automatic-index count must match
	// how many sql=NULL index rows the file itself actually has for that
	// table. A mismatch means this parser failed to recognize some
	// UNIQUE/PRIMARY KEY constraint the file's CREATE TABLE text encodes --
	// silently proceeding would reopen the database with that constraint's
	// enforcement quietly gone, exactly the write-fidelity gap this whole
	// mechanism exists to close.
	for tblName, want := range wantAutoIdx {
		if got := gotAutoIdx[tblName]; got != want {
			return fmt.Errorf("engine: "+what+": table %s: sqlite_schema has %d automatic index row(s) but this writer only re-derived %d from its CREATE TABLE text -- refusing to silently drop constraint enforcement", tblName, want, got)
		}
	}

	// Virtual tables with shadow tableMetas (fts3/4/5) are not gated by
	// ensureTableLoaded. If any virtual table is present, load all remaining
	// tables eagerly to ensure shadow tables are available.
	if eagerAll || len(db.vtabs) > 0 {
		for _, tbl := range db.tables {
			if err := db.loadTableRowsFrom(rp, tbl, eagerAll); err != nil {
				return fmt.Errorf("engine: "+what+": %w", err)
			}
		}
	}

	return nil
}

// loadTableRowsFrom fills one table's row store. force reads it through rp
// unconditionally, which is what a mid-session reload needs (see this file's
// package doc comment: ensureTableLoaded reopens db.path, whose bytes a
// reload's in-memory image has already moved past). Otherwise it is exactly
// the lazy gate every other write-path choke point uses.
func (db *DB) loadTableRowsFrom(rp *ReadOnlyPager, tbl *tableMeta, force bool) error {
	if !force {
		return db.ensureTableLoaded(tbl)
	}
	if tbl.loaded {
		return nil
	}
	rows, rowidsSorted, err := readTableRowsFromPager(rp, tbl)
	if err != nil {
		return fmt.Errorf("engine: loading table %s: %w", tbl.name, err)
	}
	tbl.rows = newRowStore(rows, rowidsSorted)
	tbl.loaded = true
	return nil
}

// schemaRowSeq is a loaded object's schemaSeq: its sqlite_schema rowid when the
// catalog recorded one -- the number the next CREATE counts past, see
// DB.nextSchemaSeq -- and its row position where it did not.
func schemaRowSeq(sr SchemaRow, i int) uint64 {
	if sr.Rowid > 0 {
		return uint64(sr.Rowid)
	}
	return uint64(i + 1)
}
