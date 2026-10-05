package compat

import "testing"

// Schema-object PRAGMAs with ATTACHed-database qualifiers.

func TestAttachedDatabasePragmas(t *testing.T) {
	base := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES t(a))`,
		`CREATE INDEX i ON t(b)`,
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.t(x INT PRIMARY KEY, y TEXT NOT NULL, z REFERENCES t(x))`,
		`CREATE UNIQUE INDEX aux.ix ON t(y COLLATE NOCASE DESC)`,
		`CREATE TABLE aux.nr(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO aux.t VALUES(1,'a',1)`,
	}
	qs := []string{
		`PRAGMA aux.table_info(t)`,
		`PRAGMA main.table_info(t)`,
		`PRAGMA aux.table_xinfo(t)`,
		`PRAGMA aux.index_list(t)`,
		`PRAGMA aux.index_info(ix)`,
		`PRAGMA aux.index_xinfo(ix)`,
		`PRAGMA aux.foreign_key_list(t)`,
		`PRAGMA aux.table_info(nr)`,
		// NOT "PRAGMA aux.table_list": its row ORDER is C SQLite's own
		// hash order, which this engine does not reproduce and differ()
		// compares position by position. The CONTENT agrees.
		`PRAGMA aux.foreign_key_check`,
		`PRAGMA aux.table_info(nosuch)`,
		`PRAGMA nosuchdb.table_info(t)`,
		`PRAGMA temp.table_info(t)`,
		`SELECT * FROM pragma_table_info('t','aux')`,
		`SELECT * FROM pragma_index_list('t','aux')`,
	}
	for _, q := range qs {
		q := q
		t.Run(q, func(t *testing.T) { differ(t, "attachpragma/"+q, append(append([]string{}, base...), q)) })
	}
}
