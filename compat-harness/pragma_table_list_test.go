package compat

// This file gates PRAGMA table_list -- both the bare pragma and its
// pragma_table_list(...) table-valued wrapper -- against C SQLite. See
// engine/pragma_table_list.go for every rule and the oracle evidence.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// tableListFixture has NO virtual table at all: rtree is this engine's one
// "private-store" module (no real shadow tables persisted at all), which pins
// the FULL/bare listing to decline whenever it is present -- see
// pragma_table_list.go's package doc comment. Every case here that exercises
// the full listing needs a schema this engine can fully represent, so the
// virtual-table shapes are gated separately (TestPragmaVtabDeclinesStayDeclined
// already pins that a schema WITH one still declines the full listing).
var tableListFixture = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT)`,
	`CREATE TABLE wr(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
	`CREATE TABLE st(a INT, b TEXT NOT NULL) STRICT`,
	`CREATE TABLE wrst(k INT, v TEXT, PRIMARY KEY(k,v)) WITHOUT ROWID, STRICT`,
	`CREATE TABLE g(a INTEGER PRIMARY KEY, v INT GENERATED ALWAYS AS (a+1), c TEXT, s AS (a*2) STORED, d INT)`,
	`CREATE VIEW v1 AS SELECT a, b FROM t1`,
	`CREATE VIEW vstar AS SELECT * FROM t1`,
	`CREATE VIEW vnamed(p,q) AS SELECT a,b FROM t1`,
	`CREATE TEMP TABLE tt(x, y)`,
}

func tableListCase(t *testing.T, q string) {
	t.Helper()
	if !differ(t, "tablelist/"+q, append(append([]string(nil), tableListFixture...), q)) {
		t.Errorf("diverged on: %s", q)
	}
}

// TestPragmaTableListMatchesCSQLite gates the wrapped pragma_table_list():
// single-name lookups (strict1.test 2.0a, view.test 1.1.120's own shapes),
// the WHERE-filtered form (auth.test 1.359), and the sorted full listing --
// pinned with an explicit ORDER BY because the bare/unqualified listing's row
// order is C SQLite's own internal schema hash-table iteration and is not
// reproducible here (see the package doc comment).
func TestPragmaTableListMatchesCSQLite(t *testing.T) {
	for _, q := range []string{
		// strict1.test 2.0a: "SELECT strict FROM pragma_table_list('t1');" {1}
		`SELECT strict FROM pragma_table_list('t1')`,
		`SELECT strict FROM pragma_table_list('st')`,
		`SELECT wr FROM pragma_table_list('t1')`,
		`SELECT wr FROM pragma_table_list('wr')`,
		`SELECT wr, strict FROM pragma_table_list('wrst')`,
		// view.test 1.1.120: "SELECT name, type FROM pragma_table_list('v1');" {v1 view}
		`SELECT name, type FROM pragma_table_list('v1')`,
		`SELECT * FROM pragma_table_list('vstar')`,
		`SELECT * FROM pragma_table_list('vnamed')`,
		// ncol counts every DECLARED column, generated ones included.
		`SELECT * FROM pragma_table_list('g')`,
		// auth.test 1.359: "SELECT * FROM pragma_table_list WHERE name='xyzzy';" {0 {}}
		`SELECT * FROM pragma_table_list WHERE name='xyzzy'`,
		`SELECT * FROM pragma_table_list WHERE name='t1'`,
		// The schema catalog alias: only the "_master" spelling resolves,
		// never the modern "_schema" one (verified directly, and deliberately
		// NOT the same rule table_info's isMainSchemaCatalogName follows).
		`SELECT * FROM pragma_table_list('sqlite_master')`,
		`SELECT * FROM pragma_table_list('sqlite_schema')`,
		`SELECT * FROM pragma_table_list('sqlite_temp_master')`,
		`SELECT * FROM pragma_table_list('sqlite_temp_schema')`,
		// A name that does not exist at all is zero rows, not an error.
		`SELECT * FROM pragma_table_list('nosuchtable')`,
		// The temp catalog: unqualified searches temp first, 'temp' scopes to
		// it, 'main' excludes it.
		`SELECT * FROM pragma_table_list('tt')`,
		`SELECT * FROM pragma_table_list('tt','temp')`,
		`SELECT * FROM pragma_table_list('tt','main')`,
		// Two call arguments is a hard error: table_list has NO schema
		// argument at all, unlike every other wrapped pragma (its own first
		// result column is already named "schema").
		`SELECT * FROM pragma_table_list('t1','main')`,
		// The hidden "arg" column, and its WHERE spelling.
		`SELECT arg FROM pragma_table_list('t1')`,
		`SELECT typeof(arg) FROM pragma_table_list('t1')`,
		`SELECT typeof(arg) FROM pragma_table_list LIMIT 1`,
		`SELECT * FROM pragma_table_list WHERE arg='t1'`,
		// No argument, an explicit NULL, and the bare pragma spelling are all
		// the SAME full listing -- SORTED, per this test's own doc comment.
		`SELECT schema, name, type, ncol, wr, strict FROM pragma_table_list ORDER BY schema, name`,
		`SELECT schema, name, type, ncol, wr, strict FROM pragma_table_list(NULL) ORDER BY schema, name`,
		`SELECT schema, name, type, ncol, wr, strict FROM pragma_table_list WHERE schema='main' ORDER BY name`,
		`SELECT count(*) FROM pragma_table_list`,
	} {
		tableListCase(t, q)
	}
}

// TestPragmaTableListAttached gates the cross-database rules fkey5.test's own
// wrapping needed: an explicit ATTACHed qualifier restricts the listing to
// exactly that database (no temp row, no other attachment), and an
// unqualified single name searches every attached database, main first
// (attach_write.go's pragmaObjectOwner -- the same rule
// table_info/index_list/foreign_key_list already apply).
func TestPragmaTableListAttached(t *testing.T) {
	base := []string{
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE aux.a1(p, q, r)`,
	}
	for _, q := range []string{
		`SELECT * FROM pragma_table_list('a1')`,
		`PRAGMA aux.table_list('a1')`,
		`PRAGMA main.table_list('a1')`,
		`SELECT schema, name, type, ncol, wr, strict FROM pragma_table_list ORDER BY schema, name`,
		`SELECT schema, name, type, ncol, wr, strict FROM pragma_table_list WHERE schema='aux' ORDER BY name`,
	} {
		stmts := append(append([]string(nil), base...), q)
		if !differ(t, "tablelistattach/"+q, stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
