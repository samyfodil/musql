//go:build sqlite_fts5

// Tests that FTS5 table-valued function calls work as INSERT ... SELECT sources.
// The table-valued form must be rewritten to a plain scan with MATCH.
package compat

import "testing"

func TestFts5TableFuncAsAnInsertSelectSource(t *testing.T) {
	differ(t, "fts5 table-valued call as an INSERT ... SELECT source", []string{
		`CREATE VIRTUAL TABLE f USING fts5(x)`,
		`INSERT INTO f VALUES('hello world'),('goodbye world'),('hello again')`,
		`CREATE TABLE t(a)`,
		`INSERT INTO t SELECT x FROM f('hello')`,
		`SELECT a FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	// The bare SELECT, which already worked -- kept beside it so a regression
	// on either half is attributable.
	differ(t, "fts5 table-valued call as a plain SELECT", []string{
		`CREATE VIRTUAL TABLE f USING fts5(x)`,
		`INSERT INTO f VALUES('hello world'),('goodbye world')`,
		`SELECT x FROM f('hello')`,
	})
	// The same source behind a WHERE and a column list, so the rewrite is not
	// only exercised in its simplest position.
	differ(t, "fts5 table-valued call as an INSERT ... SELECT source, filtered", []string{
		`CREATE VIRTUAL TABLE f USING fts5(x)`,
		`INSERT INTO f VALUES('alpha one'),('alpha two'),('beta one')`,
		`CREATE TABLE t(a TEXT)`,
		`INSERT INTO t(a) SELECT upper(x) FROM f('alpha') WHERE x LIKE '%one%'`,
		`SELECT a FROM t ORDER BY a`,
	})
}
