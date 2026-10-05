// This file tests PRAGMA temp_store in various transaction and temp-database states.
package compat

import "testing"

// TestR35ATempStoreTransactionParity pins the accept/reject boundary itself,
// against the oracle. Every case here is a statement-by-statement lockstep, so
// the FIRST disagreement is reported rather than a final state that happens to
// match.
func TestR35ATempStoreTransactionParity(t *testing.T) {
	cases := []struct {
		name   string
		script []string
	}{
		// Pragma inside a transaction without opening the temp database
		{"pragma.test#21: bare transaction", []string{
			`CREATE TABLE t1(a)`, `BEGIN`, `PRAGMA temp_store = 1`, `COMMIT`}},
		{"tempdb.test: written transaction", []string{
			`CREATE TABLE t1(a PRIMARY KEY, b, c)`, `CREATE TABLE t2(a, b, c)`,
			`BEGIN`, `INSERT INTO t1 VALUES(1,2,3)`, `INSERT INTO t1 VALUES(4,5,6)`,
			`INSERT INTO t2 VALUES(7,8,9)`, `INSERT INTO t2 SELECT * FROM t1`,
			`PRAGMA temp_store = 'memory'`, `ROLLBACK`}},
		{"no transaction at all", []string{`PRAGMA temp_store = 1`}},

		// When the temp database is open, pragma in a transaction is an error
		{"temp table open, in transaction", []string{
			`CREATE TEMP TABLE tt(z)`, `BEGIN`, `PRAGMA temp_store = 1`}},
		{"temp table DROPPED, in transaction", []string{
			`CREATE TEMP TABLE tt(z)`, `DROP TABLE tt`, `BEGIN`, `PRAGMA temp_store = 1`}},
		{"integrity_check opened it, in transaction", []string{
			`CREATE TABLE t1(a)`, `PRAGMA integrity_check`, `BEGIN`, `PRAGMA temp_store = 1`}},
		{"temp-qualified pragma opened it, in transaction", []string{
			`CREATE TABLE t1(a)`, `PRAGMA temp.locking_mode`, `BEGIN`, `PRAGMA temp_store = 1`}},

		// Re-setting to the same value or reading is always allowed
		{"re-set to the value in force", []string{
			`CREATE TEMP TABLE tt(z)`, `BEGIN`, `PRAGMA temp_store = 0`, `COMMIT`}},
		{"getter is never restricted", []string{
			`CREATE TEMP TABLE tt(z)`, `BEGIN`, `PRAGMA temp_store`, `COMMIT`}},

		// Sorting doesn't count as opening the temp database
		{"ORDER BY does not open it", []string{
			`CREATE TABLE t1(a)`, `INSERT INTO t1 VALUES(2),(1)`,
			`BEGIN`, `PRAGMA temp_store = 1`, `COMMIT`}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			flLockstep(t, tc.name, tc.script)
		})
	}
}

// TestR35ATempStoreDiscardsTempObjects verifies that changing temp_store while
// the temp database is open closes existing temp objects.
func TestR35ATempStoreDiscardsTempObjects(t *testing.T) {
	cases := []struct {
		name   string
		script []string
		verify []string
	}{
		{"a temp table does not survive it", []string{
			`CREATE TEMP TABLE tt(a)`, `INSERT INTO tt VALUES(1)`, `PRAGMA temp_store=2`},
			[]string{`SELECT count(*) FROM tt`, `SELECT count(*) FROM sqlite_temp_master`}},
		{"nor a temp view, index or trigger", []string{
			`CREATE TABLE m(a)`, `CREATE TEMP TABLE tt(a)`, `CREATE INDEX ti ON tt(a)`,
			`CREATE TEMP VIEW tv AS SELECT 1`,
			`CREATE TEMP TRIGGER ttr AFTER INSERT ON m BEGIN SELECT 1; END`,
			`PRAGMA temp_store=1`},
			[]string{`SELECT count(*) FROM sqlite_temp_master`, `SELECT count(*) FROM m`,
				`SELECT type,name FROM sqlite_master ORDER BY name`}},
		{"check_temp_store's own IF NOT EXISTS cycle", []string{
			`CREATE TEMP TABLE IF NOT EXISTS a(b)`, `INSERT INTO a VALUES(1)`,
			`PRAGMA temp_store=1`,
			`CREATE TEMP TABLE IF NOT EXISTS a(b)`},
			[]string{`SELECT count(*) FROM a`}},
		{"main's schema_version is untouched", []string{
			`CREATE TABLE m(a)`, `CREATE TEMP TABLE tt(a)`, `PRAGMA temp_store=2`},
			[]string{`PRAGMA schema_version`, `PRAGMA main.page_count`}},
		{"the temp database can be used again afterwards", []string{
			`CREATE TEMP TABLE tt(a)`, `PRAGMA temp_store=2`,
			`CREATE TEMP TABLE tt2(b)`, `INSERT INTO tt2 VALUES(9)`},
			[]string{`SELECT * FROM tt2`, `SELECT count(*) FROM sqlite_temp_master`}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			flLockstep(t, tc.name, tc.script, tc.verify...)
		})
	}
}
