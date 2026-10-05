// Tests ALTER TABLE ... ADD COLUMN with non-constant DEFAULT expressions.
// On empty tables they are accepted with the expression becoming a per-row
// default; on non-empty tables they fail.
package compat

import "testing"

func TestAddColumnNonConstantDefaultEmptyTable(t *testing.T) {
	// Verify non-constant defaults on empty tables are accepted and become per-row defaults.
	differAlter(t, "add column non-constant default, empty table", []string{
		`CREATE TABLE t2(x)`,
		`ALTER TABLE t2 ADD COLUMN g DEFAULT CURRENT_TIME`,
		`ALTER TABLE t2 ADD COLUMN h DEFAULT CURRENT_TIMESTAMP`,
		`ALTER TABLE t2 ADD COLUMN i DEFAULT CURRENT_DATE`,
		`ALTER TABLE t2 ADD COLUMN j DEFAULT (1+2)`,
		`ALTER TABLE t2 ADD COLUMN k DEFAULT (abs(-3))`,
		`SELECT sql FROM sqlite_master WHERE name='t2'`,
		`PRAGMA table_info(t2)`,
		`INSERT INTO t2(x) VALUES(1)`,
		`SELECT length(g), length(h), length(i), j, k FROM t2`,
		// The clock default must be a LIVE reading taken during this run, not a
		// constant, a NULL, or an epoch.
		//
		// A WINDOW, not equality. This was "h = strftime(...,'now')" and it
		// MANUFACTURED A DIVERGENCE under load: the two engines are run
		// separately, so if one run straddles a second boundary between the
		// INSERT and this SELECT and the other does not, they disagree with
		// nothing wrong. It fired once during a six-stream verification (cgo 1,
		// musql 0) and passed 3/3 standalone immediately after.
		//
		// The liveness the equality was reaching for is already pinned above,
		// race-free: the sqlite_master text and table_info.dflt_value must both
		// still read CURRENT_TIMESTAMP, so a value frozen at ALTER time would
		// have to have folded the clause away to get here.
		`SELECT h >= strftime('%Y-%m-%d %H:%M:%S','now','-60 seconds')
		    AND h <= strftime('%Y-%m-%d %H:%M:%S','now','+60 seconds') AS live FROM t2`,
		// An explicit value still overrides the default.
		`INSERT INTO t2(x,j) VALUES(2,99)`,
		`SELECT x, j FROM t2 ORDER BY x`,
	})
	// A non-constant default alongside REFERENCES -- the exact spelling
	// fkey2.test and without_rowid3.test use, right after the one the
	// neighbouring gate covers.
	differAlter(t, "add column non-constant default with references", []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE t1(a PRIMARY KEY)`,
		`CREATE TABLE t2(x)`,
		`ALTER TABLE t2 ADD COLUMN g DEFAULT CURRENT_TIME REFERENCES t1`,
		`SELECT sql FROM sqlite_master WHERE name='t2'`,
		`PRAGMA foreign_key_list(t2)`,
	})
}

func TestAddColumnNonConstantDefaultNonEmptyTable(t *testing.T) {
	differAlter(t, "add column non-constant default, non-empty table", []string{
		`CREATE TABLE n1(x)`,
		`INSERT INTO n1 VALUES(1)`,
		`ALTER TABLE n1 ADD COLUMN g DEFAULT CURRENT_TIME`,
		`SELECT sql FROM sqlite_master WHERE name='n1'`,
		`ALTER TABLE n1 ADD COLUMN m DEFAULT (1+2)`,
		`SELECT sql FROM sqlite_master WHERE name='n1'`,
		`SELECT * FROM n1`,
		// A CONSTANT default is still fine with rows present, and back-fills.
		`ALTER TABLE n1 ADD COLUMN c DEFAULT 7`,
		`SELECT x, c FROM n1`,
		`SELECT sql FROM sqlite_master WHERE name='n1'`,
	})
}
