// This file gates autoincrement resync when sqlite_sequence is modified directly.
// Deletes or updates to sqlite_sequence must affect subsequent autoincrement INSERTs.
package compat

import "testing"

// TestSqliteSequenceDeleteAllResyncsInTxn verifies that clearing sqlite_sequence resyncs autoincrement.
func TestSqliteSequenceDeleteAllResyncsInTxn(t *testing.T) {
	differ(t, "sqlite_sequence_delete_all_resync_txn", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`INSERT INTO t1(b) VALUES('x')`,
		`INSERT INTO t1(b) VALUES('y')`,
		`INSERT INTO t1(b) VALUES('z')`,
		`DELETE FROM t1 WHERE a=3`,
		`BEGIN`,
		`DELETE FROM sqlite_sequence`,
		`INSERT INTO t1(b) VALUES('new')`,
		`COMMIT`,
		`SELECT * FROM t1`,
	})
}

// TestSqliteSequenceDeleteOneNameResyncsInTxn verifies that selective deletes resync correctly.
func TestSqliteSequenceDeleteOneNameResyncsInTxn(t *testing.T) {
	differ(t, "sqlite_sequence_delete_one_name_resync_txn", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`CREATE TABLE t2(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`INSERT INTO t1(b) VALUES('x')`,
		`INSERT INTO t1(b) VALUES('y')`,
		`INSERT INTO t1(b) VALUES('z')`,
		`INSERT INTO t2(b) VALUES('p')`,
		`DELETE FROM t1 WHERE a=3`,
		`BEGIN`,
		`DELETE FROM sqlite_sequence WHERE name='t1'`,
		`INSERT INTO t1(b) VALUES('new1')`,
		`INSERT INTO t2(b) VALUES('new2')`,
		`COMMIT`,
		`SELECT * FROM t1`,
		`SELECT * FROM t2`,
	})
}

// TestSqliteSequenceUpdateResyncsInTxn: raising the stored seq via UPDATE
// (rather than lowering it via DELETE) is picked up too -- the resync takes
// the LARGER of the physical max and the stored value either way.
func TestSqliteSequenceUpdateResyncsInTxn(t *testing.T) {
	differ(t, "sqlite_sequence_update_resync_txn", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`INSERT INTO t1(b) VALUES('x')`,
		`BEGIN`,
		`UPDATE sqlite_sequence SET seq=100 WHERE name='t1'`,
		`INSERT INTO t1(b) VALUES('new')`,
		`COMMIT`,
		`SELECT * FROM t1`,
	})
}

// TestSqliteSequenceDeleteAllAutocommit: the same DELETE, but in AUTOCOMMIT
// (no explicit BEGIN/COMMIT) -- the shape mined from autoinc.test, which
// this engine's findTableMeta(stmt.table) declines "no such table:
// sqlite_sequence" for in the same write session that created the
// AUTOINCREMENT table: that table's tableMeta only lands in db.tables once
// its physical row has been written to disk and reloaded
// (findTableMetaOrSqliteSequenceIn's fallback, schema_write.go, covers it).
func TestSqliteSequenceDeleteAllAutocommit(t *testing.T) {
	differ(t, "sqlite_sequence_delete_all_autocommit", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`INSERT INTO t1(b) VALUES('x')`,
		`INSERT INTO t1(b) VALUES('y')`,
		`INSERT INTO t1(b) VALUES('z')`,
		`DELETE FROM t1 WHERE a=3`,
		`DELETE FROM sqlite_sequence`,
		`INSERT INTO t1(b) VALUES('new')`,
		`SELECT * FROM t1`,
	})
}

// TestSqliteSequenceUpdateNameNullAutocommit: mined from autoinc.test --
// "UPDATE sqlite_sequence SET name=NULL WHERE name='t2'" must at least
// COMPILE and RUN (the corpus statement itself, and the only shape verified
// against the oracle here -- see this file's package comment for the
// compile-time gap findTableMetaOrSqliteSequenceIn closes). What C SQLite
// does with the resulting ORPHANED (NULL, 1) row afterward -- it persists
// forever as a phantom entry, not derived from any table, which this
// engine's synthesized-from-db.tables sqlite_sequence view cannot
// represent -- is NOT covered here; it is a separate, narrower gap the
// mined corpus itself does not exercise (TestTCLCorpus's autoinc.test is
// wrong=0 with this fix; a direct read immediately after this UPDATE would
// not be).
func TestSqliteSequenceUpdateNameNullAutocommit(t *testing.T) {
	differ(t, "sqlite_sequence_update_name_null_autocommit", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`CREATE TABLE t2(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`INSERT INTO t1(b) VALUES('x')`,
		`INSERT INTO t2(b) VALUES('p')`,
		`UPDATE sqlite_sequence SET name=NULL WHERE name='t2'`,
		`INSERT INTO t2(b) VALUES('q')`,
		`SELECT * FROM t2`,
	})
}

// TestSqliteSequenceTriggerDrivenRowSurvivesDeleteElsewhere: a table whose
// AUTOINCREMENT row is only ever populated by a TRIGGER-driven insert (never
// as a top-level statement's own target, so noteAutoIncrementInsert's
// autoSeqRowSeen never fires for it) must still show its sqlite_sequence row
// -- and an unrelated DELETE FROM sqlite_sequence for a DIFFERENT table must
// not disturb it. Mined shape from autoinc.test's t3928/t3928c (a
// BEFORE/AFTER DELETE trigger pair feeding a second AUTOINCREMENT table).
func TestSqliteSequenceTriggerDrivenRowSurvivesDeleteElsewhere(t *testing.T) {
	differ(t, "sqlite_sequence_trigger_driven_row_survives", []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY, y)`,
		`CREATE TABLE t2(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`CREATE TABLE logtbl(c INTEGER PRIMARY KEY AUTOINCREMENT, d)`,
		`INSERT INTO t1 VALUES(1, 'x')`,
		`INSERT INTO t2(b) VALUES('p')`,
		`CREATE TRIGGER trg AFTER DELETE ON t1 BEGIN INSERT INTO logtbl(d) VALUES('deleted-'||old.x); END`,
		`DELETE FROM t1 WHERE x=1`,
		`DELETE FROM sqlite_sequence WHERE name='t2'`,
		`SELECT * FROM sqlite_sequence ORDER BY name`,
	})
}
