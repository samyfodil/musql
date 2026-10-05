// This file tests that schema text is normalized: SQLite removes schema
// qualifiers and IF NOT EXISTS from stored CREATE statements.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// schemaTextStmts covers each object kind against each thing that gets
// normalized away: a schema qualifier, IF NOT EXISTS, extra whitespace, a
// trailing semicolon -- and, for an index, the UNIQUE modifier SQLite is
// careful to KEEP. Internal spacing after the name ("q2  (a, b)",
// "qi1  ON  q1 ( a )") must survive untouched.
var schemaTextStmts = []string{
	`CREATE TABLE main.q1(a, b)`,
	`CREATE   TABLE   IF   NOT   EXISTS   q2  (a, b)`,
	`CREATE TABLE q3(a, b, c VARCHAR(9), UNIQUE(a,b))`,
	`CREATE TABLE q4(a);`,
	`CREATE  UNIQUE  INDEX  main.qi1  ON  q1 ( a )`,
	`CREATE INDEX IF NOT EXISTS qi2 ON q1(b)`,
	`CREATE  VIEW  main.qv1  AS  SELECT 1`,
	`CREATE VIEW IF NOT EXISTS qv2 AS SELECT 2`,
}

// TestStoredSchemaTextKeepsTemp: TEMP is the one modifier normalizeSchemaSQL
// must NOT drop. C SQLite does drop it (a temp object's row lives in the
// temp schema, whose text says plain "CREATE TABLE"), but this engine has a
// single schema b-tree and recognizes a temp object ONLY by that keyword
// surviving in its stored text -- which is what lets a catalog read be split
// into the two catalogs C SQLite keeps apart (engine/temp_catalog.go)
// instead of wrongly reporting a temp table as if it were in main (two
// alter.test wrongs when the keyword was briefly dropped).
//
// So the observable is the SPLIT: the temp table must appear in
// sqlite_temp_master and NOT in sqlite_master, and its sql must read back with
// the keyword taken off again -- exactly what C SQLite stores. This test
// used to assert that sqlite_master merely DECLINED, which the split replaced.
func TestStoredSchemaTextKeepsTemp(t *testing.T) {
	db, err := engine.Create(filepath.Join(t.TempDir(), "temp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(`CREATE TABLE kept(x)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE   TEMP   TABLE  objlist(a, b)`); err != nil {
		t.Fatal(err)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	// engineRowsToStrings tags each cell with its storage class, so the wanted
	// values carry the same "T:" TEXT prefix every other test in this file uses.
	for _, tc := range []struct{ q, want string }{
		{`SELECT name FROM sqlite_master`, "T:kept"},
		{`SELECT name FROM sqlite_temp_master`, "T:objlist"},
		// C SQLite reprints "CREATE <kind> " and keeps the source from the
		// NAME token on, so the run of spaces before "objlist" collapses but
		// the "(a, b)" after it survives byte for byte.
		{`SELECT sql FROM sqlite_temp_master`, "T:CREATE TABLE objlist(a, b)"},
	} {
		_, rows, qerr := p.QueryArgs(tc.q, nil)
		if qerr != nil {
			t.Errorf("%s: %v", tc.q, qerr)
			continue
		}
		got := engineRowsToStrings(rows)
		if len(got) != 1 || len(got[0]) != 1 || got[0][0] != tc.want {
			t.Errorf("%s: got %v, want [[%s]]", tc.q, got, tc.want)
		}
	}
}

const schemaTextQuery = `SELECT name, sql FROM sqlite_master ORDER BY name`

func TestStoredSchemaTextParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	for _, s := range schemaTextStmts {
		eErr := edb.Exec(s)
		_, cErr := cdb.Exec(s)
		if (eErr != nil) != (cErr != nil) {
			t.Fatalf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eErr, cErr)
		}
	}

	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	eCols, eVals, eErr := p.QueryArgs(schemaTextQuery, nil)
	if eErr != nil {
		t.Fatal(eErr)
	}
	cCols, cRows, cErr := cgoSelect(t, cdb, schemaTextQuery, nil)
	if cErr != nil {
		t.Fatal(cErr)
	}
	eRows := engineRowsToStrings(eVals)
	if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
		t.Errorf("stored schema text DIVERGES: %s\n  engine: %v\n  cgo:    %v", reason, eRows, cRows)
	}
}

// addColumnStmts pin WHERE "ALTER TABLE ... ADD COLUMN" splices the new
// column into the stored text: C SQLite puts it after the last COLUMN
// DEFINITION and BEFORE any table-level constraint (its addColOffset), not
// at the end of the list -- verified directly. An inline "a PRIMARY KEY" is
// a column definition, not a table constraint, and must not move the point.
var addColumnStmts = []string{
	`CREATE TABLE q3(a, b, c VARCHAR(9), UNIQUE(a,b), CHECK(a>0))`,
	`CREATE TABLE q4(a, b)`,
	`CREATE TABLE q5(a, b, CONSTRAINT ck CHECK(a>0))`,
	`CREATE TABLE q6(a PRIMARY KEY, b)`,
	`ALTER TABLE q3 ADD COLUMN zz INT`,
	`ALTER TABLE q3 ADD COLUMN yy`,
	`ALTER TABLE q4 ADD COLUMN zz`,
	`ALTER TABLE q5 ADD COLUMN zz TEXT DEFAULT 'x'`,
	`ALTER TABLE q6 ADD COLUMN zz`,
}

func TestAddColumnSchemaTextParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	for _, s := range addColumnStmts {
		eErr := edb.Exec(s)
		_, cErr := cdb.Exec(s)
		if (eErr != nil) != (cErr != nil) {
			t.Fatalf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eErr, cErr)
		}
	}

	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	const q = `SELECT name, sql FROM sqlite_master WHERE type='table' ORDER BY name`
	eCols, eVals, eErr := p.QueryArgs(q, nil)
	if eErr != nil {
		t.Fatal(eErr)
	}
	cCols, cRows, cErr := cgoSelect(t, cdb, q, nil)
	if cErr != nil {
		t.Fatal(cErr)
	}
	eRows := engineRowsToStrings(eVals)
	if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
		t.Errorf("ADD COLUMN schema text DIVERGES: %s\n  engine: %v\n  cgo:    %v", reason, eRows, cRows)
	}
}

// TestStoredSchemaTextSurvivesReopen: the normalized text is what a reopen
// re-parses the schema from, so it has to still describe the same table.
func TestStoredSchemaTextSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE main.t(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE   INDEX  IF NOT EXISTS  main.ti  ON  t(b)`,
		`INSERT INTO t VALUES(1,'x')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := db.Exec(`INSERT INTO t VALUES(2,'y')`); err != nil {
		t.Fatalf("insert after reopen: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	var ic string
	if err := cdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatal(err)
	}
	if ic != "ok" {
		t.Errorf("integrity_check: %s", ic)
	}
	var n int
	if err := cdb.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows after reopen: %d, want 2", n)
	}
}
