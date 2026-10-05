//go:build sqlite_fts5

// Fts5_tablefunc_test verifies FTS5 table-valued function rewrites to MATCH
// correctly.
package compat

import (
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestFts5InitialTokenOperator verifies the FTS5 initial-token operator works
// correctly.
func TestFts5InitialTokenOperator(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts5(a, b)`,
		`INSERT INTO t1(rowid,a,b) VALUES(1,'a b c','x')`,
		`INSERT INTO t1(rowid,a,b) VALUES(2,'c b a','x')`,
		`INSERT INTO t1(rowid,a,b) VALUES(3,'b a','a')`,
		`INSERT INTO t1(rowid,a,b) VALUES(4,'b','a c')`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("%s setup: %v", drv, err)
		}
	}
	for _, q := range []string{
		// Anchored at position 0 of SOME column: row 2 has 'a' last, so it is
		// excluded; rows 3 and 4 match on their SECOND column.
		`SELECT rowid FROM t1 WHERE t1 MATCH '^a' ORDER BY rowid`,
		`SELECT rowid FROM t1 WHERE t1 MATCH '^b' ORDER BY rowid`,
		`SELECT rowid FROM t1 WHERE t1 MATCH '^c' ORDER BY rowid`,
		`SELECT rowid FROM t1 WHERE t1 MATCH '^a OR ^b' ORDER BY rowid`,
		`SELECT rowid FROM t1 WHERE t1 MATCH '^a AND b' ORDER BY rowid`,
		`SELECT rowid FROM t1 WHERE t1 MATCH 'a ^b' ORDER BY rowid`,
		// A column filter narrows which column the anchor applies to.
		`SELECT rowid FROM t1 WHERE t1 MATCH 'a:^a' ORDER BY rowid`,
		`SELECT rowid FROM t1 WHERE t1 MATCH 'b:^a' ORDER BY rowid`,
		// Combines with a quoted phrase and with a prefix term.
		`SELECT rowid FROM t1 WHERE t1 MATCH '^"a b"' ORDER BY rowid`,
		`SELECT rowid FROM t1 WHERE t1 MATCH '^a*' ORDER BY rowid`,
		// Rejected by both: '^' is not allowed inside NEAR, and is not a
		// phrase on its own.
		`SELECT rowid FROM t1 WHERE t1 MATCH 'NEAR(^a c)' ORDER BY rowid`,
		`SELECT rowid FROM t1 WHERE t1 MATCH '^' ORDER BY rowid`,
		// ...and through the table-valued spelling, which is how it surfaced.
		`SELECT rowid FROM t1('^a OR ^b') ORDER BY rowid`,
	} {
		goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
		switch {
		case goErr != nil && cgoErr != nil:
		case goErr != nil:
			t.Errorf("this engine declined a '^' query C fts5 answers\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED a '^' query C fts5 rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("'^' DIVERGES from C fts5\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
		}
	}
}

func TestFts5TableValuedMatch(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts5(a, b)`,
		`INSERT INTO t1(rowid,a,b) VALUES(1,'one two','x y')`,
		`INSERT INTO t1(rowid,a,b) VALUES(2,'two three','y z')`,
		`INSERT INTO t1(rowid,a,b) VALUES(3,'four','z')`,
		`CREATE TABLE other(k, v)`,
		`INSERT INTO other VALUES(1,'p'),(2,'q'),(3,'r')`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("%s setup: %v", drv, err)
		}
	}

	// Answered: every one of these must match C fts5 exactly.
	for _, q := range []string{
		`SELECT * FROM t1('two')`,
		`SELECT rowid, a, b FROM t1('two')`,
		`SELECT rowid FROM t1('one AND two')`,
		`SELECT rowid FROM t1('two NOT three')`,
		`SELECT rowid FROM t1('"one two"')`,
		`SELECT rowid FROM t1('a:two')`,
		`SELECT rowid FROM t1('t*')`,
		`SELECT count(*) FROM t1('nosuchterm')`,
		`SELECT rowid FROM t1('two') ORDER BY rowid DESC`,
		`SELECT rowid FROM t1('two') WHERE rowid>1`,
		// No argument at all is a plain scan, NOT an empty query.
		`SELECT rowid FROM t1()`,
		// The MATCH must attach to the ALIAS when there is one.
		`SELECT rowid FROM t1('two') AS x`,
		`SELECT x.rowid FROM t1('two') AS x`,
		// ...and survive a join, where the MATCH placement rule bites
		// (fts5_diff_test.go's "MATCH placement in a join" group).
		`SELECT t1.rowid, other.v FROM t1('two'), other WHERE other.k=t1.rowid ORDER BY t1.rowid`,
		`SELECT t1.rowid FROM other, t1('two') WHERE other.k=t1.rowid ORDER BY t1.rowid`,
		// Nested one level down, where the rewrite is reached by execSelect
		// re-entering rather than by walking the tree.
		`SELECT rowid FROM (SELECT rowid FROM t1('two')) ORDER BY rowid`,
		`WITH c AS (SELECT rowid AS r FROM t1('two')) SELECT r FROM c ORDER BY r`,
		`SELECT rowid FROM t1('two') UNION ALL SELECT rowid FROM t1('four')`,
	} {
		goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
		switch {
		case goErr != nil:
			t.Errorf("this engine declined a table-valued MATCH C fts5 answers\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED a form C fts5 rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("table-valued MATCH DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
		}
	}

	// Declined shapes. Each is a form C SQLite treats differently from a
	// plain MATCH, so the ONLY safe outcome is that both engines reject it --
	// a silent answer here would be wrong, not merely incomplete.
	for _, q := range []string{
		// NULL is "fts5: syntax error near ''" there; a rewritten MATCH NULL
		// would quietly select no rows here.
		`SELECT rowid FROM t1(NULL)`,
		`SELECT rowid FROM t1('')`,
		// The second argument is a RANK function, not more query text.
		`SELECT rowid FROM t1('two','extra')`,
		`SELECT rowid FROM t1('(((')`,
		// A COVERAGE gap rather than a semantic one, and the only form of this
		// feature left on the table: a table-valued call inside an EXPRESSION
		// subquery. Those hold their SELECT behind an Expr node, which the
		// rewrite does not walk (engine/fts5_tablefunc.go says why). It stays
		// a clean decline -- and this loop still COMPARES the answer the day it
		// starts giving one, so it can never turn into a wrong one unnoticed.
		`SELECT count(*) FROM other WHERE k IN (SELECT rowid FROM t1('two'))`,
	} {
		goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
		if goErr == nil {
			cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
			if cgoErr != nil {
				t.Errorf("this engine ANSWERED a table-valued form C fts5 rejects\n  sql: %s\n  go:  %q\n  cgo err: %v", q, goOut, cgoErr)
			} else if goOut != cgoOut {
				t.Errorf("table-valued MATCH DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
			}
		}
	}
}
