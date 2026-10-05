// This file gates CREATE VIRTUAL TABLE in the TEMP catalog. Shadow tables
// belong to the same catalog as their table. fts4aux's two-argument form is
// accepted only when fts4aux itself is created in temp.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

// catalogNames returns the sorted names in a schema catalog table.
func catalogNames(t *testing.T, db *sql.DB, catalog string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM ` + catalog + ` ORDER BY name`)
	if err != nil {
		t.Fatalf("%s: %v", catalog, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", catalog, err)
	}
	return names
}

func assertNamesEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestCreateVirtualTableTempFts4auxAccepted checks that a temp fts4aux table
// targeting main is accepted and listed in sqlite_temp_master, not sqlite_master.
func TestCreateVirtualTableTempFts4auxAccepted(t *testing.T) {
	dir := t.TempDir()

	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE VIRTUAL TABLE ft1 USING fts4(a)`,
		`INSERT INTO ft1(a) VALUES('hello world')`,
		`CREATE VIRTUAL TABLE temp.aux1 USING fts4aux(main, ft1)`,
	} {
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("oracle %q: %v (this shape is expected to succeed against C SQLite)", s, err)
		}
	}
	var term string
	if err := cgodb.QueryRow(`SELECT term FROM aux1 LIMIT 1`).Scan(&term); err != nil {
		t.Fatalf("oracle SELECT FROM temp aux1: %v", err)
	}
	if term == "" {
		t.Fatal("oracle temp aux1 returned an empty term")
	}
	cgoTemp := catalogNames(t, cgodb, "sqlite_temp_master")
	cgoMain := catalogNames(t, cgodb, "sqlite_master")

	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if err := godb.Exec(`CREATE VIRTUAL TABLE ft1 USING fts4(a)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE ft1: %v", err)
	}
	if err := godb.Exec(`INSERT INTO ft1(a) VALUES('hello world')`); err != nil {
		t.Fatalf("engine INSERT: %v", err)
	}
	if err := godb.Exec(`CREATE VIRTUAL TABLE temp.aux1 USING fts4aux(main, ft1)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE temp.aux1 USING fts4aux(main, ft1): %v (the oracle accepts this shape; see this file's package comment)", err)
	}
	pg, err := godb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pg.Close()
	cols, rows, qerr := pg.QueryArgs(`SELECT term FROM aux1 LIMIT 1`, nil)
	if qerr != nil {
		t.Fatalf("engine SELECT FROM temp aux1: %v", qerr)
	}
	if len(rows) != 1 || len(cols) != 1 || string(rows[0][0].S) != term {
		t.Fatalf("engine temp aux1 term: got %v, want [%q]", rows, term)
	}

	_, goTempRows, gerr := pg.QueryArgs(`SELECT name FROM sqlite_temp_master ORDER BY name`, nil)
	if gerr != nil {
		t.Fatalf("engine sqlite_temp_master: %v", gerr)
	}
	_, goMainRows, gerr := pg.QueryArgs(`SELECT name FROM sqlite_master ORDER BY name`, nil)
	if gerr != nil {
		t.Fatalf("engine sqlite_master: %v", gerr)
	}
	var goTemp, goMain []string
	for _, r := range goTempRows {
		goTemp = append(goTemp, string(r[0].S))
	}
	for _, r := range goMainRows {
		goMain = append(goMain, string(r[0].S))
	}
	assertNamesEqual(t, "sqlite_temp_master", goTemp, cgoTemp)
	assertNamesEqual(t, "sqlite_master", goMain, cgoMain)
	if !containsStr(goTemp, "aux1") {
		t.Fatalf("engine sqlite_temp_master does not list aux1: %v", goTemp)
	}
	if containsStr(goMain, "aux1") {
		t.Fatalf("engine sqlite_master WRONGLY lists aux1 (it belongs in temp): %v", goMain)
	}
}

// TestCreateVirtualTableTempFts3Accepted checks that a temp fts3 table and its
// shadow tables land in sqlite_temp_master, and that INSERT/MATCH work.
func TestCreateVirtualTableTempFts3Accepted(t *testing.T) {
	dir := t.TempDir()

	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)
	if _, err := cgodb.Exec(`CREATE VIRTUAL TABLE temp.t1 USING fts3(a)`); err != nil {
		t.Fatalf("oracle CREATE VIRTUAL TABLE temp.t1 USING fts3(a): %v (expected to succeed against C SQLite)", err)
	}
	if _, err := cgodb.Exec(`INSERT INTO t1(a) VALUES('foo bar')`); err != nil {
		t.Fatalf("oracle INSERT INTO t1: %v", err)
	}
	var matched string
	if err := cgodb.QueryRow(`SELECT a FROM t1 WHERE t1 MATCH 'foo'`).Scan(&matched); err != nil {
		t.Fatalf("oracle MATCH: %v", err)
	}
	cgoTemp := catalogNames(t, cgodb, "sqlite_temp_master")
	cgoMain := catalogNames(t, cgodb, "sqlite_master")
	// t1 plus content, segments, segdir and the autoindex.
	if len(cgoTemp) != 5 {
		t.Fatalf("oracle sqlite_temp_master after CREATE VIRTUAL TABLE temp.t1 USING fts3(a): got %v, want 5 rows (t1 + its shadow tables)", cgoTemp)
	}
	if len(cgoMain) != 0 {
		t.Fatalf("oracle sqlite_master after CREATE VIRTUAL TABLE temp.t1 USING fts3(a): got %v, want none", cgoMain)
	}

	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if err := godb.Exec(`CREATE VIRTUAL TABLE temp.t1 USING fts3(a)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE temp.t1 USING fts3(a): %v (the oracle accepts this shape; see this file's package comment)", err)
	}
	if err := godb.Exec(`INSERT INTO t1(a) VALUES('foo bar')`); err != nil {
		t.Fatalf("engine INSERT INTO t1: %v", err)
	}
	pg, err := godb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pg.Close()
	_, matchRows, merr := pg.QueryArgs(`SELECT a FROM t1 WHERE t1 MATCH 'foo'`, nil)
	if merr != nil {
		t.Fatalf("engine MATCH: %v", merr)
	}
	if len(matchRows) != 1 || string(matchRows[0][0].S) != matched {
		t.Fatalf("engine MATCH result: got %v, want [%q]", matchRows, matched)
	}
	_, goTempRows, gerr := pg.QueryArgs(`SELECT name FROM sqlite_temp_master ORDER BY name`, nil)
	if gerr != nil {
		t.Fatalf("engine sqlite_temp_master: %v", gerr)
	}
	_, goMainRows, gerr := pg.QueryArgs(`SELECT name FROM sqlite_master ORDER BY name`, nil)
	if gerr != nil {
		t.Fatalf("engine sqlite_master: %v", gerr)
	}
	var goTemp, goMain []string
	for _, r := range goTempRows {
		goTemp = append(goTemp, string(r[0].S))
	}
	for _, r := range goMainRows {
		goMain = append(goMain, string(r[0].S))
	}
	assertNamesEqual(t, "sqlite_temp_master", goTemp, cgoTemp)
	assertNamesEqual(t, "sqlite_master", goMain, cgoMain)
	for _, want := range []string{"t1", "t1_content", "t1_segments", "t1_segdir"} {
		if !containsStr(goTemp, want) {
			t.Errorf("engine sqlite_temp_master does not list %s: %v", want, goTemp)
		}
		if containsStr(goMain, want) {
			t.Errorf("engine sqlite_master WRONGLY lists %s (it belongs in temp): %v", want, goMain)
		}
	}
}

// TestCreateVirtualTableTempFts4auxAttachedAccepted checks a temp fts4aux
// table whose target lives in an ATTACHed database.
func TestCreateVirtualTableTempFts4auxAttachedAccepted(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "att", `CREATE VIRTUAL TABLE ft1 USING fts4(a)`, `INSERT INTO ft1(a) VALUES('v w')`)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	if _, err := p.cgo.Exec(p.stmt(`ATTACH '{0}' AS att`, p.cgoPaths)); err != nil {
		t.Fatalf("oracle ATTACH: %v", err)
	}
	if _, err := p.cgo.Exec(`CREATE VIRTUAL TABLE temp.aux2 USING fts4aux(att, ft1)`); err != nil {
		t.Fatalf("oracle CREATE VIRTUAL TABLE temp.aux2 USING fts4aux(att, ft1): %v (expected to succeed)", err)
	}
	var term string
	if err := p.cgo.QueryRow(`SELECT term FROM aux2 LIMIT 1`).Scan(&term); err != nil {
		t.Fatalf("oracle SELECT FROM temp aux2 (reading the ATTACHed ft1's index): %v", err)
	}

	if err := p.godb.Exec(p.stmt(`ATTACH '{0}' AS att`, p.goPaths)); err != nil {
		t.Fatalf("engine ATTACH: %v", err)
	}
	if err := p.godb.Exec(`CREATE VIRTUAL TABLE temp.aux2 USING fts4aux(att, ft1)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE temp.aux2 USING fts4aux(att, ft1): %v (the oracle accepts this shape; see this file's package comment)", err)
	}
	pg, err := p.godb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pg.Close()
	cols, rows, qerr := pg.QueryArgs(`SELECT term FROM aux2 LIMIT 1`, nil)
	if qerr != nil {
		t.Fatalf("engine SELECT FROM temp aux2: %v", qerr)
	}
	if len(rows) != 1 || len(cols) != 1 || string(rows[0][0].S) != term {
		t.Fatalf("engine temp aux2 term: got %v, want [%q]", rows, term)
	}
}

// TestCreateVirtualTableTempTakesAnyCreatableModule checks that any creatable
// module works in temp, while eponymous-only modules (no xCreate) give C
// SQLite's "no such module".
func TestCreateVirtualTableTempTakesAnyCreatableModule(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if err := godb.Exec(`CREATE VIRTUAL TABLE temp.r1 USING rtree(id, x0, x1)`); err != nil {
		t.Errorf("engine DECLINED a temp rtree, which C SQLite creates: %v", err)
	}
	if err := godb.Exec(`INSERT INTO r1 VALUES(1, 0.0, 1.0)`); err != nil {
		t.Errorf("INSERT into a temp rtree: %v", err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE temp.g1 USING generate_series`,
		`CREATE VIRTUAL TABLE g2 USING generate_series`,
		`CREATE VIRTUAL TABLE temp.p1 USING pragma_table_list`,
	} {
		err := godb.Exec(s)
		if err == nil {
			t.Errorf("engine ACCEPTED %q; C SQLite answers \"no such module\" for an eponymous-only module", s)
		} else if got := err.Error(); !strings.Contains(got, "no such module") {
			t.Errorf("%q declined with %q, want C SQLite's \"no such module\"", s, got)
		}
	}
}

// TestCreateVirtualTableTempFts3CrossCatalogCollisionDeclined checks that a
// second same-named fts4 table in the other catalog is declined: the fts3/fts4
// read path resolves shadow tables by name, so the pair would be ambiguous.
func TestCreateVirtualTableTempFts3CrossCatalogCollisionDeclined(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if err := godb.Exec(`CREATE VIRTUAL TABLE t1 USING fts4(a)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE t1: %v", err)
	}
	if err := godb.Exec(`CREATE VIRTUAL TABLE temp.t1 USING fts4(a)`); err == nil {
		t.Fatal("engine ACCEPTED a same-named fts4 table in BOTH main and temp -- the oracle keeps the two independent, but this engine's shadow-table read path cannot (see this file's package comment); update this test to the stronger assertion (a real per-catalog read, checked against the oracle) only once materializeFts3Ascending is made scope-aware")
	}
}

// TestCreateVirtualTableTempSurvivesSessionGoneAfterReopen checks that a temp
// vtab lives for its connection only and does not leak into main.
func TestCreateVirtualTableTempSurvivesSessionGoneAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE VIRTUAL TABLE ft1 USING fts4(a)`,
		`INSERT INTO ft1(a) VALUES('hello world')`,
		`CREATE VIRTUAL TABLE temp.aux1 USING fts4aux(main, ft1)`,
		`CREATE VIRTUAL TABLE temp.t1 USING fts3(a)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
	var term string
	if err := db.QueryRow(`SELECT term FROM aux1 LIMIT 1`).Scan(&term); err != nil {
		t.Fatalf("SELECT FROM temp aux1 within the session: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open (reopen): %v", err)
	}
	defer db2.Close()
	// The vtab and its shadow tables must be gone on a new connection.
	if _, err := db2.Query(`SELECT term FROM aux1 LIMIT 1`); err == nil {
		t.Error("engine still answers temp aux1 after reopening a NEW connection on the same file -- it must not outlive the connection that created it")
	}
	if _, err := db2.Query(`SELECT * FROM t1`); err == nil {
		t.Error("engine still answers temp t1 after reopening a NEW connection on the same file -- it must not outlive the connection that created it")
	}
	tempNames := catalogNames(t, db2, "sqlite_temp_master")
	if len(tempNames) != 0 {
		t.Errorf("sqlite_temp_master after reopen: got %v, want none", tempNames)
	}
	mainNames := catalogNames(t, db2, "sqlite_master")
	for _, leaked := range []string{"aux1", "t1", "t1_content", "t1_segments", "t1_segdir"} {
		if containsStr(mainNames, leaked) {
			t.Errorf("sqlite_master after reopen WRONGLY lists %s -- a temp object must never leak into main on drop: %v", leaked, mainNames)
		}
	}
	// ft1 (created bare, i.e. in main) and its own shadow tables must
	// survive: only the TEMP objects are dropped.
	for _, want := range []string{"ft1", "ft1_content", "ft1_docsize", "ft1_segdir", "ft1_segments", "ft1_stat"} {
		if !containsStr(mainNames, want) {
			t.Errorf("sqlite_master after reopen is missing %s (a MAIN object, should have survived): %v", want, mainNames)
		}
	}
}

// TestDMLDispatchScopesToSameNamedVtabAcrossCatalogs checks that INSERT,
// UPDATE, DELETE and DROP on temp.t1 reach the temp fts4 table, not a
// same-named main rtree created earlier. Results are read through fts4's
// ordinary %_content shadow table, which resolves by root page.
func TestDMLDispatchScopesToSameNamedVtabAcrossCatalogs(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if err := godb.Exec(`CREATE VIRTUAL TABLE t1 USING rtree(id, minX, maxX)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE t1 USING rtree: %v", err)
	}
	if err := godb.Exec(`CREATE VIRTUAL TABLE temp.t1 USING fts4(a)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE temp.t1 USING fts4(a): %v", err)
	}
	// INSERT: must reach the TEMP fts4 table, not the main rtree one.
	if err := godb.Exec(`INSERT INTO temp.t1(a) VALUES('hello world')`); err != nil {
		t.Fatalf("engine INSERT INTO temp.t1(a): %v (must dispatch to the TEMP fts4 vtab, not the main rtree one)", err)
	}
	if err := godb.Exec(`INSERT INTO temp.t1(a) VALUES('second row')`); err != nil {
		t.Fatalf("engine second INSERT INTO temp.t1(a): %v", err)
	}
	// UPDATE: fts4's own UPDATE-by-docid form, still scoped to temp.
	if err := godb.Exec(`UPDATE temp.t1 SET a='updated row' WHERE docid=2`); err != nil {
		t.Fatalf("engine UPDATE temp.t1: %v (must dispatch to the TEMP fts4 vtab, not the main rtree one)", err)
	}

	readShadow := func() [][]string {
		t.Helper()
		pg, err := godb.SnapshotPager()
		if err != nil {
			t.Fatalf("SnapshotPager: %v", err)
		}
		defer pg.Close()
		_, rows, qerr := pg.QueryArgs(`SELECT c0a FROM temp.t1_content ORDER BY docid`, nil)
		if qerr != nil {
			t.Fatalf("engine SELECT FROM temp.t1_content (fts4's ORDINARY shadow table -- root-page scoped, unaffected by the separate vtab-schema-resolution gap this test's comment describes): %v", qerr)
		}
		out := make([][]string, len(rows))
		for i, r := range rows {
			out[i] = []string{string(r[0].S)}
		}
		return out
	}
	if got := readShadow(); len(got) != 2 || got[0][0] != "hello world" || got[1][0] != "updated row" {
		t.Fatalf("engine temp.t1_content rows: got %v, want [[hello world] [updated row]]", got)
	}

	// DELETE: still scoped to temp.
	if err := godb.Exec(`DELETE FROM temp.t1 WHERE docid=1`); err != nil {
		t.Fatalf("engine DELETE FROM temp.t1: %v (must dispatch to the TEMP fts4 vtab, not the main rtree one)", err)
	}
	if got := readShadow(); len(got) != 1 || got[0][0] != "updated row" {
		t.Fatalf("engine temp.t1_content after DELETE: got %v, want [[updated row]]", got)
	}

	// DROP TABLE removes only temp t1 and its shadows.
	if err := godb.Exec(`DROP TABLE temp.t1`); err != nil {
		t.Fatalf("engine DROP TABLE temp.t1: %v (must dispatch to the TEMP fts4 vtab, not the main rtree one)", err)
	}
	pg3, err := godb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pg3.Close()
	if _, _, qerr := pg3.QueryArgs(`SELECT * FROM temp.t1_content`, nil); qerr == nil {
		t.Error("engine still answers temp.t1_content after DROP TABLE temp.t1 -- it should be gone")
	}
	_, tempRows, gerr := pg3.QueryArgs(`SELECT name FROM sqlite_temp_master ORDER BY name`, nil)
	if gerr != nil {
		t.Fatalf("engine sqlite_temp_master after DROP: %v", gerr)
	}
	if len(tempRows) != 0 {
		t.Errorf("engine sqlite_temp_master after DROP TABLE temp.t1: got %v, want empty", tempRows)
	}
}
