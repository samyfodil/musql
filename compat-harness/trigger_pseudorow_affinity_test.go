package compat

// This file tests that NEW./OLD. pseudo-row references in triggers yield
// their column's affinity in comparisons, but not in collation.

import "testing"

// TestTriggerPseudoRowYieldsItsAffinity tests affinity with trigger pseudo-row references.
func TestTriggerPseudoRowYieldsItsAffinity(t *testing.T) {
	differ(t, "trigger pseudo-row yields its affinity: NEW.<int> vs a TEXT column", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('05')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.a=d.x THEN 'hit' ELSE 'miss' END FROM d; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst`,
	})
	differ(t, "trigger pseudo-row yields its affinity: a TEXT column vs NEW.<int>", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('05')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN d.x=new.a THEN 'hit' ELSE 'miss' END FROM d; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst`,
	})
	differ(t, "trigger pseudo-row yields its affinity: OLD.<int> vs a TEXT column", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('05')`,
		`INSERT INTO t VALUES(5)`,
		`CREATE TRIGGER tr AFTER DELETE ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN old.a=d.x THEN 'hit' ELSE 'miss' END FROM d; END`,
		`DELETE FROM t`,
		`SELECT r FROM dst`,
	})
	differ(t, "trigger pseudo-row yields its affinity: in the WHEN clause", []string{
		`CREATE TABLE t(a INTEGER, s TEXT)`,
		`CREATE TABLE dst(r)`,
		`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.a=new.s BEGIN` +
			` INSERT INTO dst VALUES('fired'); END`,
		`INSERT INTO t VALUES(5,'05')`,
		`INSERT INTO t VALUES(5,'5')`,
		`SELECT r FROM dst`,
	})
	// A TYPELESS pseudo-row column is the same story from the other end: the
	// TEXT column still governs, so '5' is what 5 is compared as.
	differ(t, "trigger pseudo-row yields its affinity: typeless NEW column", []string{
		`CREATE TABLE t(z)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('5')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.z=d.x THEN 'hit' ELSE 'miss' END FROM d; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst`,
	})
}

// TestTriggerPseudoRowKeepsItsCollationAndRowidAffinity is the other half, and
// the reason this cannot be a blanket "drop everything" the way the excluded
// pseudo-row is: the DECLARED COLLATION survives (expr.c:255-263), and the
// ROWID -- both spellings -- keeps SQLITE_AFF_INTEGER (resolve.c:601-602).
func TestTriggerPseudoRowKeepsItsCollationAndRowidAffinity(t *testing.T) {
	// NOCASE declared on the pseudo-row's column still governs a comparison
	// against a BINARY column: 'ABC' matches 'abc'.
	differ(t, "trigger pseudo-row keeps its collation: NEW.<nocase> vs a BINARY column", []string{
		`CREATE TABLE t(v TEXT COLLATE NOCASE)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('abc')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.v=d.x THEN 'hit' ELSE 'miss' END FROM d; END`,
		`INSERT INTO t VALUES('ABC')`,
		`SELECT r FROM dst`,
	})
	// And against a literal, where there is nothing to fall through to at all.
	differ(t, "trigger pseudo-row keeps its collation: NEW.<nocase> vs a literal", []string{
		`CREATE TABLE t(v TEXT COLLATE NOCASE)`,
		`CREATE TABLE dst(r)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.v='abc' THEN 'hit' ELSE 'miss' END; END`,
		`INSERT INTO t VALUES('ABC')`,
		`SELECT r FROM dst`,
	})
	// An INTEGER PRIMARY KEY column is the rowid: iCol folds to -1 and the
	// reference keeps INTEGER affinity, so '05' IS coerced and DOES match.
	differ(t, "trigger pseudo-row keeps rowid affinity: NEW.<integer primary key>", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('05')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.k=d.x THEN 'hit' ELSE 'miss' END FROM d; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst`,
	})
	// The rowid pseudo-column spelling of the same thing.
	differ(t, "trigger pseudo-row keeps rowid affinity: NEW.rowid", []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('01')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.rowid=d.x THEN 'hit' ELSE 'miss' END FROM d; END`,
		`INSERT INTO t VALUES(7)`,
		`SELECT r FROM dst`,
	})
}
