// This file tests single-quoted identifiers in DDL against C SQLite 3.53.3.
// FTS3/FTS4 shadow tables use this syntax; the engine must parse and resolve them correctly.
package compat

import "testing"

func TestQuotedIdentifierDDLDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"fts3 content shadow table verbatim", []string{
			`CREATE TABLE 't_content'(docid INTEGER PRIMARY KEY, 'c0a', 'c1b')`,
			`INSERT INTO t_content VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t_content(c0a) VALUES('only a')`,
			`SELECT docid, c0a, c1b FROM t_content ORDER BY docid`,
			`SELECT * FROM t_content ORDER BY docid`,
			`PRAGMA table_info(t_content)`,
			`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
		}},
		{"fts3 segdir shadow table verbatim", []string{
			`CREATE TABLE 't_segdir'(level INTEGER,idx INTEGER,start_block INTEGER,leaves_end_block INTEGER,end_block INTEGER,root BLOB,PRIMARY KEY(level, idx))`,
			`INSERT INTO t_segdir VALUES(0,0,0,0,0,x'0003616263')`,
			`SELECT level, idx, quote(root) FROM t_segdir`,
			`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
		}},
		{"quoted table name with a space", []string{
			`CREATE TABLE 'a b'(q)`,
			`INSERT INTO "a b" VALUES(7)`,
			`SELECT q FROM "a b"`,
			`SELECT name FROM sqlite_master`,
		}},
		{"quoted column name is not a string literal", []string{
			`CREATE TABLE q1('x' INTEGER PRIMARY KEY, 'y z')`,
			`INSERT INTO q1 VALUES(3,'v')`,
			`SELECT x, "y z" FROM q1`,
			`SELECT rowid, x FROM q1`,
			`PRAGMA table_info(q1)`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}
