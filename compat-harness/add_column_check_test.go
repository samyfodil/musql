// ALTER TABLE ADD COLUMN CHECK: evaluate against existing rows; empty table OK.
//
//	two CHECK(...) clauses on one column     -> both enforced independently
package compat

import "testing"

func TestAddColumnCheckEmptyTable(t *testing.T) {
	differAlter(t, "add column check, empty table, default violates", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > 0) DEFAULT -5`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`INSERT INTO t1(id) VALUES(1)`,
		`SELECT * FROM t1`,
		// The check now enforces going forward, even though the empty-table
		// ALTER itself never evaluated it.
		`INSERT INTO t1 VALUES(2, 1)`,
	})
	differAlter(t, "add column check, empty table, default satisfies", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > 0) DEFAULT 5`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`INSERT INTO t1(id) VALUES(1)`,
		`SELECT * FROM t1`,
		`INSERT INTO t1(id, x) VALUES(2, -1)`,
	})
}

func TestAddColumnCheckNonEmptyTable(t *testing.T) {
	// The refusal itself, and that it leaves the schema untouched.
	differAlter(t, "add column check, non-empty table, default violates", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES(1)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > 0) DEFAULT -5`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`SELECT * FROM t1`,
	})
	differAlter(t, "add column check, non-empty table, default satisfies", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES(1)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > 0) DEFAULT 5`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`SELECT * FROM t1`,
	})
	// No DEFAULT at all: existing rows are NULL-filled, and NULL passes an
	// ordinary CHECK by 3-valued logic (NULL is neither true nor false).
	differAlter(t, "add column check, non-empty table, no default (NULL fill)", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES(1)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > 0)`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`SELECT * FROM t1`,
	})
	// Two CHECK clauses on the same added column, both enforced.
	differAlter(t, "add column with two CHECK clauses", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES(1)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > 0) CHECK(x < 100) DEFAULT 5`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`SELECT * FROM t1`,
		`INSERT INTO t1 VALUES(2, 500)`,
		`INSERT INTO t1 VALUES(3, -5)`,
	})
	// Adding a column with NO check of its own must not disturb the table's
	// OWN pre-existing CHECK constraint (tbl.checks must not be silently
	// replaced with an empty list).
	differAlter(t, "add column without a check preserves an existing one", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY, y INTEGER CHECK(y > 0))`,
		`INSERT INTO t1 VALUES(1, 10)`,
		`ALTER TABLE t1 ADD COLUMN z TEXT`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`INSERT INTO t1 VALUES(2, -5, 'x')`,
		`UPDATE t1 SET y = -1 WHERE id=1`,
	})
}

func TestAddColumnCheckReferencesSiblingColumn(t *testing.T) {
	// The CHECK is evaluated PER ROW against that row's own sibling-column
	// value, not just the literal DEFAULT checked once in isolation: the
	// same default (0) satisfies row 2's check (0 > -10) but violates row
	// 1's (0 > 10 is false), so the whole ALTER refuses.
	differAlter(t, "add column check references a sibling column, mixed rows", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY, y INTEGER)`,
		`INSERT INTO t1 VALUES(1, 10), (2, -10)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > y) DEFAULT 0`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`SELECT * FROM t1`,
	})
	differAlter(t, "add column check references a sibling column, all rows pass", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY, y INTEGER)`,
		`INSERT INTO t1 VALUES(1, -10), (2, -20)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x > y) DEFAULT 0`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`SELECT * FROM t1`,
	})
}

// An unsupported CHECK shape (a subquery, same restriction CREATE TABLE's
// own CHECK parsing already applies) is a MUTUAL reject, not a one-sided
// decline -- worth pinning since it is the boundary this addColumn work
// does NOT attempt to widen.
func TestAddColumnCheckUnsupportedShapeMutualReject(t *testing.T) {
	differAlter(t, "add column check with a subquery is a mutual reject", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY)`,
		`ALTER TABLE t1 ADD COLUMN x INTEGER CHECK(x IN (SELECT 1))`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
	})
}
