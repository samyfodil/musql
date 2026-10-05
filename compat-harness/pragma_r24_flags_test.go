// Pragma row shape tests for legacy_alter_table, ignore_check_constraints, and data_version.
package compat

import "testing"

var pragmaR24FlagCases = []struct {
	name  string
	stmts []string
}{
	{"getters-on-a-fresh-connection", []string{
		`PRAGMA legacy_alter_table`,
		`PRAGMA ignore_check_constraints`,
		`PRAGMA main.legacy_alter_table`,
		`PRAGMA main.ignore_check_constraints`,
	}},
	{"off-setters-are-exact-no-ops", []string{
		`PRAGMA legacy_alter_table = OFF`,
		`PRAGMA legacy_alter_table`,
		`PRAGMA legacy_alter_table = 0`,
		`PRAGMA legacy_alter_table`,
		`PRAGMA legacy_alter_table = false`,
		`PRAGMA legacy_alter_table`,
		`PRAGMA legacy_alter_table(no)`,
		`PRAGMA legacy_alter_table`,
		`PRAGMA ignore_check_constraints = OFF`,
		`PRAGMA ignore_check_constraints`,
		`PRAGMA ignore_check_constraints = 0`,
		`PRAGMA ignore_check_constraints`,
	}},
	{"off-setters-leave-the-behavior-alone", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`PRAGMA ignore_check_constraints = OFF`,
		`INSERT INTO t1 VALUES(5)`,
		`SELECT count(*) FROM t1`,
		`INSERT INTO t1 VALUES(50)`,
		`SELECT a FROM t1`,
		`CREATE TABLE t2(a, b)`,
		`CREATE VIEW v2 AS SELECT a FROM t2`,
		`PRAGMA legacy_alter_table = OFF`,
		`ALTER TABLE t2 RENAME TO t2x`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
		`SELECT * FROM v2`,
	}},
	{"temp-data-version-tracks-main", []string{
		`PRAGMA data_version`,
		`PRAGMA temp.data_version`,
		`PRAGMA main.data_version`,
		`CREATE TEMP TABLE tt(a)`,
		`INSERT INTO tt VALUES(1)`,
		`PRAGMA temp.data_version`,
		`PRAGMA data_version`,
		`CREATE TABLE m(a)`,
		`INSERT INTO m VALUES(1)`,
		`PRAGMA temp.data_version`,
		`PRAGMA main.data_version`,
		`DROP TABLE tt`,
		`PRAGMA temp.data_version`,
	}},
}

func TestPragmaR24FlagRowShapesMatchCSQLite(t *testing.T) {
	for _, tc := range pragmaR24FlagCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "pragma-r24/"+tc.name, tc.stmts) })
	}
}
