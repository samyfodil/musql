// This file tests that PRAGMA page_count and freelist_count match C SQLite.
// The engine retains released pages on a freelist like C SQLite does, and
// allows C SQLite to walk the freelist chain and reuse pages.
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// freelistPrograms tests that verify page and freelist counts match C SQLite.
var freelistPrograms = []struct {
	name  string
	stmts []string
}{
	{"a fresh table frees nothing", []string{`CREATE TABLE t(a)`}},
	{"a row frees nothing", []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`}},
	{"an index frees nothing", []string{`CREATE TABLE t(a)`, `CREATE INDEX ti ON t(a)`}},
	{"an overflow chain frees nothing", []string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,zeroblob(9000))`}},
	{"emptying a table frees nothing", []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1),(2),(3)`, `DELETE FROM t`}},
	{"DROP TABLE frees a page", []string{`CREATE TABLE t(a)`, `CREATE TABLE u(b)`, `DROP TABLE t`}},
	{"DROP TABLE with rows and an index frees a page", []string{
		`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`, `INSERT INTO t VALUES(2)`,
		`CREATE TABLE u(b)`, `CREATE INDEX ti ON t(a)`, `DROP TABLE u`}},
	{"two DROPs free two pages", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE u(b)`, `CREATE TABLE v(c)`,
		`DROP TABLE t`, `DROP TABLE u`}},
	// Freed pages are reused by later CREATEs.
	{"a later CREATE reuses the freed page", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE u(b)`, `DROP TABLE t`, `CREATE TABLE w(c)`}},
	{"a later CREATE reuses two freed pages", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE u(b)`, `CREATE TABLE v(c)`,
		`DROP TABLE t`, `DROP TABLE u`, `CREATE TABLE w(c)`, `CREATE TABLE x(d)`}},
	// VACUUM removes freed pages.
	{"a VACUUM gives the freed pages back", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE u(b)`, `DROP TABLE t`, `VACUUM`}},
	{"a VACUUM gives two freed pages back", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE u(b)`, `CREATE TABLE v(c)`,
		`INSERT INTO u VALUES(1),(2)`, `DROP TABLE t`, `DROP TABLE v`, `VACUUM`}},
	{"dropping an index frees its page", []string{
		`CREATE TABLE t(a)`, `CREATE INDEX ti ON t(a)`, `DROP INDEX ti`}},
	{"a view and a trigger free nothing", []string{
		`CREATE TABLE t(a)`, `CREATE VIEW v AS SELECT a FROM t`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`, `DROP VIEW v`}},
	// Growing one table in a wide schema frees nothing overall.
	{"a wide schema growing one table frees nothing", []string{
		`CREATE TABLE t0(a)`, `CREATE TABLE t1(a)`, `CREATE TABLE t2(a)`,
		`CREATE TABLE t3(a)`, `CREATE TABLE t4(a)`, `CREATE TABLE t5(a)`,
		`INSERT INTO t0 VALUES(1)`, `INSERT INTO t0 VALUES(2)`,
		`INSERT INTO t0 VALUES(3)`}},
	// Updating an indexed table in a wide schema.
	{"a wide schema updating one indexed table frees nothing", []string{
		`CREATE TABLE t0(a)`, `CREATE INDEX t0a ON t0(a)`,
		`CREATE TABLE t1(a)`, `CREATE TABLE t2(a)`, `CREATE TABLE t3(a)`,
		`INSERT INTO t0 VALUES(1),(2),(3)`,
		`UPDATE t0 SET a = a + 100 WHERE a = 2`}},
}

var freelistRead = []string{
	`PRAGMA page_count`,
	`PRAGMA freelist_count`,
	`PRAGMA integrity_check`,
	`PRAGMA main.page_count`,
	`PRAGMA main.freelist_count`,
	`SELECT type, name FROM sqlite_master ORDER BY name`,
}

func TestFreelistCountsMatchCSQLite(t *testing.T) {
	for _, tc := range freelistPrograms {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			differ(t, "freelist/"+tc.name, append(append([]string{}, tc.stmts...), freelistRead...))
		})
	}
}

func TestVacuumPragmasMatchCSQLite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"incremental_vacuum is a no-op on a normal database", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE u(b)`,
			`DROP TABLE t`,
			`PRAGMA auto_vacuum`,
			`PRAGMA incremental_vacuum`,
			`PRAGMA page_count`,
			`PRAGMA freelist_count`,
			`PRAGMA integrity_check`,
			`PRAGMA incremental_vacuum(1)`,
			`PRAGMA incremental_vacuum(100)`,
			`PRAGMA page_count`,
			`PRAGMA freelist_count`,
			`PRAGMA main.auto_vacuum`,
			`PRAGMA main.incremental_vacuum`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		{"auto_vacuum=0 and an unrecognized value are no-ops", []string{
			`CREATE TABLE t(a)`,
			`PRAGMA auto_vacuum=0`,
			`PRAGMA auto_vacuum`,
			`PRAGMA auto_vacuum=none`,
			`PRAGMA auto_vacuum`,
			`PRAGMA auto_vacuum=xyzzy`,
			`PRAGMA auto_vacuum`,
			`PRAGMA page_count`,
		}},
		// auto_vacuum setter is ignored once the database has pages.
		{"auto_vacuum ON is ignored once the database has pages", []string{
			`CREATE TABLE t(a)`,
			`PRAGMA auto_vacuum=1`,
			`PRAGMA auto_vacuum`,
			`PRAGMA page_count`,
			`PRAGMA auto_vacuum=full`,
			`PRAGMA auto_vacuum`,
			`PRAGMA auto_vacuum=2`,
			`PRAGMA auto_vacuum`,
			`PRAGMA auto_vacuum=incremental`,
			`PRAGMA auto_vacuum`,
			`PRAGMA auto_vacuum=FULL`,
			`PRAGMA auto_vacuum('1')`,
			`PRAGMA main.auto_vacuum=1`,
			`PRAGMA auto_vacuum`,
			`PRAGMA page_count`,
			`PRAGMA freelist_count`,
			`INSERT INTO t VALUES(1)`,
			`SELECT count(*) FROM t`,
			`PRAGMA integrity_check`,
		}},
		// Out-of-range auto_vacuum values are no-ops.
		{"auto_vacuum values that resolve to no mode are no-ops on an empty database", []string{
			`PRAGMA auto_vacuum=0`,
			`PRAGMA auto_vacuum=none`,
			`PRAGMA auto_vacuum=5`,
			`PRAGMA auto_vacuum=on`,
			`PRAGMA auto_vacuum=true`,
			`PRAGMA auto_vacuum='invalid'`,
			`PRAGMA auto_vacuum=-1`,
			`PRAGMA auto_vacuum`,
			`CREATE TABLE t(a)`,
			`PRAGMA auto_vacuum`,
			`PRAGMA page_count`,
		}},
		// Clearing auto_vacuum leaves VACUUM converting nothing.
		{"clearing the deferred request leaves the VACUUM converting nothing", []string{
			`CREATE TABLE t(a)`,
			`INSERT INTO t VALUES(1),(2)`,
			`PRAGMA auto_vacuum=1`,
			`PRAGMA auto_vacuum=0`,
			`VACUUM`,
			`PRAGMA auto_vacuum`,
			`PRAGMA page_count`,
			`SELECT rowid, a FROM t ORDER BY rowid`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "vacuum/"+tc.name, tc.stmts) })
	}
}
