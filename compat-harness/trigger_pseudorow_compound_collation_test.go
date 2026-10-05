package compat

import "testing"

// TestTriggerPseudoRowCompoundCollation tests that NEW/OLD references in
// trigger compound SELECT bodies use the trigger table's declared collation.
func TestTriggerPseudoRowCompoundCollation(t *testing.T) {
	t.Run("intersect-new", func(t *testing.T) {
		flLockstep(t, "intersect-new", []string{
			`CREATE TABLE s(a TEXT COLLATE NOCASE)`,
			`CREATE TABLE log(v)`,
			`CREATE TRIGGER tr AFTER INSERT ON s BEGIN INSERT INTO log VALUES((SELECT new.a INTERSECT SELECT 'ABC')); END`,
			`INSERT INTO s VALUES('abc')`,
		}, `SELECT v, typeof(v) FROM log`)
	})
	t.Run("except-new", func(t *testing.T) {
		flLockstep(t, "except-new", []string{
			`CREATE TABLE s(a TEXT COLLATE NOCASE)`,
			`CREATE TABLE log(v)`,
			`CREATE TRIGGER tr AFTER INSERT ON s BEGIN INSERT INTO log VALUES((SELECT new.a EXCEPT SELECT 'ABC')); END`,
			`INSERT INTO s VALUES('abc')`,
		}, `SELECT v, typeof(v) FROM log`)
	})
	t.Run("union-new", func(t *testing.T) {
		flLockstep(t, "union-new", []string{
			`CREATE TABLE s(a TEXT COLLATE NOCASE)`,
			`CREATE TABLE log(v)`,
			`CREATE TRIGGER tr AFTER INSERT ON s BEGIN INSERT INTO log SELECT new.a UNION SELECT 'ABC'; END`,
			`INSERT INTO s VALUES('abc')`,
		}, `SELECT v FROM log ORDER BY rowid`)
	})
	// The BINARY control: the same shapes over a column with no declared
	// collation must keep answering the BINARY way, so the case above is a
	// measurement of the collation and not of the compound.
	t.Run("intersect-new-binary-control", func(t *testing.T) {
		flLockstep(t, "intersect-new-binary-control", []string{
			`CREATE TABLE s(a TEXT)`,
			`CREATE TABLE log(v)`,
			`CREATE TRIGGER tr AFTER INSERT ON s BEGIN INSERT INTO log VALUES((SELECT new.a INTERSECT SELECT 'ABC')); END`,
			`INSERT INTO s VALUES('abc')`,
		}, `SELECT v, typeof(v) FROM log`)
	})
	// OLD's side of the same rule, on a DELETE event.
	t.Run("intersect-old", func(t *testing.T) {
		flLockstep(t, "intersect-old", []string{
			`CREATE TABLE s(a TEXT COLLATE NOCASE)`,
			`CREATE TABLE log(v)`,
			`CREATE TRIGGER tr AFTER DELETE ON s BEGIN INSERT INTO log VALUES((SELECT old.a INTERSECT SELECT 'ABC')); END`,
			`INSERT INTO s VALUES('abc')`,
			`DELETE FROM s`,
		}, `SELECT v, typeof(v) FROM log`)
	})
}
