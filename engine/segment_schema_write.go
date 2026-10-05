package engine

import "fmt"

// WRITING sqlite_schema DIRECTLY, on the segment format.
//
// "PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=...; PRAGMA
// writable_schema=RESET" is how SQLite lets an application edit the catalog
// itself, and the corpus uses it heavily -- a rewritten column list, an index
// definition changed under a live table, a catalog row simply deleted, and every
// corruption-recovery test in SQLite's own suite. It was DECLINED here, which is
// honest but not compatible: the interchange promise is about what a database can
// be asked to do, not only about the bytes.
//
// The statement records an OVERLAY on the catalog (db.wsEdits, see
// schema_write_direct.go), reads see the overlay, and RESET performs a
// WHOLE-SCHEMA RELOAD from the edited rows -- prepare.c's sqlite3InitOne, which
// is what pragma.c:1182 reaches for.
//
//   - THE CATALOG'S NATURAL ROWS are rendered from the live schema
//     (segmentSchemaRows), and each table's "root page" is its synthetic one --
//     the number that maps it to its segments (SegmentFile.RootOf).
//   - THE RELOAD re-parses the edited rows and carries each surviving table's
//     ROWS across by that same synthetic root. See
//     reloadSchemaFromSegmentCatalog.
//
// What still declines, with the reason, because it is a capability this format
// does not have rather than work not done:
//
//   - AN EDIT TO rootpage. A root page number is how the SQLite format names
//     storage; here storage is named by the directory entry a table's rows are
//     written under, and "point object A at object B's storage" has no spelling.
//     The READ of that column declines for the same reason
//     (segmentCatalogColumnDecline, query.go).
//   - A "WHERE rowid=<n>" over the catalog. The rowid a segment catalog can
//     offer is its position in the directory; C's is its schema b-tree key, gaps
//     and all, so matching on it would edit a DIFFERENT row than C does.

// segmentNaturalCatalog is db.wsNatural for a segment-backed session: the
// catalog's PRE-overlay rows, rendered from the live schema.
//
// The rowid is the row's 1-based position. It is what pairs a scanned row with
// its overlay entry (schemaWriteState.keyOf, vdbe_schema_write.go) and never
// reaches the file.
func (db *DB) segmentNaturalCatalog() []catalogRow {
	rows := db.segmentSchemaRows()
	out := make([]catalogRow, 0, len(rows))
	var main []SchemaRow
	for _, r := range rows {
		if !r.Temp {
			main = append(main, r)
		}
	}
	// MAIN's rows only: TEMP has its own catalog and numbering, and the overlay
	// skips its rows (wsCatalogWithOverlay).
	own := schemaRowidsUsable(main)
	// The PUBLIC rootpage, the number a reader of sqlite_schema sees -- not the
	// internal routing key (SegmentFile.RootOf) the rows carry. A catalog write
	// evaluates this column ("UPDATE ... WHERE rootpage = 3", "DELETE ... WHERE
	// rootpage > 2") and matched nothing, or everything, against routing keys
	// (TestPagesR25RootpageReadMatchesCSQLite). The reload maps it back
	// (segmentPublicToRouting).
	// MAIN's rows only: TEMP is its own SQLite-format file (temp_store.go), whose
	// rootpages are real page numbers and stay as they are.
	mainRoots := catalogRootpages(main)
	mi := 0
	for i, r := range rows {
		rowid := int64(i + 1)
		if own && !r.Temp {
			rowid = r.Rowid
		}
		root := int64(r.RootPage)
		if !r.Temp {
			root = int64(mainRoots[mi])
			mi++
		}
		out = append(out, catalogRow{
			key:   wsCatalogKey{name: r.Name, temp: r.Temp},
			rowid: rowid,
			vals: []Value{
				{Typ: Text, S: []byte(r.Type)},
				{Typ: Text, S: []byte(r.Name)},
				{Typ: Text, S: []byte(r.TblName)},
				{Typ: Int, I: root},
				sqlValueForSchemaRow(r.SQL),
			},
		})
	}
	return out
}

// wsEnsureNatural fills db.wsNatural for a segment-backed session.
//
// Nothing fills it as a side effect, so every reader of it has to ask -- and an INSERT is the shape
// that proves it: it has no scan, so it never reaches wsCurrentCatalog, and the
// reload then saw an overlay whose natural rows were EMPTY and rebuilt the schema
// out of the inserted row alone. "INSERT INTO sqlite_schema(...) VALUES('table',
// 'x','x',2,'CREATE TABLE x(a)')" followed by RESET dropped t1 from the database.
//
// Refreshed rather than memoized, because the natural rows are the LIVE schema's
// and an edit does not change that until the reload: wsEdits is the overlay ON
// this list, so re-rendering it is what keeps the two in step.
func (db *DB) wsEnsureNatural() {
	db.wsNatural = db.segmentNaturalCatalog()
}

// rootEditKind is what a rootpage assignment on an existing catalog row means
// once the reload reaches it. See segmentRootpageEditPlan.
type rootEditKind int

const (
	rootEditDecline rootEditKind = iota
	rootEditNoop
	rootEditAlias
	rootEditDuplicateSibling
)

// segmentRootpageEditPlan classifies a rootpage assignment on key's row by the
// number it assigns, which names storage the way C's page number does:
//
//   - the row's own number: a no-op.
//   - another ordinary TABLE's number, both rowid tables with the same column
//     count, INTEGER PRIMARY KEY position and column affinities, and no index on
//     either: an ALIAS -- the two names read and write one row store, as two
//     catalog rows naming one b-tree do in C. Those conditions are what make one
//     stored row mean the same thing under both names; an index would not follow
//     the alias (C's keeps the entries of the table it was built on, which this
//     format derives rather than stores).
//   - a SIBLING index's number (another index on the same table): C's load fails
//     "invalid rootpage" (prepare.c:62-68, :181; build.c:4393).
//
// Anything else -- a number that is no object's root (an interior page, or past
// C's end of file: which of the two depends on C's page layout), or an alias
// involving an index -- declines.
func (db *DB) segmentRootpageEditPlan(key wsCatalogKey, v *Value) rootEditKind {
	if v == nil || v.Typ != Int {
		return rootEditDecline
	}
	cur, ok := db.wsNaturalRootpage(key)
	if !ok {
		return rootEditDecline
	}
	if v.I == cur {
		return rootEditNoop
	}
	target, ok := db.segmentRowForPublicRoot(v.I)
	if !ok {
		return rootEditDecline
	}
	self, ok := db.segmentRowForPublicRoot(cur)
	if !ok {
		return rootEditDecline
	}
	switch {
	case self.Type == "index" && target.Type == "index" && equalFoldName(self.TblName, target.TblName):
		return rootEditDuplicateSibling
	case self.Type == "table" && target.Type == "table":
		a, b := db.findTableMeta(self.Name), db.findTableMeta(target.Name)
		if a == nil || b == nil || a.isTemp || b.isTemp || a.withoutRowid || b.withoutRowid ||
			len(a.cols) != len(b.cols) || a.ipkIndex != b.ipkIndex ||
			db.tableHasAnyIndex(a.name) || db.tableHasAnyIndex(b.name) {
			return rootEditDecline
		}
		// ...and every column's AFFINITY the same, with no generated column. C
		// stores a value in the form its WRITER's affinity left it -- an integral
		// REAL as an integer record, realified again only by a REAL column's read
		// (OP_RealAffinity, vdbe.c:2134) -- and this format keeps the logical
		// value. Two tables that agree column for column read one stored row
		// identically, which is what makes sharing the store exact; strict2's
		// STRICT t1 read through an untyped t2 is the shape this leaves declined.
		for i := range a.cols {
			if a.cols[i].Aff != b.cols[i].Aff || a.cols[i].IsGenerated() || b.cols[i].IsGenerated() {
				return rootEditDecline
			}
		}
		return rootEditAlias
	}
	return rootEditDecline
}

// errSegmentRootpageNotReproducible is the RESET a rootpage edit declines.
var errSegmentRootpageNotReproducible = fmt.Errorf("%w: PRAGMA writable_schema=RESET over a rootpage assignment that is not reproducible against C SQLite on this format -- the number names no object's storage here (C reads an interior page or fails the load past its end of file, which depends on C's page layout), or it aliases storage an index is built on (C's index keeps the entries of the table it was built on; this format derives indexes rather than storing them)", errVDBEUnsupported)

// segmentRootpageEditDeclined reports whether an outstanding rootpage
// assignment is one segmentRootpageEditPlan declines.
//
// ...and the same displacement spelled as a NAME: an index row renamed onto a
// sibling index's name makes C's reload bind that sibling to this row's b-tree
// and leave the sibling's own with no row (prepare.c:195-203's autoindex arm,
// sqlite3FindIndex by name) -- an index reading storage it was not built on
// (fkey1.test 8.2).
func (db *DB) segmentRootpageEditDeclined() bool {
	for key, e := range db.wsEdits {
		if key.ins != 0 {
			continue
		}
		if e.set[3] != nil && db.segmentRootpageEditPlan(key, e.set[3]) == rootEditDecline {
			return true
		}
		if e.set[1] != nil && e.set[1].Typ == Text && db.segmentIndexRenamedOntoSibling(key.name, string(e.set[1].S)) {
			return true
		}
	}
	return false
}

// segmentIndexRenamedOntoSibling reports whether renaming the index row named
// from to the name to lands it on ANOTHER index of the same table.
func (db *DB) segmentIndexRenamedOntoSibling(from, to string) bool {
	if equalFoldName(from, to) {
		return false
	}
	var self, other *indexMeta
	for _, ix := range db.indexes {
		if ix.isTemp {
			continue
		}
		if equalFoldName(ix.name, from) {
			self = ix
		}
		if equalFoldName(ix.name, to) {
			other = ix
		}
	}
	return self != nil && other != nil && equalFoldName(self.table, other.table)
}

// replayRootEdits applies a file's ConvertedCatalog.RootEdits at open, through
// the very reload a RESET runs: C's next schema load reads the numbers the
// catalog holds, so the load here resolves them the same way. A shape the RESET
// would decline fails the OPEN with that decline -- every statement on the
// connection then refuses, where C's failed load answers from a schema this
// format cannot reproduce; a duplicate sibling root latches C's own "invalid
// rootpage" (the reload does it).
func (db *DB) replayRootEdits(edits []ConvertedRootEdit) error {
	for _, re := range edits {
		if re.Root < 0 {
			return errSegmentRootpageNotReproducible
		}
		v := Value{Typ: Int, I: re.Root}
		db.wsEditFor(wsCatalogKey{name: re.Name}).set[3] = &v
	}
	if db.reloadSchemaFromSegmentCatalog(true) {
		return nil
	}
	if db.segmentRootpageEditDeclined() {
		return errSegmentRootpageNotReproducible
	}
	return fmt.Errorf("%w: the catalog's rootpage assignments could not be loaded", errVDBEUnsupported)
}

// segmentPublicToRouting maps the edited catalog rows' rootpage column, which
// holds PUBLIC numbers (segmentNaturalCatalog), back to what the loader binds by:
// a table's routing root (SegmentFile.RootOf), and 0 for an index, whose storage this
// format derives. An alias carries its target's public number and so lands on the
// target's routing root. A number no live table renders is left as it is; the
// plan has already declined those (segmentRootpageEditPlan).
func (db *DB) segmentPublicToRouting(rows []SchemaRow) []SchemaRow {
	if rows == nil {
		return nil
	}
	natural := db.segmentSchemaRows()
	routing := make(map[uint32]uint32, len(natural))
	for i, root := range catalogRootpages(natural) {
		if root != 0 && natural[i].Type == "table" && natural[i].AliasOf == "" && !natural[i].Temp {
			routing[root] = natural[i].RootPage
		}
	}
	for i := range rows {
		switch {
		case rows[i].Temp:
		case rows[i].Type == "index":
			rows[i].RootPage = 0
		default:
			if r, ok := routing[rows[i].RootPage]; ok {
				rows[i].RootPage = r
			}
		}
	}
	return rows
}

// segmentRowForPublicRoot is the live catalog row whose RENDERED rootpage is n.
func (db *DB) segmentRowForPublicRoot(n int64) (SchemaRow, bool) {
	rows := db.segmentSchemaRows()
	for i, root := range catalogRootpages(rows) {
		if root != 0 && int64(root) == n && !rows[i].Temp {
			return rows[i], true
		}
	}
	return SchemaRow{}, false
}

// segmentDuplicateRootLater names the later, in catalog order, of key's row and
// the sibling whose root it was given -- the one C's load reaches second.
func (db *DB) segmentDuplicateRootLater(key wsCatalogKey, n int64) string {
	rows := db.segmentSchemaRows()
	selfAt, targetAt := -1, -1
	target, _ := db.segmentRowForPublicRoot(n)
	for i, r := range rows {
		if r.Temp {
			continue
		}
		if r.Name == key.name {
			selfAt = i
		}
		if r.Name == target.Name {
			targetAt = i
		}
	}
	if targetAt > selfAt {
		return target.Name
	}
	return key.name
}

// wsNaturalRootpage is the rootpage a reader of sqlite_schema sees for key's row
// -- the RENDERED number (catalogRootpages, segment_open.go), not the routing
// key (SegmentFile.RootOf) the catalog rows carry internally.
func (db *DB) wsNaturalRootpage(key wsCatalogKey) (int64, bool) {
	if key.ins != 0 {
		return 0, false
	}
	rows := db.segmentSchemaRows()
	roots := catalogRootpages(rows)
	for i, r := range rows {
		if r.Temp == key.temp && r.Name == key.name {
			return int64(roots[i]), true
		}
	}
	return 0, false
}

// reloadSchemaFromSegmentCatalog is "PRAGMA writable_schema=RESET"'s reload
// (schema_reload_image.go): re-derive every object from the EDITED catalog rows,
// which is prepare.c's sqlite3InitOne as pragma.c:1182 reaches it.
//
// It reports whether it ran; false leaves every piece of state exactly as it was,
// so the RESET ladder can fall through to its own decline.
//
// There is no page-1 image to materialize (the rows come from the overlay
// directly) and no b-tree to re-bind (a table's storage is its directory entry,
// which the synthetic root page names). What IS load-bearing is the last step -- carrying each surviving table's ROWS across by that root --
// because those rows are this session's only copy of anything it has not
// committed, and the re-derived tableMeta starts empty.
func (db *DB) reloadSchemaFromSegmentCatalog(cookieMoved bool) bool {
	if !db.writableSchemaEditsActive() {
		return false
	}
	for key, e := range db.wsEdits {
		if key.ins == 0 {
			// A rootpage ASSIGNMENT on an existing row has no meaning here
			// (refused at compile time); this is the belt for a route that
			// reaches the overlay some other way.
			if e.set[3] != nil {
				switch db.segmentRootpageEditPlan(key, e.set[3]) {
				case rootEditNoop:
					// DROPPED, not applied: the value is the rendered number, while the
					// rows below are carried across by the internal routing root, which
					// applying it would overwrite -- the table reloaded EMPTY
					// (TestWritableSchemaResetReloadAdversarial_RootpageSetToSameValue...).
					e.set[3] = nil
				case rootEditAlias:
					// Carried to the TARGET's routing root below, so this table binds to
					// the very row store the target has: one storage, two names, as C's
					// two catalog rows naming one b-tree.
					target, _ := db.segmentRowForPublicRoot(e.set[3].I)
					// The owner's rows IN HAND first: the carry-over below shares a
					// LOADED store, and a lazily loaded alias would read the owner's
					// segments into a second store of its own -- one storage, two
					// copies, a write through either invisible through the other.
					if o := db.findTableMeta(target.Name); o == nil || db.ensureTableLoaded(o) != nil {
						return false
					}
					// set[3] keeps the target's PUBLIC number; segmentPublicToRouting
					// below maps it to the target's routing root, binding the two.
				case rootEditDuplicateSibling:
					// C's load fails naming the LATER of the two rows (the earlier one's
					// tnum is already set when the later is reached -- prepare.c:181 with
					// sqlite3IndexHasDuplicateRootPage, prepare.c:62-68), and the RESET
					// itself succeeds; every statement after it answers "malformed
					// database schema (<later>) - invalid rootpage" until writable_schema
					// goes ON again.
					db.latchSchemaCorrupt(db.segmentDuplicateRootLater(key, e.set[3].I), "invalid rootpage")
					return true
				default:
					return false
				}
			}
			// An index row renamed onto a sibling index's name binds that sibling
			// to this row's b-tree in C's reload (prepare.c:195-203) -- an index
			// reading storage it was not built on, which this format cannot
			// reproduce. Refused here, so the caller's segmentRootpageEditDeclined
			// declines it, rather than reloaded into a schema C does not have.
			if e.set[1] != nil && e.set[1].Typ == Text && db.segmentIndexRenamedOntoSibling(key.name, string(e.set[1].S)) {
				return false
			}
			continue
		}
		// AN INSERTED ROW carries all five columns, so its rootpage is not an
		// assignment -- it is part of the new object. One that NAMES STORAGE is
		// the case this format cannot reproduce: C reads the b-tree at that page,
		// so "INSERT INTO sqlite_schema VALUES('table','x','x',2,'CREATE TABLE
		// x(a)')" makes x an ALIAS of whatever lives at page 2 (t1, in the
		// corpus's own shape, so "SELECT count(*) FROM x" answers 1 there). A
		// segment file names storage by the directory entry rows are written
		// under; there is no page 2 to point at. A row for an object with no
		// storage of its own -- a view or a trigger -- is served.
		if wsInsertedRowNamesStorage(e) {
			return false
		}
	}
	// A reload inside an open transaction is declined: C's ROLLBACK leaves the reverted rows under
	// the RELOADED schema, and this engine's ROLLBACK restores both together --
	// unless the schema cookie moved ("PRAGMA schema_version=N", vdbe.c:4262),
	// when C's ROLLBACK reloads too and the two agree (altertab.test 22.0).
	if db.inTransaction() && !cookieMoved {
		return false
	}
	db.wsEnsureNatural()
	rows := db.segmentPublicToRouting(db.writableSchemaEditedRows())
	if rows == nil || !reloadCatalogTextAgreesWithRow(rows) {
		// ...and a row whose sql text names another object, or another kind,
		// than its own name/type columns: C's reload binds by the text.
		return false
	}
	// sqlite3InitCallback refuses a row it cannot parse by failing the WHOLE
	// load (prepare.c:145-200's corruptSchema), never by skipping it, and the
	// caller has already latched that case for an INSERTed row
	// (wsInsertedRowCorrupt). Here the same rule means: if loadSchemaObjects
	// refuses these rows, this reload did not happen.
	p, perr := db.segmentReadPager()
	if perr != nil {
		return false
	}
	p.schemaRows, p.schemaLoaded = rows, true

	// Everything the loader appends to, saved so a refusal leaves the session on
	// exactly the schema it had (a return between the clear and the restore left
	// a session with no schema at all).
	savedTables, savedIndexes := db.tables, db.indexes
	savedViews, savedTriggers, savedVtabs := db.views, db.triggers, db.vtabs
	savedSeq := db.schemaSeq
	db.tables, db.indexes, db.views, db.triggers, db.vtabs = nil, nil, nil, nil, nil
	// ...and an AUTOMATIC index the edit introduced is refused exactly as the
	// image path refuses it (reloadAutoIndexRowsAgree): C's reload writes no
	// catalog row for one, and serving it listed an sqlite_autoindex row C does
	// not have (TestIntegrityCheckRowLevelBattery's "stays declined" case).
	if lerr := db.loadSchemaObjects(p, rows, "PRAGMA writable_schema=RESET", true); lerr != nil || !db.reloadAutoIndexRowsAgree(rows) {
		db.tables, db.indexes = savedTables, savedIndexes
		db.views, db.triggers, db.vtabs = savedViews, savedTriggers, savedVtabs
		db.schemaSeq = savedSeq
		return false
	}
	// THE ROWS COME ACROSS BY SYNTHETIC ROOT. A table whose definition changed
	// keeps the rows it had: that is what C does too, since the edit rewrites the
	// catalog row and leaves the b-tree alone, and it is what makes "rewrite the
	// column list, then SELECT" answer about the same data. A table the edit
	// DELETED is simply absent from rows above, so its store is dropped with it.
	byRoot := make(map[uint32]*tableMeta, len(savedTables))
	for _, t := range savedTables {
		byRoot[t.rootPage] = t
	}
	// Which reloaded table is an ALIAS: two now name one routing root, and the
	// one whose own original root that is, is the owner.
	owner := make(map[uint32]string, len(savedTables))
	for _, t := range savedTables {
		if t.aliasOf == "" {
			owner[t.rootPage] = t.name
		}
	}
	for _, t := range db.tables {
		if o := owner[t.rootPage]; o != "" && !equalFoldName(o, t.name) {
			t.aliasOf = o
		}
	}
	for _, t := range db.tables {
		old, ok := byRoot[t.rootPage]
		if !ok || !old.loaded {
			continue
		}
		t.rows, t.loaded = old.rows, true
		t.rowsMutatedThisSession = old.rowsMutatedThisSession
		t.rowsWrittenSinceCommit = old.rowsWrittenSinceCommit
	}
	db.wsEdits = nil
	db.noteReloadedFromImage()
	return true
}

// wsInsertedRowNamesStorage reports whether an INSERTED catalog row describes an
// object whose storage is a b-tree ROOT PAGE: a table or an index with a non-zero
// rootpage. See reloadSchemaFromSegmentCatalog for why that one is refused.
//
// Anything else is served, including a row too broken to be an object at all --
// C fails the whole schema load for one (prepare.c:145-200's corruptSchema) and
// so does this engine, through wsInsertedRowCorrupt's latch, which is a shared
// rule rather than a format one.
func wsInsertedRowNamesStorage(e *wsCatalogEdit) bool {
	if e == nil || e.set[0] == nil || e.set[3] == nil {
		return false
	}
	typ := ""
	if e.set[0].Typ == Text {
		typ = string(e.set[0].S)
	}
	if !equalFoldName(typ, "table") && !equalFoldName(typ, "index") {
		return false
	}
	return e.set[3].Typ == Int && e.set[3].I != 0
}

// wsEditedRowDisagreesWithItsSQL applies sqlite3CheckObjectName (build.c:1031-1052)
// to the catalog rows this session EDITED, and reports the first one a schema load
// would refuse.
//
// The C is short and exact: while a load is running (db->init.busy), the object
// the row's SQL text creates must agree with the row's own type, name and
// tbl_name columns -- all three, case-insensitively -- or the parse fails with an
// empty message and corruptSchema supplies "malformed database schema (<name>)".
// (The same function SKIPS the check while writable_schema is ON, which is why
// the edit itself is allowed; RESET turns the flag OFF before the reload,
// pragma.c:1182, so the reload is where it bites.)
//
// So "UPDATE sqlite_schema SET type='tabl' WHERE name='t1'" and "... SET
// name='t9' ..." are not reloads that fail -- they are reloads that CORRUPT, and
// every later statement on the connection answers "malformed database schema"
// until "PRAGMA writable_schema=ON" lifts it. Verified against the oracle both
// ways: RESET itself succeeds, and the next SELECT errors.
//
// Measured before this: RESET declined and the session went on answering from the
// edited catalog -- the engine answering where C errors.
func (db *DB) wsEditedRowDisagreesWithItsSQL() (obj string, bad bool) {
	for _, r := range db.writableSchemaEditedRows() {
		sql := r.SQL
		if len(sql) < 2 || !equalFoldName(sql[:2], "cr") {
			continue // not a CREATE: prepare.c's other arms own it
		}
		typ, name, tbl, ok := wsDeclaredObjectOf(sql)
		if !ok {
			continue // unparseable: the reload itself refuses it
		}
		if !equalFoldName(typ, r.Type) || !equalFoldName(name, r.Name) || !equalFoldName(tbl, r.TblName) {
			return r.Name, true
		}
	}
	return "", false
}

// wsDeclaredObjectOf is what a CREATE statement itself says it creates: the type,
// the object's name, and the "tbl_name" column's value for it -- which is the
// parent table for an index or a trigger and the object's own name otherwise
// (build.c:2673's sqlite3EndTable and friends write it that way).
func wsDeclaredObjectOf(sql string) (typ, name, tbl string, ok bool) {
	if isCreateVirtualTableSQL(sql) {
		n, _, _, _, err := parseCreateVirtualTableStmt(sql)
		if err != nil {
			return "", "", "", false
		}
		return "table", n, n, true
	}
	if idx, err := parseCreateIndexStmt(sql); err == nil {
		return "index", idx.name, idx.table, true
	}
	if trg, err := parseCreateTriggerStmt(sql); err == nil {
		return "trigger", trg.name, trg.table, true
	}
	if v, err := parseCreateViewStmt(sql); err == nil {
		return "view", v.name, v.name, true
	}
	if n, _, _, err := parseCreateTableName(sql); err == nil {
		return "table", n, n, true
	}
	return "", "", "", false
}
