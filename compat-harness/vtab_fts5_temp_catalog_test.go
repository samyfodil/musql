//go:build sqlite_fts5

// This file tests fts5/fts5vocab temp catalog support.
//     names (TestCreateVirtualTableTempFts5vocabAnswerDiff), which is what
//     the corpus scenario is really probing;
//
//   - every OTHER module keeps its existing "temp is not supported" decline
//     unchanged (vtab_temp_catalog_test.go's
//     TestCreateVirtualTableTempNonFts3StillDeclined already covers rtree
//     and generate_series; this file does not touch that boundary).
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

func init() { engine.RegisterFTS5() }

// TestCreateVirtualTableTempFts5Accepted pins the corpus's fifth statement in
// isolation: an fts5 table created directly IN temp, with its shadow tables
// filed in sqlite_temp_master (not sqlite_master), and INSERT/MATCH working
// against it -- mirrors vtab_temp_catalog_test.go's
// TestCreateVirtualTableTempFts3Accepted exactly, fts5 in place of fts3.
func TestCreateVirtualTableTempFts5Accepted(t *testing.T) {
	dir := t.TempDir()

	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)
	if _, err := cgodb.Exec(`CREATE VIRTUAL TABLE temp.t1 USING fts5(x)`); err != nil {
		t.Fatalf("oracle CREATE VIRTUAL TABLE temp.t1 USING fts5(x): %v (expected to succeed against C SQLite)", err)
	}
	if _, err := cgodb.Exec(`INSERT INTO t1(x) VALUES('foo bar')`); err != nil {
		t.Fatalf("oracle INSERT INTO t1: %v", err)
	}
	var matched string
	if err := cgodb.QueryRow(`SELECT x FROM t1 WHERE t1 MATCH 'foo'`).Scan(&matched); err != nil {
		t.Fatalf("oracle MATCH: %v", err)
	}
	cgoTemp := catalogNames(t, cgodb, "sqlite_temp_master")
	cgoMain := catalogNames(t, cgodb, "sqlite_master")
	// t1 plus its shadow tables (data/idx/content/docsize/config).
	if len(cgoTemp) != 6 {
		t.Fatalf("oracle sqlite_temp_master after CREATE VIRTUAL TABLE temp.t1 USING fts5(x): got %v, want 6 rows (t1 + its 5 shadow tables)", cgoTemp)
	}
	if len(cgoMain) != 0 {
		t.Fatalf("oracle sqlite_master after CREATE VIRTUAL TABLE temp.t1 USING fts5(x): got %v, want none", cgoMain)
	}

	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if err := godb.Exec(`CREATE VIRTUAL TABLE temp.t1 USING fts5(x)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE temp.t1 USING fts5(x): %v (the oracle accepts this shape; see this file's package comment)", err)
	}
	if err := godb.Exec(`INSERT INTO t1(x) VALUES('foo bar')`); err != nil {
		t.Fatalf("engine INSERT INTO t1: %v", err)
	}
	pg, err := godb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pg.Close()
	_, matchRows, merr := pg.QueryArgs(`SELECT x FROM t1 WHERE t1 MATCH 'foo'`, nil)
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
	for _, want := range []string{"t1", "t1_data", "t1_idx", "t1_content", "t1_docsize", "t1_config"} {
		if !containsStr(goTemp, want) {
			t.Errorf("engine sqlite_temp_master does not list %s: %v", want, goTemp)
		}
		if containsStr(goMain, want) {
			t.Errorf("engine sqlite_master WRONGLY lists %s (it belongs in temp): %v", want, goMain)
		}
	}
}

// TestCreateVirtualTableTempFts5vocabVerbatimStatementsAccepted reproduces
// the five mined corpus statements BYTE FOR BYTE (fts5vocab.test 5.1's four,
// plus 5.0's own "CREATE VIRTUAL TABLE temp.t1 USING fts5(x)"), against a
// fresh db that never creates a colliding main.t1 -- fts5vocab defers
// resolving its target to SELECT time (mirroring fts4aux, see
// vtab_fts5vocab.go's package comment), so every one of these CREATEs
// succeeds whether or not "t1" actually exists anywhere, on both engines.
func TestCreateVirtualTableTempFts5vocabVerbatimStatementsAccepted(t *testing.T) {
	stmts := []string{
		`CREATE VIRTUAL TABLE temp.vt2 USING fts5vocab(temp, t1, row)`,
		`CREATE VIRTUAL TABLE temp.vt1 USING fts5vocab(t1, row)`,
		`CREATE VIRTUAL TABLE temp.vm USING fts5vocab(main, t1, row)`,
		`CREATE VIRTUAL TABLE temp.va USING fts5vocab(aux, t1, row)`,
		`CREATE VIRTUAL TABLE temp.t1 USING fts5(x)`,
	}
	dir := t.TempDir()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)
	for _, s := range stmts {
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("oracle %q: %v (this shape is expected to succeed against C SQLite)", s, err)
		}
	}

	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	for _, s := range stmts {
		if err := godb.Exec(s); err != nil {
			t.Errorf("engine %q: %v (the oracle accepts this shape; see this file's package comment)", s, err)
		}
	}
}

// TestCreateVirtualTableTempFts5vocabAnswerDiff verifies the MECHANISM the
// corpus scenario is really probing -- that all four fts5vocab qualifier
// forms (bare, "main", "temp", an ATTACHed name) actually read the target in
// the RIGHT catalog, not just that CREATE accepts the statement -- using
// names distinct enough to avoid the cross-catalog collision boundary this
// file's package comment documents (main's own table is "m1", so it never
// collides with temp's "t1", exactly like the corpus's aux.t1 vs temp.t1
// never collide with each other there either since they're different
// databases entirely).
func TestCreateVirtualTableTempFts5vocabAnswerDiff(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", `CREATE VIRTUAL TABLE x1 USING fts5(x)`, `INSERT INTO x1(x) VALUES('m n o')`, `INSERT INTO x1(x) VALUES('x n z')`)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	setup := []string{
		`CREATE VIRTUAL TABLE m1 USING fts5(x)`,
		`INSERT INTO m1(x) VALUES('a b c')`,
		`INSERT INTO m1(x) VALUES('d e f')`,
		`CREATE VIRTUAL TABLE temp.t1 USING fts5(x)`,
		`INSERT INTO temp.t1(x) VALUES('1 2 3')`,
		`INSERT INTO temp.t1(x) VALUES('4 5 6')`,
		`CREATE VIRTUAL TABLE temp.vm USING fts5vocab(main, m1, row)`,
		`CREATE VIRTUAL TABLE temp.vt1 USING fts5vocab(t1, row)`,
		`CREATE VIRTUAL TABLE temp.vt2 USING fts5vocab(temp, t1, row)`,
		`CREATE VIRTUAL TABLE temp.va USING fts5vocab(aux, x1, row)`,
	}
	if _, err := p.cgo.Exec(p.stmt(`ATTACH '{0}' AS aux`, p.cgoPaths)); err != nil {
		t.Fatalf("oracle ATTACH: %v", err)
	}
	for _, s := range setup {
		if _, err := p.cgo.Exec(s); err != nil {
			t.Fatalf("oracle %q: %v", s, err)
		}
	}
	if err := p.godb.Exec(p.stmt(`ATTACH '{0}' AS aux`, p.goPaths)); err != nil {
		t.Fatalf("engine ATTACH: %v", err)
	}
	for _, s := range setup {
		if err := p.godb.Exec(s); err != nil {
			t.Fatalf("engine %q: %v", s, err)
		}
	}

	pg, err := p.godb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pg.Close()
	for _, probe := range []struct {
		name  string
		query string
	}{
		{"vm reads main's m1", `SELECT term, doc, cnt FROM vm ORDER BY term`},
		{"vt1 (bare) reads temp's OWN t1", `SELECT term, doc, cnt FROM vt1 ORDER BY term`},
		{"vt2 (temp-qualified) reads temp's t1 identically to vt1", `SELECT term, doc, cnt FROM vt2 ORDER BY term`},
		{"va reads the ATTACHed aux's x1", `SELECT term, doc, cnt FROM va ORDER BY term`},
	} {
		cgoCols, cgoRows, cgoErr := tclRunCGOQuery(p.cgo, probe.query)
		if cgoErr != nil {
			t.Fatalf("%s: oracle %q: %v", probe.name, probe.query, cgoErr)
		}
		if len(cgoRows) == 0 {
			t.Fatalf("%s: oracle %q returned no rows -- probe is not exercising anything", probe.name, probe.query)
		}
		goCols, goVals, goErr := pg.QueryArgs(probe.query, nil)
		if goErr != nil {
			t.Fatalf("%s: engine %q: %v", probe.name, probe.query, goErr)
		}
		goRows := make([][]string, len(goVals))
		for i, row := range goVals {
			cells := make([]string, len(row))
			for j, v := range row {
				cells[j] = normalizeEngineValue(v)
			}
			goRows[i] = cells
		}
		if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
			t.Errorf("%s (%q): %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", probe.name, probe.query, reason, goCols, goRows, cgoCols, cgoRows)
		}
	}
}

// TestCreateVirtualTableTempFts5CrossCatalogCollisionDeclined pins the SAME
// documented boundary vtab_temp_catalog_test.go's
// TestCreateVirtualTableTempFts3CrossCatalogCollisionDeclined pins for
// fts3/fts4: two fts5 tables of the identical name, one per catalog. See this
// file's own package comment for the full reasoning (identical to fts3/4's,
// citing fts5SchemaTok/fts5_shadow.go's name-keyed lookups in place of
// materializeFts3Ascending's).
func TestCreateVirtualTableTempFts5CrossCatalogCollisionDeclined(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	if err := godb.Exec(`CREATE VIRTUAL TABLE t1 USING fts5(x)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE t1: %v", err)
	}
	if err := godb.Exec(`CREATE VIRTUAL TABLE temp.t1 USING fts5(x)`); err == nil {
		t.Fatal("engine ACCEPTED a same-named fts5 table in BOTH main and temp -- the oracle keeps the two independent, but this engine's fts5 read path cannot (see this file's package comment); update this test to the stronger assertion (a real per-catalog read, checked against the oracle) only once fts5SchemaTok and the shadow-table lookups are made scope-aware")
	}

	// The reverse order (temp first, then a colliding main) must decline
	// identically -- the guard checks BOTH directions (vtab.go's "other :=
	// scopeMain; if scope == scopeMain { other = scopeTemp }").
	dir2 := t.TempDir()
	godb2, err := engine.Create(filepath.Join(dir2, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb2.Discard()
	if err := godb2.Exec(`CREATE VIRTUAL TABLE temp.t2 USING fts5(x)`); err != nil {
		t.Fatalf("engine CREATE VIRTUAL TABLE temp.t2: %v", err)
	}
	if err := godb2.Exec(`CREATE VIRTUAL TABLE t2 USING fts5(x)`); err == nil {
		t.Fatal("engine ACCEPTED a same-named fts5 table in BOTH temp and main (temp created first) -- see this file's package comment")
	}
}
