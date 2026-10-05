// This file gates that ANALYZE creates sqlite_stat1 for every accepted form,
// including catalog names like sqlite_master, and that arguments are accepted
// once schema qualifiers are honored.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// statNameProbe checks for sqlite_stat1 in the schema catalog.
const statNameProbe = `SELECT name FROM sqlite_master WHERE name LIKE 'sqlite_stat%'`

var analyzeCreatesStat1Cases = []struct {
	name  string
	stmts []string
}{
	// The reported shape, verbatim.
	{"sqlite_master, indexed table", []string{
		`CREATE TABLE t1(aa,bb,cc,dd)`, `CREATE INDEX ix ON t1(aa,bb)`,
		`INSERT INTO t1 VALUES(1,2,3,4)`, `ANALYZE sqlite_master`}},
	// The create is unconditional: it does not depend on there being an index,
	// a row, a table, or anything at all in the schema.
	{"sqlite_master, no index", []string{`CREATE TABLE t1(a,b)`, `INSERT INTO t1 VALUES(1,2)`, `ANALYZE sqlite_master`}},
	{"sqlite_master, empty table", []string{`CREATE TABLE t1(a,b)`, `ANALYZE sqlite_master`}},
	{"sqlite_master, empty schema", []string{`ANALYZE sqlite_master`}},
	{"sqlite_schema, empty schema", []string{`ANALYZE sqlite_schema`}},
	// All four spellings that name MAIN's catalog behave identically, and the
	// name is an ordinary identifier: quoting and case are immaterial.
	{"main.sqlite_master", []string{`CREATE TABLE t1(a,b)`, `ANALYZE main.sqlite_master`}},
	{"main.sqlite_schema", []string{`CREATE TABLE t1(a,b)`, `ANALYZE main.sqlite_schema`}},
	{"double-quoted", []string{`CREATE TABLE t1(a,b)`, `ANALYZE "sqlite_master"`}},
	{"bracket-quoted", []string{`CREATE TABLE t1(a,b)`, `ANALYZE [sqlite_schema]`}},
	{"backtick-quoted", []string{`CREATE TABLE t1(a,b)`, "ANALYZE `sqlite_master`"}},
	{"upper case", []string{`CREATE TABLE t1(a,b)`, `ANALYZE SQLITE_MASTER`}},
	{"mixed case, qualified", []string{`CREATE TABLE t1(a,b)`, `ANALYZE MaIn.SqLiTe_ScHeMa`}},
	// A VIEW has nothing of its own to analyze, and had the same hole.
	{"view", []string{`CREATE TABLE t1(a,b)`, `CREATE VIEW v1 AS SELECT * FROM t1`, `ANALYZE v1`}},
	{"main.view", []string{`CREATE TABLE t1(a,b)`, `CREATE VIEW v1 AS SELECT * FROM t1`, `ANALYZE main.v1`}},
	// The whole-schema and scoped forms already created it; they are here so
	// they cannot regress while the catalog forms are added.
	{"bare, empty schema", []string{`ANALYZE`}},
	{"main, empty schema", []string{`ANALYZE main`}},
	{"empty table by name", []string{`CREATE TABLE t1(a,b)`, `ANALYZE t1`}},
}

// TestAnalyzeCreatesStat1Parity verifies every accepted ANALYZE creates sqlite_stat1.
func TestAnalyzeCreatesStat1Parity(t *testing.T) {
	probes := []string{
		statNameProbe,
		`SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name='sqlite_stat1'`,
		`SELECT count(*) FROM sqlite_stat1`,
		// The corpus's own idiom: hand-seed stats into the table ANALYZE
		// created, then read them back.
		`INSERT INTO sqlite_stat1 VALUES('t1','ix','2695116 1347558')`,
		`SELECT tbl, idx, stat FROM sqlite_stat1`,
	}
	for _, tc := range analyzeCreatesStat1Cases {
		edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range tc.stmts {
			eerr := edb.Exec(s)
			_, cerr := cdb.Exec(s)
			if (eerr == nil) != (cerr == nil) {
				t.Errorf("[%s] %q accept/reject disagrees\n  engine=%v\n  cgo=%v", tc.name, s, eerr, cerr)
			}
		}
		for _, q := range probes {
			// The INSERT is a write on both sides, not a query.
			if q[0] == 'I' {
				eerr := edb.Exec(q)
				_, cerr := cdb.Exec(q)
				if (eerr == nil) != (cerr == nil) {
					t.Errorf("[%s] %q accept/reject disagrees\n  engine=%v\n  cgo=%v", tc.name, q, eerr, cerr)
				}
				continue
			}
			p, err := edb.SnapshotPager()
			if err != nil {
				t.Fatalf("[%s] %v", tc.name, err)
			}
			ecols, ev, eerr := p.QueryArgs(q, nil)
			ccols, cr, cerr := cgoSelect(t, cdb, q, nil)
			if (eerr == nil) != (cerr == nil) {
				t.Errorf("[%s] %q accept/reject disagrees\n  engine=%v\n  cgo=%v", tc.name, q, eerr, cerr)
				continue
			}
			if eerr != nil {
				continue
			}
			eRows := engineRowsToStrings(ev)
			if ok, reason := queryResultsMatch(ecols, eRows, ccols, cr, false); !ok {
				t.Errorf("[%s] %q DIVERGES: %s\n  engine: %v\n  cgo:    %v", tc.name, q, reason, eRows, cr)
			}
		}
		edb.Close()
		cdb.Close()
	}
}

// TestAnalyzeStat1Preserved verifies "ANALYZE sqlite_master" creates the table
// but doesn't compute or erase rows.
func TestAnalyzeStat1Preserved(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a,b,c)`, `CREATE INDEX ix ON t1(a)`,
		`INSERT INTO t1 VALUES(1,2,3),(4,5,6)`, `ANALYZE`,
	}
	steps := []string{
		`ANALYZE sqlite_master`,
		`INSERT INTO sqlite_stat1 VALUES('zz','zz','999 999')`,
		`ANALYZE sqlite_schema`,
		`ANALYZE main.sqlite_master`,
	}
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
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	q := `SELECT tbl, ifnull(idx,'<NULL>'), stat FROM sqlite_stat1 ORDER BY tbl, 2`
	for _, s := range steps {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Fatalf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eerr, cerr)
		}
		p, err := edb.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		ecols, ev, eerr := p.QueryArgs(q, nil)
		if eerr != nil {
			t.Fatalf("after %q: %v", s, eerr)
		}
		ccols, cr, _ := cgoSelect(t, cdb, q, nil)
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch(ecols, eRows, ccols, cr, true); !ok {
			t.Errorf("after %q, sqlite_stat1 DIVERGES: %s\n  engine: %v\n  cgo:    %v", s, reason, eRows, cr)
		}
	}
}

// TestAnalyzeTargetAcceptParity verifies which ANALYZE arguments are accepted
// once schema qualifiers are honored.
func TestAnalyzeTargetAcceptParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a,b)`, `CREATE INDEX ix ON t1(a)`,
		`CREATE VIEW v1 AS SELECT * FROM t1`, `INSERT INTO t1 VALUES(1,2)`,
		`CREATE TEMP TABLE tt(x)`,
	}
	targets := []string{
		// Every database has a catalog answering to sqlite_master/sqlite_schema.
		`main.sqlite_master`, `main.sqlite_schema`,
		`temp.sqlite_master`, `temp.sqlite_schema`,
		// sqlite_temp_master/sqlite_temp_schema name the TEMP catalog
		// specifically, so they resolve nowhere under "main.".
		`temp.sqlite_temp_master`, `temp.sqlite_temp_schema`,
		`main.sqlite_temp_master`, `main.sqlite_temp_schema`,
		// An object resolves only in its own catalog.
		`main.t1`, `main.ix`, `main.v1`, `temp.tt`, `tt`,
		`temp.t1`, `temp.ix`, `temp.v1`, `main.tt`,
		// And the long-standing rejections.
		`nosuchtable`, `nosuchschema.x`, `main.nosuch`,
	}
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
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	for _, tg := range targets {
		s := `ANALYZE ` + tg
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eerr, cerr)
		}
	}
}

// TestAnalyzeIgnoresTempObjects verifies a MAIN-schema ANALYZE records only
// MAIN objects, not temp tables.
func TestAnalyzeIgnoresTempObjects(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a,b)`, `CREATE INDEX ix ON t1(a)`,
		`INSERT INTO t1 VALUES(1,2),(3,4)`,
		`CREATE TEMP TABLE tt(x)`, `CREATE INDEX tix ON tt(x)`,
		`INSERT INTO tt VALUES(1),(2),(3)`,
		`ANALYZE`,
	}
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
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	q := `SELECT tbl, ifnull(idx,'<NULL>'), stat FROM sqlite_stat1 ORDER BY tbl, 2`
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	ecols, ev, eerr := p.QueryArgs(q, nil)
	if eerr != nil {
		t.Fatal(eerr)
	}
	ccols, cr, cerr := cgoSelect(t, cdb, q, nil)
	if cerr != nil {
		t.Fatal(cerr)
	}
	eRows := engineRowsToStrings(ev)
	if ok, reason := queryResultsMatch(ecols, eRows, ccols, cr, true); !ok {
		t.Errorf("sqlite_stat1 DIVERGES: %s\n  engine: %v\n  cgo:    %v", reason, eRows, cr)
	}
}
