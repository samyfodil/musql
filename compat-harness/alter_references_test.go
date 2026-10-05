// TestAlterTableRenameRewritesReferences verifies that ALTER TABLE RENAME
// rewrites REFERENCES clauses in dependent tables to maintain referential integrity.
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestAlterTableRenameRewritesReferences(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"a child table's REFERENCES follows the rename", []string{
			`CREATE TABLE p1(a PRIMARY KEY)`,
			`CREATE TABLE c1(x INTEGER PRIMARY KEY, y REFERENCES p1(a))`,
			`ALTER TABLE p1 RENAME TO ppp`,
			`SELECT sql FROM sqlite_master WHERE name='c1'`,
			`SELECT sql FROM sqlite_master WHERE name='ppp'`,
			`PRAGMA foreign_key_list(c1)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		{"every REFERENCES form follows it", []string{
			`CREATE TABLE p(a PRIMARY KEY, b)`,
			`CREATE TABLE c1(x REFERENCES p)`,
			`CREATE TABLE c2(x REFERENCES p(a))`,
			`CREATE TABLE c3(x, FOREIGN KEY(x) REFERENCES p(a) ON DELETE CASCADE)`,
			`CREATE TABLE c4(x REFERENCES p(a) ON UPDATE SET NULL, y REFERENCES p(b))`,
			`ALTER TABLE p RENAME TO renamed`,
			`SELECT name, sql FROM sqlite_master WHERE type='table' ORDER BY name`,
			`PRAGMA foreign_key_list(c3)`,
			`PRAGMA foreign_key_list(c4)`,
		}},
		{"a self-referential key follows it", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, parent REFERENCES t(a))`,
			`ALTER TABLE t RENAME TO tt`,
			`SELECT sql FROM sqlite_master WHERE name='tt'`,
			`PRAGMA foreign_key_list(tt)`,
		}},
		{"only the token after REFERENCES is rewritten", []string{
			`CREATE TABLE p1(a PRIMARY KEY)`,
			`CREATE TABLE c1(p1 INTEGER, y REFERENCES p1(a))`,
			`ALTER TABLE p1 RENAME TO ppp`,
			`SELECT sql FROM sqlite_master WHERE name='c1'`,
			`PRAGMA foreign_key_list(c1)`,
			`PRAGMA table_info(c1)`,
		}},
		{"the renamed parent still enforces the key", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p1(a PRIMARY KEY)`,
			`CREATE TABLE c1(x INTEGER PRIMARY KEY, y REFERENCES p1(a))`,
			`INSERT INTO p1 VALUES(1)`,
			`ALTER TABLE p1 RENAME TO ppp`,
			`INSERT INTO c1 VALUES(10, 1)`,
			`INSERT INTO c1 VALUES(11, 99)`,
			`SELECT x, y FROM c1 ORDER BY x`,
			`DELETE FROM ppp WHERE a=1`,
			`SELECT count(*) FROM ppp`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "alterrefs/"+tc.name, tc.stmts) })
	}
}
