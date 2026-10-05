// This file gates CREATE INDEX key/WHERE expressions for differential testing.
// Index keys follow different rules from WHERE clauses: qualified references
// are forbidden in keys but allowed in WHERE; double-quoted non-column
// identifiers are treated as string constants.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestWpathR24IndexKeyExpressions pins the index KEY list rules.
func TestWpathR24IndexKeyExpressions(t *testing.T) {
	for _, c := range []struct {
		name string
		key  string
	}{
		{"null-key", `NULL`},
		{"dquoted-nocolumn", `"y"`},
		{"dquoted-nocolumn-star", `"y*"`},
		{"dquoted-real-column", `"b"`},
		{"unary-dquoted", `+"y"`},
		{"unary-string", `+'z'`},
		{"bare-string", `'z'`},
		{"bracket-nocolumn", `[y]`},
		{"backtick-nocolumn", "`y`"},
		{"qualified-key", `t1.b`},
		{"qualified-key-3part", `main.t1.b`},
		{"expr-with-qualified", `b+t1.b`},
		{"bare-nocolumn", `y`},
	} {
		flLockstep(t, "index-key-"+c.name, []string{
			`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT)`,
			`INSERT INTO t1(a,b) VALUES(1,0),(2,7),(3,7)`,
			`CREATE INDEX ix ON t1(` + c.key + `)`,
			// Verify index works with subsequent writes.
			`INSERT INTO t1(a,b) VALUES(4,9)`,
			`UPDATE t1 SET b=b+1 WHERE a=2`,
			`DELETE FROM t1 WHERE a=3`,
		}, `SELECT * FROM t1 ORDER BY a`, `SELECT count(*) FROM t1 WHERE b=8`,
			`SELECT name FROM sqlite_master WHERE type='index' ORDER BY name`)
	}
}

// TestWpathR24PartialIndexWhereQualifier verifies partial-index WHERE clauses allow qualified references.
func TestWpathR24PartialIndexWhereQualifier(t *testing.T) {
	for _, c := range []struct {
		name  string
		where string
	}{
		{"unqualified", `b BETWEEN 5 AND 10`},
		{"unknown-table", `nosuchtable.a > 1`},
		{"bogus-db-unknown-table", `xyzzy.nosuchtab.a > 1`},
	} {
		flLockstep(t, "partial-where-"+c.name, []string{
			`CREATE TABLE t3(a,b,c)`,
			`CREATE TABLE t4(a,b,c)`,
			`INSERT INTO t3 VALUES(1,5,1),(2,6,2),(3,7,3),(4,20,4)`,
			`CREATE INDEX t3b ON t3(b) WHERE ` + c.where,
			`INSERT INTO t3 VALUES(5,8,5),(6,90,6)`,
			`UPDATE t3 SET b=9 WHERE a=1`,
			`DELETE FROM t3 WHERE a=2`,
		}, `SELECT * FROM t3 ORDER BY a`,
			`SELECT count(*) FROM t3 WHERE t3.b BETWEEN 5 AND 10`,
			`SELECT name FROM sqlite_master WHERE type='index' ORDER BY name`)
	}
}

// TestWpathR24PartialIndexOwnTableQualifierRenames verifies that index WHERE
// qualifiers are rewritten when the indexed table is renamed.
func TestWpathR24PartialIndexOwnTableQualifierRenames(t *testing.T) {
	const want = `CREATE INDEX t2expr ON "t2new"(a) WHERE "t2new".b>0`
	script := []string{
		`CREATE TABLE t2(a,b)`,
		`CREATE INDEX t2expr ON t2(a) WHERE t2.b>0`,
		`ALTER TABLE t2 RENAME TO t2new`,
	}

	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	for _, where := range []string{`t2.b>0`, `main.t2.b>0`, `xyzzy.t2.b>0`} {
		if e := edb.Exec(`CREATE TABLE q(a,b)`); e != nil {
			t.Fatalf("create: %v", e)
		}
		if e := edb.Exec(`CREATE INDEX qexpr ON q(a) WHERE ` + strings.Replace(where, "t2.", "q.", 1)); e != nil {
			t.Errorf("partial-index WHERE %q was REJECTED: %v", where, e)
		}
		if e := edb.Exec(`DROP TABLE q`); e != nil {
			t.Fatalf("drop: %v", e)
		}
	}
	for _, s := range script {
		if e := edb.Exec(s); e != nil {
			t.Fatalf("engine %s: %v", s, e)
		}
	}
	p, perr := edb.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	defer p.Close()
	_, rows, qerr := p.QueryArgs(`SELECT sql FROM sqlite_master WHERE name='t2expr'`, nil)
	if qerr != nil {
		t.Fatalf("engine sql: %v", qerr)
	}
	// Verify engine stores the index SQL correctly.
	if got := engineRowsToStrings(rows); len(got) != 1 || len(got[0]) != 1 || got[0][0] != "T:"+want {
		t.Errorf("engine stored index SQL after rename = %v, want %q", got, "T:"+want)
	}

	// Verify C SQLite rewrites the qualifier.
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	for _, s := range script {
		if _, e := cdb.Exec(s); e != nil {
			t.Fatalf("cgo %s: %v", s, e)
		}
	}
	var got string
	if e := cdb.QueryRow(`SELECT sql FROM sqlite_master WHERE name='t2expr'`).Scan(&got); e != nil {
		t.Fatalf("cgo sql: %v", e)
	}
	if got != want {
		t.Errorf("oracle stored index SQL after rename = %q, want %q -- if the qualifier is no longer rewritten, this rule has to be re-derived", got, want)
	}
}

// TestWpathR24IndexReopens verifies that indexes with double-quoted non-column
// keys can be reopened without errors.
func TestWpathR24IndexReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT)`,
		`INSERT INTO t1(a,b) VALUES(1,10),(2,20)`,
		`CREATE INDEX x1 ON t1("y")`,
		`CREATE INDEX x4 ON t1("y*")`,
		`CREATE INDEX i0 ON t1(NULL)`,
		`CREATE INDEX x6 ON t1("b")`,
		`CREATE INDEX t1p ON t1(b) WHERE b > 15`,
	} {
		if e := edb.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	if e := edb.Close(); e != nil {
		t.Fatalf("Close: %v", e)
	}

	edb2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite after creating a double-quoted-string index: %v", err)
	}
	if e := edb2.Exec(`INSERT INTO t1(a,b) VALUES(3,30)`); e != nil {
		t.Fatalf("insert after reopen: %v", e)
	}
	if e := edb2.Close(); e != nil {
		t.Fatalf("Close 2: %v", e)
	}

	// Test with C SQLite via exported format.
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	var ic string
	if e := cdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); e != nil {
		t.Fatalf("integrity_check: %v", e)
	}
	if ic != "ok" {
		t.Errorf("C SQLite integrity_check on the reopened file = %q, want ok", ic)
	}
	var n int
	if e := cdb.QueryRow(`SELECT count(*) FROM t1 WHERE b>15`).Scan(&n); e != nil {
		t.Fatalf("count: %v", e)
	}
	if n != 2 {
		t.Errorf("C SQLite reads %d rows with b>15, want 2", n)
	}
}
