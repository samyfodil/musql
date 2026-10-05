// This file tests PRAGMA recursive_triggers with REPLACE conflicts and
// implicit DELETE triggers across multiple unique constraints.
package compat

import "testing"

// TestReplaceDeleteTriggerAdversarial_TwoUniqueConstraints_NewestVictimFirst tests
// conflict victim deletion order with two unique constraints.
func TestReplaceDeleteTriggerAdversarial_TwoUniqueConstraints_NewestVictimFirst(t *testing.T) {
	differ(t, "rvd-adv-two-unique-newest-first", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)",
		"CREATE TABLE log(kind TEXT, a INTEGER)",
		"INSERT INTO t VALUES(1,'x','p')",
		"INSERT INTO t VALUES(2,'y','q')",
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(3,'x','q')",
		"SELECT * FROM t ORDER BY a",
		"SELECT kind,a FROM log ORDER BY rowid",
	})
}

// TestReplaceDeleteTriggerAdversarial_TwoUniqueConstraints_BeforeTrigger tests
// with a BEFORE DELETE trigger instead of AFTER.
func TestReplaceDeleteTriggerAdversarial_TwoUniqueConstraints_BeforeTrigger(t *testing.T) {
	differ(t, "rvd-adv-two-unique-before-trigger", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)",
		"CREATE TABLE log(kind TEXT, a INTEGER, snapshot_count INTEGER)",
		"INSERT INTO t VALUES(1,'x','p')",
		"INSERT INTO t VALUES(2,'y','q')",
		"CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('bdel', old.a, (SELECT count(*) FROM t)); END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(3,'x','q')",
		"SELECT * FROM t ORDER BY a",
		"SELECT kind,a,snapshot_count FROM log ORDER BY rowid",
	})
}

// TestReplaceDeleteTriggerAdversarial_ThreeUniqueConstraints tests with three
// unique constraints conflicting simultaneously.
func TestReplaceDeleteTriggerAdversarial_ThreeUniqueConstraints(t *testing.T) {
	differ(t, "rvd-adv-three-unique", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE, d UNIQUE)",
		"CREATE TABLE log(kind TEXT, a INTEGER)",
		"INSERT INTO t VALUES(1,'x','p','m')",
		"INSERT INTO t VALUES(2,'y','q','m2')",
		"INSERT INTO t VALUES(3,'z','p2','n')",
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(4,'x','q','n')",
		"SELECT * FROM t ORDER BY a",
		"SELECT kind,a FROM log ORDER BY rowid",
	})
}

// TestReplaceDeleteTriggerAdversarial_MultiRowStatement tests multi-row INSERT
// with conflicts on each row.
func TestReplaceDeleteTriggerAdversarial_MultiRowStatement(t *testing.T) {
	differ(t, "rvd-adv-multi-row", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)",
		"CREATE TABLE log(kind TEXT, a INTEGER)",
		"INSERT INTO t VALUES(1,'x','p')",
		"INSERT INTO t VALUES(2,'y','q')",
		"INSERT INTO t VALUES(5,'w','r')",
		"INSERT INTO t VALUES(6,'v','s')",
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(3,'x','q'),(7,'w','s')",
		"SELECT * FROM t ORDER BY a",
		"SELECT kind,a FROM log ORDER BY rowid",
	})
}

// TestReplaceDeleteTriggerAdversarial_UpdateOrReplace_TwoUniqueConstraints tests
// UPDATE OR REPLACE with two unique constraint conflicts.
func TestReplaceDeleteTriggerAdversarial_UpdateOrReplace_TwoUniqueConstraints(t *testing.T) {
	differ(t, "rvd-adv-update-or-replace-two-unique", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)",
		"CREATE TABLE log(kind TEXT, a INTEGER)",
		"INSERT INTO t VALUES(1,'x','p')",
		"INSERT INTO t VALUES(2,'y','q')",
		"INSERT INTO t VALUES(9,'w','r')",
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END",
		"PRAGMA recursive_triggers=ON",
		"UPDATE OR REPLACE t SET b='x', c='q' WHERE a=9",
		"SELECT * FROM t ORDER BY a",
		"SELECT kind,a FROM log ORDER BY rowid",
	})
}

// TestReplaceDeleteTriggerAdversarial_RecheckUnrelatedThirdIndex tests that
// the post-trigger recheck catches conflicts on unrelated unique constraints.
func TestReplaceDeleteTriggerAdversarial_RecheckUnrelatedThirdIndex(t *testing.T) {
	differ(t, "rvd-adv-recheck-unrelated-index", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE, d UNIQUE)",
		"INSERT INTO t VALUES(1,'x','p','m1')",
		"INSERT INTO t VALUES(2,'y','q','m2')",
		"INSERT INTO t VALUES(3,'z','r','m3')",
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN UPDATE t SET d='n' WHERE a=3; END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(4,'x','q','n')",
		"SELECT * FROM t ORDER BY a",
	})
}

// TestReplaceDeleteTriggerAdversarial_FKCascade_TwoUniqueConstraints tests FK
// ON DELETE CASCADE interacting with conflict victim deletion order.
func TestReplaceDeleteTriggerAdversarial_FKCascade_TwoUniqueConstraints(t *testing.T) {
	differ(t, "rvd-adv-fk-cascade-two-unique", []string{
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE p(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)",
		"CREATE TABLE ch(id INTEGER PRIMARY KEY, pa INTEGER REFERENCES p(a) ON DELETE CASCADE)",
		"INSERT INTO p VALUES(1,'x','p')",
		"INSERT INTO p VALUES(2,'y','q')",
		"INSERT INTO ch VALUES(10,1)",
		"INSERT INTO ch VALUES(11,1)",
		"INSERT INTO ch VALUES(12,2)",
		"CREATE TABLE log(kind TEXT, a INTEGER)",
		"CREATE TRIGGER ad AFTER DELETE ON p BEGIN INSERT INTO log VALUES('del', old.a); END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO p VALUES(3,'x','q')",
		"SELECT * FROM p ORDER BY a",
		"SELECT * FROM ch ORDER BY id",
		"SELECT kind,a FROM log ORDER BY rowid",
	})
}

// TestReplaceDeleteTriggerAdversarial_PartialRaiseIgnore_TwoUniqueConstraints tests
// partial victim survival via RAISE(IGNORE) in a DELETE trigger.
func TestReplaceDeleteTriggerAdversarial_PartialRaiseIgnore_TwoUniqueConstraints(t *testing.T) {
	differ(t, "rvd-adv-partial-raise-ignore-two-unique", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE)",
		"INSERT INTO t VALUES(1,'x','p')",
		"INSERT INTO t VALUES(2,'y','q')",
		"CREATE TRIGGER bd BEFORE DELETE ON t BEGIN SELECT CASE WHEN old.a=2 THEN RAISE(IGNORE) END; END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(3,'x','q')",
		"SELECT * FROM t ORDER BY a",
	})
}

// TestReplaceDeleteTriggerAdversarial_RecursiveCascade tests a DELETE trigger
// that itself issues an INSERT OR REPLACE, cascading victim-finding across
// recursion levels.
func TestReplaceDeleteTriggerAdversarial_RecursiveCascade(t *testing.T) {
	differ(t, "rvd-adv-recursive-cascade", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE, d UNIQUE)",
		"CREATE TABLE log(kind TEXT, a INTEGER)",
		"INSERT INTO t VALUES(1,'x','p','m1')",
		"INSERT INTO t VALUES(2,'y','q','m2')",
		"INSERT INTO t VALUES(9,'w','r','n')",
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN " +
			"INSERT INTO log VALUES('del', old.a); " +
			"INSERT OR REPLACE INTO t SELECT 20,'zz','qq','n' WHERE old.a=2; " +
			"END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(3,'x','q','m3')",
		"SELECT * FROM t ORDER BY a",
		"SELECT kind,a FROM log ORDER BY rowid",
	})
}
