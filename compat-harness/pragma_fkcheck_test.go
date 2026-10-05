package compat

// Tests the pragma_foreign_key_check(...) table-valued function against C SQLite,
// both for single-table and whole-database forms.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// fkCheckVtabFixture sets up multiple foreign key violations across tables
// to test both single-table and whole-database check scenarios.
var fkCheckVtabFixture = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT REFERENCES t2)`,
	`CREATE TABLE t2(x TEXT PRIMARY KEY, y INT)`,
	`CREATE TABLE t3(w TEXT, z INT REFERENCES t1)`,
	`CREATE TABLE t4(m, n)`,
	`INSERT INTO t2 VALUES('abc',11),('def',22),('xyz',99)`,
	`INSERT INTO t1 VALUES(5,'abc'),(7,'xyz'),(9,'oops')`,
	`INSERT INTO t3 VALUES(11,7),(22,19)`,
}

func fkCheckVtabCase(t *testing.T, q string) {
	t.Helper()
	if !differ(t, "fkcheckvtab/"+q, append(append([]string(nil), fkCheckVtabFixture...), q)) {
		t.Errorf("diverged on: %s", q)
	}
}

// TestPragmaForeignKeyCheckVtabMatchesCSQLite gates the wrapped
// pragma_foreign_key_check() table-valued function.
func TestPragmaForeignKeyCheckVtabMatchesCSQLite(t *testing.T) {
	for _, q := range []string{
		// fkey5.test 13.0: "SELECT *, 'x' FROM pragma_foreign_key_check('t1');" {t1 9 t2 0 x}
		`SELECT *, 'x' FROM pragma_foreign_key_check('t1')`,
		`SELECT *, 'x' FROM pragma_foreign_key_check('t3')`,
		// A table with no foreign keys, and one with no violations, are both
		// zero rows, not an error.
		`SELECT *, 'x' FROM pragma_foreign_key_check('t4')`,
		`SELECT *, 'x' FROM pragma_foreign_key_check('t2')`,
		// A name that does not exist at all is an error.
		`SELECT * FROM pragma_foreign_key_check('nosuchtable')`,
		// The explicit schema argument, both spellings C SQLite accepts.
		`SELECT *, 'x' FROM pragma_foreign_key_check('t1','main')`,
		// The hidden "arg" column, and its WHERE spelling.
		`SELECT arg FROM pragma_foreign_key_check('t1')`,
		`SELECT typeof(arg) FROM pragma_foreign_key_check('t1')`,
		`SELECT * FROM pragma_foreign_key_check WHERE arg='t1'`,
		// fkey5.test 13.11/13.12: the bare/whole-database form, gated SORTED
		// (see this file's own doc comment) -- and its NULL-argument spelling,
		// which is the identical form (verified directly).
		`SELECT *, '|' FROM pragma_foreign_key_check AS x ORDER BY x."table"`,
		`SELECT *, '|' FROM pragma_foreign_key_check(NULL) AS x ORDER BY x."table"`,
		`SELECT count(*) FROM pragma_foreign_key_check`,
	} {
		fkCheckVtabCase(t, q)
	}
}

// TestPragmaForeignKeyCheckVtabAttached gates the cross-database rules
// fkey5.test 12.0-13.2 need: an unqualified single-table argument resolves
// across every attached database, main first (fkey5.test 13.0, whose "t1"
// exists ONLY in an attached "aux"), and an explicit "aux" schema argument
// routes the whole pragma to that database's own pager (fkey5.test 13.2).
func TestPragmaForeignKeyCheckVtabAttached(t *testing.T) {
	base := []string{
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.t1(a INTEGER PRIMARY KEY, b TEXT REFERENCES t2)`,
		`CREATE TABLE main.t2(x TEXT PRIMARY KEY, y INT)`,
		`INSERT INTO main.t2 VALUES('abc',11),('def',22),('xyz',99)`,
		`INSERT INTO aux.t1 VALUES(5,'abc'),(7,'xyz'),(9,'oops')`,
	}
	for _, q := range []string{
		`SELECT *, 'x' FROM pragma_foreign_key_check('t1')`,
		`SELECT *, 'x' FROM pragma_foreign_key_check('t1','aux')`,
		`SELECT * FROM pragma_foreign_key_check('t1','main')`,
	} {
		stmts := append(append([]string(nil), base...), q)
		if !differ(t, "fkcheckvtabattach/"+q, stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
