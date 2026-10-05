package engine

import (
	"path/filepath"
	"testing"
)

// colIndexOf finds name's position in a PRAGMA-style column-name list.
func colIndexOf(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}

// TestWritableSchemaUpdateWithTempObjectPresent checks that direct sqlite_schema
// updates work even when temp objects exist. An unqualified sqlite_master write
// can never touch temp rows, so the presence of temp objects should not block it.
func TestWritableSchemaUpdateWithTempObjectPresent(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "repro.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()

	stmts := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT COLLATE nocase, c INT COLLATE nocase, d TEXT)`,
		`INSERT INTO t1(a,b,c,d) VALUES (1,'one','one','one'), (2,'two','two','two'), (3,'three','three','three'), (4,'four','four','four'), (5,'five','five','five')`,
		`CREATE INDEX t1bcd ON t1(b,c,d)`,
		`CREATE TABLE t2(a INTEGER PRIMARY KEY, b TEXT COLLATE nocase, c INT COLLATE nocase, d TEXT)`,
		`INSERT INTO t2(a,b,c,d) VALUES (1,'one','one','one'), (2,'two','two','TWO'), (3,'three','THREE','three'), (4,'FOUR','four','four'), (5,'FIVE','FIVE','five')`,
		`CREATE INDEX t2bcd ON t2(b,c,d)`,
		`CREATE TEMP TABLE saved_schema AS SELECT name, rootpage FROM sqlite_schema`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM saved_schema WHERE name='t2bcd') WHERE name='t1bcd'`,
		`UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM saved_schema WHERE name='t1bcd') WHERE name='t2bcd'`,
	}
	for i, s := range stmts {
		if e := db.Exec(s); e != nil {
			t.Fatalf("stmt %d %q: %v", i, s, e)
		}
	}

	// Edit is left outstanding for the CREATEs below to exercise.

	// Create main and temp tables with same name while edit is outstanding.
	if e := db.Exec(`CREATE TABLE trial(col_main)`); e != nil {
		t.Fatalf("CREATE TABLE trial: %v", e)
	}
	if e := db.Exec(`CREATE TEMP TABLE trial(col_temp)`); e != nil {
		t.Fatalf("CREATE TEMP TABLE trial: %v", e)
	}

	// PRAGMA table_info must tell main and temp tables apart.
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	mainCols, mainRows, merr := p.QueryArgs(`PRAGMA main.table_info(trial)`, nil)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(mainRows) != 1 || valueText(mainRows[0][colIndexOf(mainCols, "name")]) != "col_main" {
		t.Fatalf("PRAGMA main.table_info(trial) = %v, want one row naming col_main", mainRows)
	}
	tempCols, tempRows, terr := p.QueryArgs(`PRAGMA temp.table_info(trial)`, nil)
	if terr != nil {
		t.Fatal(terr)
	}
	if len(tempRows) != 1 || valueText(tempRows[0][colIndexOf(tempCols, "name")]) != "col_temp" {
		t.Fatalf("PRAGMA temp.table_info(trial) = %v, want one row naming col_temp", tempRows)
	}
}

// TestWritableSchemaFilteredCatalogReadSeesEditWithTempPresent checks that
// filtered catalog reads see outstanding writable_schema edits even when
// temp objects exist.
func TestWritableSchemaFilteredCatalogReadSeesEditWithTempPresent(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "repro2.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()

	for _, s := range []string{
		`CREATE TABLE m1(a,b)`,
		`CREATE TEMP TABLE t1(z)`, // temp object exists before edit
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE TABLE m1(a,b,c)' WHERE name='m1'`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows, qerr := p.QueryArgs(`SELECT sql FROM sqlite_master WHERE name='m1'`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got, want := valueText(rows[0][0]), "CREATE TABLE m1(a,b,c)"; got != want {
		t.Fatalf("filtered sqlite_master.sql = %q, want %q (the read-after-write gap)", got, want)
	}

	// And the temp catalog's own row must be completely unaffected.
	_, tempRows, terr := p.QueryArgs(`SELECT sql FROM sqlite_temp_master WHERE name='t1'`, nil)
	if terr != nil {
		t.Fatal(terr)
	}
	if len(tempRows) != 1 || valueText(tempRows[0][0]) != "CREATE TABLE t1(z)" {
		t.Fatalf("sqlite_temp_master.sql for t1 = %v, want CREATE TABLE t1(z) unedited", tempRows)
	}
}

// TestWritableSchemaCrossCatalogNameCollisionSafe directly probes the
// adversarial-review risk this fix's own analysis flagged: a MAIN object and
// a TEMP object sharing the SAME bare name, with a writable_schema edit
// outstanding on the main one. wsCatalogKey's temp field
// (schema_write_direct.go) exists precisely so materialize (writer.go)
// cannot apply main's edit to temp's same-named row, or vice versa -- a
// mutation test (removing the temp field from the key construction in
// writer.go's writeCollectedSchemaRows) confirmed this test fails loudly
// without the fix: both rows collapse onto the SAME wsCatalogKey, so the
// edit meant for main's row is applied to temp's too, silently overwriting
// its stored sql (including the TEMP marker that keyword-strip/classification
// -- markTempSchemaRows -- depends on) and leaking it into main's filtered
// results as well.
func TestWritableSchemaCrossCatalogNameCollisionSafe(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "repro3.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()

	for _, s := range []string{
		`CREATE TABLE dup(a)`,
		// The same-named TEMP object exists BEFORE the edit -- exactly
		// pragma.test#12's own saved_schema/t1bcd shape -- so this reaches
		// materialize's per-row overlay application (writer.go) rather than
		// the separate, still-conservative "CREATE of an object whose name
		// already carries an edit" DDL guard (schema_write_direct.go), which
		// this probe is not about.
		`CREATE TEMP TABLE dup(z)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE TABLE dup(a,b,c)' WHERE name='dup'`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, mainRows, merr := p.QueryArgs(`SELECT sql FROM sqlite_master WHERE name='dup'`, nil)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(mainRows) != 1 || valueText(mainRows[0][0]) != "CREATE TABLE dup(a,b,c)" {
		t.Fatalf("sqlite_master.sql for dup = %v, want exactly one row with the edited text -- main's own row", mainRows)
	}
	_, tempRows, terr := p.QueryArgs(`SELECT sql FROM sqlite_temp_master WHERE name='dup'`, nil)
	if terr != nil {
		t.Fatal(terr)
	}
	if len(tempRows) != 1 || valueText(tempRows[0][0]) != "CREATE TABLE dup(z)" {
		t.Fatalf("sqlite_temp_master.sql for dup = %v, want its OWN unedited text -- a collision would leak main's edit here", tempRows)
	}

	// PRAGMA table_info must also see the two independently: temp's dup
	// still has its own real, uncorrupted column list.
	_, tempInfo, ierr := p.QueryArgs(`PRAGMA temp.table_info(dup)`, nil)
	if ierr != nil {
		t.Fatal(ierr)
	}
	if len(tempInfo) != 1 {
		t.Fatalf("PRAGMA temp.table_info(dup) = %v, want exactly one column (z)", tempInfo)
	}
}

// TestWritableSchemaAlterAddColumnCrossCatalogNameCollisionSafe is
// TestWritableSchemaCrossCatalogNameCollisionSafe's sibling for
// wsEditForObjectName's OTHER call site: ALTER TABLE ADD COLUMN's own
// overlay-splice reuse (addColumnUnderWritableSchemaEdit, alter_write.go).
//
// An unqualified "ALTER TABLE dup ADD COLUMN y" resolves TEMP-first (real
// SQLite's own unqualified name resolution, build.c:337-393; this engine's
// own findTableMetaIn(scopeAny,...), temp_schema.go, mirrors it) to temp's
// dup, which carries NO edit of its own -- only main's same-named dup does.
// The CORRECT, safe answer is to decline (writableSchemaDDLDecline's
// existing "a DIFFERENT table's row carries the edit" rule, unrelated to
// temp/main, already declines any ALTER ADD COLUMN whose OWN target isn't
// the session's sole outstanding edit) rather than let addColumn silently
// splice main's edited text onto temp's unrelated table.
//
// This is not a hypothetical: a mutation test (making wsEditForObjectName
// ignore its temp parameter, reproducing the OLD unscoped bare-name lookup)
// was run directly against this exact statement sequence and confirmed real
// corruption, not just a missed decline. addColumnUnderWritableSchemaEdit's
// byte-offset splice (addColumnSpliceOffset, alter_write.go) computes its
// insertion point from tbl.sql -- the table it now WRONGLY believes owns the
// edit -- so the column list below is deliberately chosen to keep that
// offset landing inside main's edited text's own parens (rather than past
// the end, which a length mismatch would otherwise make parseTableTailClauses
// reject as unparseable and incidentally decline for an unrelated reason).
// Under the mutation this statement returned NO error and left db.tables
// with sqlite_master showing TWO rows both named "dup" with IDENTICAL,
// main-edited text and sqlite_temp_master showing ZERO -- temp's dup, its
// OWN "TEMP" marker overwritten by the splice into main's untagged text, had
// been silently reclassified as a second main row by markTempSchemaRows,
// which classifies purely off stored SQL text (temp_schema.go). That is a
// materialize-level identity corruption, not merely a wrong column list.
func TestWritableSchemaAlterAddColumnCrossCatalogNameCollisionSafe(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "repro4.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()

	for _, s := range []string{
		// Temp's dup starts with the SAME leading column name ("a") main's
		// EDITED text will claim, deliberately -- so a cross-attribution bug
		// is not incidentally caught by addColumnUnderWritableSchemaEdit's
		// own unrelated "does the existing column list still match"
		// safety check (which a differently-named column, e.g. "z", would
		// trip regardless of whether the scope fix exists, making the probe
		// worthless).
		`CREATE TABLE dup(a)`,
		`CREATE TEMP TABLE dup(a)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE TABLE dup(a,bcde)' WHERE name='dup'`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}

	if e := db.Exec(`ALTER TABLE dup ADD COLUMN y`); e == nil {
		t.Fatalf("ALTER TABLE dup ADD COLUMN y: expected a decline (main's edit must not be attributed to temp's dup), got success")
	}

	// Both rows must be completely untouched by the declined ALTER.
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, tempInfo, terr := p.QueryArgs(`PRAGMA temp.table_info(dup)`, nil)
	if terr != nil {
		t.Fatal(terr)
	}
	if len(tempInfo) != 1 {
		t.Fatalf("PRAGMA temp.table_info(dup) = %v, want exactly one column (a) -- unaffected by the declined ALTER", tempInfo)
	}
	_, mainRows, merr := p.QueryArgs(`SELECT sql FROM sqlite_master WHERE name='dup'`, nil)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(mainRows) != 1 || valueText(mainRows[0][0]) != "CREATE TABLE dup(a,bcde)" {
		t.Fatalf("sqlite_master.sql for dup = %v, want the untouched edited text", mainRows)
	}
	// And exactly one row per catalog -- not the collapsed-into-one/two-in-
	// main-zero-in-temp shape the mutation produced.
	_, tempSQL, tqerr := p.QueryArgs(`SELECT sql FROM sqlite_temp_master WHERE name='dup'`, nil)
	if tqerr != nil {
		t.Fatal(tqerr)
	}
	if len(tempSQL) != 1 || valueText(tempSQL[0][0]) != "CREATE TABLE dup(a)" {
		t.Fatalf("sqlite_temp_master.sql for dup = %v, want its own untouched text", tempSQL)
	}
}

// TestWritableSchemaResetReloadCrossCatalogNameCollisionSafe is
// TestWritableSchemaCrossCatalogNameCollisionSafe/
// TestWritableSchemaAlterAddColumnCrossCatalogNameCollisionSafe's third
// sibling: writableSchemaReload's own table lookup (PRAGMA
// writable_schema=RESET's one reproducible shape, a table's sql-text TAIL
// rewrite) was the one pre-existing call site this fix's own wsCatalogKey.temp
// discriminator never got threaded into -- it resolved the edited row's
// *tableMeta via a bare "for _, t := range db.tables { if
// equalFoldName(t.name, key.name) ... }" scan, same as the other two sites
// before their own fixes, ignoring the temp/main split entirely.
//
// Unlike the other two sites, an all-main-text edit whose PREFIX would let a
// wrongly-picked temp table's tableMeta pass writableSchemaReload's own
// byte-identical-column-list safety check (tableColListEnd) is not reachable
// through an ordinary CREATE statement: this engine synthesizes the literal
// "TEMP "/"TEMPORARY " keyword into every temp object's stored sql
// (withTempKeywordIf, schema_write.go) regardless of how TEMP-ness was
// spelled, so a temp table's own sql can never share a byte-identical prefix
// with a plain "CREATE TABLE ..." edit text. That is what makes the SIMPLE
// version of this collision (below) merely a missed reload -- the OLD code
// declines it -- rather than silent corruption.
//
// Real corruption needs the malformed-but-collision-triggering edit text
// itself to start with "CREATE TEMP TABLE" (writable_schema's arbitrary-text
// UPDATE lets it): this was run directly, under the OLD (pre-fix) lookup, and
// confirmed to reach db.tables' TEMP row -- PRAGMA writable_schema=RESET
// reported SUCCESS while MAIN's own tableMeta.sql stayed the untouched
// pre-edit text (the edit silently vanished) and TEMP's tableMeta.cols/
// .strict were overwritten with MAIN's edit, making the untouched TEMP table
// wrongly STRICT. That mutation is exercised directly below too, via the
// runtime NOT NULL check, rather than only inspecting stored text.
//
// RULE ZERO citation for the lookup fix itself: sqliteInt.h:1246-1261 (two
// different reserved catalog-table strings) and build.c:337-393
// (sqlite3FindTable's unqualified-name probe order) -- the same citation
// wsEditForObjectName's own doc comment above already gives, since this is
// the identical resolution question asked a third time.
func TestWritableSchemaResetReloadCrossCatalogNameCollisionSafe(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "repro5.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()

	for _, s := range []string{
		// TEMP's dup precedes MAIN's dup in db.tables (creation order), so a
		// bare-name lookup that ignores isTemp finds IT first.
		`CREATE TEMP TABLE dup(id ANY PRIMARY KEY, x TEXT)`,
		`CREATE TABLE dup(id ANY PRIMARY KEY, x TEXT)`,
		`PRAGMA writable_schema=ON`,
		// An ordinary tail-only edit on MAIN's own row (sql references its
		// OWN current value, so this is genuinely main's true prefix, not a
		// spoofed one) -- exactly the shape writableSchemaReload's doc
		// comment says is reproducible.
		`UPDATE sqlite_master SET sql=(sql||'STRICT') WHERE name='dup'`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	if e := db.Exec(`PRAGMA writable_schema=RESET`); e != nil {
		t.Fatalf("PRAGMA writable_schema=RESET: %v (should be SERVED: a plain tail-only edit on MAIN's own row, unaffected by an unrelated same-named TEMP table)", e)
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, mainRows, merr := p.QueryArgs(`SELECT sql FROM sqlite_master WHERE name='dup'`, nil)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(mainRows) != 1 || valueText(mainRows[0][0]) != "CREATE TABLE dup(id ANY PRIMARY KEY, x TEXT)STRICT" {
		t.Fatalf("sqlite_master.sql for dup = %v, want the STRICT-tail edit applied to MAIN's own row", mainRows)
	}
	_, tempRows, terr := p.QueryArgs(`SELECT sql FROM sqlite_temp_master WHERE name='dup'`, nil)
	if terr != nil {
		t.Fatal(terr)
	}
	// sqlite_temp_master's own sql column never shows the TEMP keyword
	// (withoutTempKeyword, temp_catalog.go -- C SQLite's temp schema never
	// stored it either), so the untouched-original text reads the same as
	// main's own would have BEFORE its edit -- not STRICT, and it must stay
	// that way.
	if len(tempRows) != 1 || valueText(tempRows[0][0]) != "CREATE TABLE dup(id ANY PRIMARY KEY, x TEXT)" {
		t.Fatalf("sqlite_temp_master.sql for dup = %v, want its own text, UNTOUCHED by main's reload", tempRows)
	}

	// Runtime cross-check: TEMP's dup must stay non-STRICT (an implicit NULL
	// id is fine), while MAIN's dup is now really STRICT (NOT NULL on its
	// ANY PRIMARY KEY column -- see writableSchemaReload's own doc comment
	// for the STRICT/ANY PRIMARY KEY oracle citation).
	if e := db.Exec(`INSERT INTO temp.dup(x) VALUES('nine')`); e != nil {
		t.Errorf("INSERT INTO temp.dup(x) VALUES('nine'): want success (temp's dup was never edited), got %v", e)
	}
	if e := db.Exec(`INSERT INTO main.dup(x) VALUES('nine')`); e == nil {
		t.Errorf("INSERT INTO main.dup(x) VALUES('nine'): want a NOT NULL failure (main's dup is now STRICT), got success")
	}
}
