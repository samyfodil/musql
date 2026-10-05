// Writes into ATTACHed databases tested against C SQLite.
package compat

import "testing"

var xdbCases = []struct {
	name  string
	stmts []string
}{
	{"create-and-insert-into-attached", []string{
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.t(a, b)`,
		`INSERT INTO aux.t VALUES(1,'x'),(2,'y')`,
		`SELECT a, b FROM aux.t ORDER BY a`,
		`SELECT count(*) FROM aux.t`,
		`UPDATE aux.t SET b='z' WHERE a=1`,
		`SELECT a,b FROM aux.t ORDER BY a`,
		`DELETE FROM aux.t WHERE a=2`,
		`SELECT a,b FROM aux.t ORDER BY a`,
		`SELECT name FROM aux.sqlite_master ORDER BY name`,
	}},
	{"index-view-and-drop-in-attached", []string{
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.t(a, b)`,
		`INSERT INTO aux.t VALUES(3,'c'),(1,'a'),(2,'b')`,
		`CREATE INDEX aux.ti ON t(a)`,
		`CREATE VIEW aux.v AS SELECT a FROM t`,
		`SELECT a FROM aux.v ORDER BY a`,
		`SELECT type, name FROM aux.sqlite_master ORDER BY name`,
		`DROP VIEW aux.v`,
		`DROP INDEX aux.ti`,
		`SELECT type, name FROM aux.sqlite_master ORDER BY name`,
		`DROP TABLE aux.t`,
		`SELECT count(*) FROM aux.sqlite_master`,
	}},
	{"main-and-attached-are-independent", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.t(a)`,
		`INSERT INTO aux.t VALUES(99)`,
		`SELECT a FROM t`,
		`SELECT a FROM aux.t`,
		`SELECT a FROM main.t`,
	}},
	{"alter-in-attached", []string{
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.t(a)`,
		`INSERT INTO aux.t VALUES(1)`,
		`ALTER TABLE aux.t ADD COLUMN b DEFAULT 7`,
		`SELECT a, b FROM aux.t`,
		`ALTER TABLE aux.t RENAME TO t2`,
		`SELECT a, b FROM aux.t2`,
		`SELECT name FROM aux.sqlite_master ORDER BY name`,
	}},
	{"unknown-database-still-errors", []string{
		`CREATE TABLE nope.t(a)`,
		`INSERT INTO nope.t VALUES(1)`,
		`SELECT 1`,
	}},
}

func TestCrossDatabaseWritesMatchCSQLite(t *testing.T) {
	for _, tc := range xdbCases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "xdb/"+tc.name, tc.stmts) })
	}
}
