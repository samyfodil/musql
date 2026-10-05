//go:build sqlite_fts5

package compat

import "testing"

// TestR33RFts5ExternalContentRename tests ALTER TABLE RENAME on external-content fts5 tables.
func TestR33RFts5ExternalContentRename(t *testing.T) {
	if !harnessFTS5 {
		t.Skip("fts5 is absent from both engines in this build")
	}
	for _, c := range []struct {
		name   string
		script []string
		verify []string
	}{
		{"external content", []string{
			`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
			`INSERT INTO src VALUES(1,'hello world','x'),(2,'goodbye world','y')`,
			`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
			`INSERT INTO t(t) VALUES('rebuild')`,
			`ALTER TABLE t RENAME TO t2`,
		}, []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
			`SELECT rowid, a, b FROM t2 ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM t2 WHERE t2 MATCH 'world' ORDER BY rowid)`,
		}},
		// With columnsize=0 flag.
		{"columnsize=0", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, columnsize=0)`,
			`INSERT INTO t VALUES('alpha'),('beta')`,
			`ALTER TABLE t RENAME TO t2`,
		}, []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
			`SELECT rowid, a FROM t2 ORDER BY rowid`,
		}},
		// Ordinary fts5 table with normal content mode.
		{"normal content", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t VALUES('hello world','x')`,
			`ALTER TABLE t RENAME TO t2`,
			`INSERT INTO t2(t2) VALUES('integrity-check')`,
		}, []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
			`SELECT rowid, a, b FROM t2 ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM t2 WHERE t2 MATCH 'world' ORDER BY rowid)`,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			flLockstep(t, c.name, c.script, c.verify...)
		})
	}
}
