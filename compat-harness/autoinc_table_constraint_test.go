package compat

import "testing"

// TestAutoincrementTableConstraintMatches verifies that table-constraint
// AUTOINCREMENT spelling works like the inline form, including sqlite_sequence
// management and high-water behavior.
func TestAutoincrementTableConstraintMatches(t *testing.T) {
	differ(t, "autoinc-table-constraint", []string{
		`CREATE TABLE t7(x INTEGER, y REAL, PRIMARY KEY(x AUTOINCREMENT))`,
		`INSERT INTO t7(y) VALUES(1.5)`,
		`INSERT INTO t7(y) VALUES(2.5)`,
		`SELECT x, y, rowid FROM t7 ORDER BY x`,
		`SELECT name, seq FROM sqlite_sequence`,
		`SELECT sql FROM sqlite_master WHERE name='t7'`,
		`DELETE FROM t7`,
		`INSERT INTO t7(y) VALUES(9.5)`,
		`SELECT x FROM t7`,
		`CREATE TABLE ok6(x InTeGeR, PRIMARY KEY(x ASC AUTOINCREMENT))`,
		`INSERT INTO ok6(x) VALUES(NULL)`,
		`SELECT x FROM ok6`,
		`CREATE TABLE bad1(x TEXT, PRIMARY KEY(x AUTOINCREMENT))`,
		`CREATE TABLE bad2(x INTEGER, y INTEGER, PRIMARY KEY(x, y AUTOINCREMENT))`,
		`CREATE TABLE bad4(x INT, PRIMARY KEY(x AUTOINCREMENT))`,
		`CREATE TABLE bad5(x, PRIMARY KEY(x AUTOINCREMENT))`,
		`CREATE TABLE bad8(x INTEGER, PRIMARY KEY(x AUTOINCREMENT)) WITHOUT ROWID`,
		`SELECT name FROM sqlite_master ORDER BY name`,
	})
}
