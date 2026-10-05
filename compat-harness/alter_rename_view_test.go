// This file tests ALTER TABLE RENAME cascading to dependent views.
// Views must be rewritten when table or column names change.
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestAlterTableRenameRewritesViews(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"RENAME COLUMN rewrites a view that spells the column out", []string{
			`CREATE TABLE t1(a INTEGER, b TEXT)`,
			`CREATE VIEW v1 AS SELECT a, b FROM t1`,
			`ALTER TABLE t1 RENAME b TO bbb`,
			`SELECT sql FROM sqlite_master WHERE name='v1'`,
			`SELECT sql FROM sqlite_master WHERE name='t1'`,
			`INSERT INTO t1 VALUES(1,2)`,
			`SELECT * FROM v1`,
		}},
		{"RENAME COLUMN leaves a view untouched when it never mentions the column", []string{
			`CREATE TABLE rr(a, b)`,
			`CREATE VIEW vv AS SELECT * FROM rr`,
			`INSERT INTO rr VALUES(1,2)`,
			`ALTER TABLE rr RENAME a TO c`,
			`SELECT sql FROM sqlite_master WHERE name='vv'`,
			`SELECT sql FROM sqlite_master WHERE name='rr'`,
			`SELECT * FROM vv`,
		}},
		{"RENAME TO rewrites a view's FROM clause, quoting the new name", []string{
			`CREATE TABLE t1(a INTEGER, b TEXT)`,
			`CREATE VIEW v1 AS SELECT * FROM t1`,
			`ALTER TABLE t1 RENAME TO t2`,
			`SELECT sql FROM sqlite_master WHERE name='v1'`,
			`SELECT sql FROM sqlite_master WHERE name='t2'`,
		}},
		// altermalloc2.test segment #4's own shape: the table reference is
		// nested inside a WITH clause inside a further-nested FROM subquery.
		{"RENAME TO rewrites a table reference nested inside a view's own WITH clause", []string{
			`CREATE TABLE rr(a, b)`,
			`CREATE VIEW vv AS SELECT * FROM ( WITH abc(d, e) AS (SELECT * FROM rr) SELECT * FROM abc )`,
			`ALTER TABLE rr RENAME TO c`,
			`SELECT sql FROM sqlite_master WHERE name='vv'`,
			`SELECT sql FROM sqlite_master WHERE name='c'`,
			`SELECT * FROM vv`,
		}},
		// altertab.test 19.100 (ticket f50af3e8a565776b): the renamed table
		// appears THREE times in the view's FROM clause (bare, and twice
		// inside a parenthesized join group), all three colliding on the new
		// name after rewrite -- but the view's own select list ("SELECT 1")
		// never resolves a single column, so C SQLite accepts it outright
		// (see engine/alter_write.go's viewRenameAmbiguityExempt).
		{"RENAME TO leaves an unqualified self-join-by-name view accepted when its select list touches no column", []string{
			`CREATE TABLE t1(x)`,
			`CREATE VIEW t2 AS SELECT 1 FROM t1, (t1 AS a0, t1)`,
			`ALTER TABLE t1 RENAME TO t3`,
			`SELECT sql FROM sqlite_master`,
			`INSERT INTO t3(x) VALUES(123)`,
			`SELECT * FROM t2`,
			`INSERT INTO t3(x) VALUES('xyz')`,
			`SELECT * FROM t2`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "alterviews/"+tc.name, tc.stmts) })
	}
}
