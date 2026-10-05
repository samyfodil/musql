// Tests ON CONFLICT target with explicit COLLATE, verifying it matches the
// index's effective collation.
package compat

import "testing"

func TestUpsertTargetCollateMatches(t *testing.T) {
	differ(t, "upsert_target_collate_matches", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT UNIQUE)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`INSERT INTO t1(a,b) VALUES(2,'x') ON CONFLICT(b COLLATE binary) DO UPDATE SET a=99`,
		`SELECT * FROM t1`,
	})
	differ(t, "upsert_target_collate_desc", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT UNIQUE)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`INSERT INTO t1(a,b) VALUES(2,'x') ON CONFLICT(b COLLATE binary DESC) DO UPDATE SET a=99`,
		`SELECT * FROM t1`,
	})
	// With a unique index instead of table-level UNIQUE constraint.
	differ(t, "upsert_target_collate_mined_repro", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT, c DEFAULT 0)`,
		`CREATE UNIQUE INDEX t1x1 ON t1(b)`,
		`INSERT INTO t1 VALUES(1,'x',1)`,
		`INSERT INTO t1(a,b) VALUES(5,'x') ON CONFLICT(b COLLATE binary) DO NOTHING`,
		`SELECT * FROM t1`,
	})
}

// TestUpsertTargetCollateMismatchFails verifies that a mismatched COLLATE fails
// the entire statement.
func TestUpsertTargetCollateMismatchFails(t *testing.T) {
	differ(t, "upsert_target_collate_mismatch", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT UNIQUE)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`INSERT INTO t1(a,b) VALUES(2,'x') ON CONFLICT(b COLLATE nocase) DO UPDATE SET a=99`,
		`SELECT * FROM t1`,
	})
}

// TestUpsertTargetExpressionStaysDeclined verifies that expression conflict
// targets remain declined.
func TestUpsertTargetExpressionStaysDeclined(t *testing.T) {
	res := run(t, "musql", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT UNIQUE)`,
		`INSERT INTO t1(a,b) VALUES(2,'x') ON CONFLICT(b+1) DO NOTHING`,
	})
	if res[len(res)-1]["kind"] != "error" {
		t.Errorf("expected decline for expression conflict target, got: %v", res[len(res)-1])
	}
}
