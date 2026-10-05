// This file tests CREATE TRIGGER spellings with string-literal names
// and VALUES(...) body statements, verified via their side effects.
package compat

import "testing"

// TestWpathR24TriggerStringNames tests string-literal trigger names.
func TestWpathR24TriggerStringNames(t *testing.T) {
	// String trigger name.
	flLockstep(t, "string-trigger-name", []string{
		`CREATE TABLE t8(a,b,c)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER 'trig4' AFTER INSERT ON t8 BEGIN INSERT INTO log VALUES(new.b); END`,
		`INSERT INTO t8 VALUES(4,5,6)`,
		`DROP TRIGGER 'trig4'`,
		`INSERT INTO t8 VALUES(7,8,9)`,
	}, `SELECT * FROM log`, `SELECT name FROM sqlite_master WHERE type='trigger'`)

	// String in schema position.
	flLockstep(t, "string-schema-qualifier", []string{
		`CREATE TABLE "ON"(a,b,c)`,
		`CREATE TRIGGER 'on'.trig4 AFTER INSERT ON 'ON' BEGIN SELECT 1; END`,
	}, `SELECT name FROM sqlite_master WHERE type='trigger'`)

	// String catalog qualifier.
	flLockstep(t, "string-main-qualifier", []string{
		`CREATE TABLE t8(a)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER 'main'.tr1 AFTER INSERT ON t8 BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER tr3 AFTER INSERT ON "main".t8 BEGIN INSERT INTO log VALUES(new.a*10); END`,
		`INSERT INTO t8 VALUES(1)`,
	}, `SELECT * FROM log ORDER BY x`, `SELECT name FROM sqlite_master WHERE type='trigger' ORDER BY name`)
}

// TestWpathR24TriggerStringTableName tests the ON clause's TABLE name
// spelled as a STRING literal, including after ALTER TABLE RENAME.
func TestWpathR24TriggerStringTableName(t *testing.T) {
	flLockstep(t, "string-on-clause-table-name", []string{
		`CREATE TABLE t8(a,b,c)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER trig3 AFTER INSERT ON main.'t8' BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO t8 VALUES(1,2,3)`,
		`ALTER TABLE t8 RENAME TO t9`,
		`INSERT INTO t9 VALUES(4,5,6)`,
	}, `SELECT * FROM log ORDER BY x`,
		`SELECT name, tbl_name FROM sqlite_master WHERE type='trigger'`)

	// Unqualified with string trigger name.
	flLockstep(t, "string-on-clause-unqualified", []string{
		`CREATE TABLE t8(a,b,c)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER 'trig4' AFTER INSERT ON 't8' BEGIN INSERT INTO log VALUES(new.b); END`,
		`INSERT INTO t8 VALUES(1,2,3)`,
		`ALTER TABLE t8 RENAME TO t9`,
		`INSERT INTO t9 VALUES(4,5,6)`,
	}, `SELECT * FROM log ORDER BY x`,
		`SELECT name, tbl_name FROM sqlite_master WHERE type='trigger'`)
}

// TestWpathR24TriggerValuesBody tests VALUES(...) as trigger body.
func TestWpathR24TriggerValuesBody(t *testing.T) {
	// returning1.test 13: VALUES(0) is a no-op body statement, and the firing
	// UPDATE still applies.
	flLockstep(t, "values-body-update-trigger", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER r1 AFTER UPDATE ON t1 BEGIN VALUES(0); END`,
		`UPDATE t1 SET b=3`,
	}, `SELECT * FROM t1`, `SELECT name FROM sqlite_master WHERE type='trigger'`)

	// A bad reference inside a VALUES body raises even for a ZERO-row firing.
	flLockstep(t, "values-body-bad-column", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER r3 AFTER DELETE ON t1 BEGIN VALUES(nosuchcol); END`,
		`DELETE FROM t1 WHERE a=99`,
		`DELETE FROM t1 WHERE a=1`,
	}, `SELECT * FROM t1 ORDER BY a`)

	// OLD/NEW resolve inside it, like any other body expression.
	flLockstep(t, "values-body-old-new", []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER r4 BEFORE UPDATE ON t1 BEGIN VALUES(old.b + new.b); END`,
		`CREATE TRIGGER r5 AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES(old.b||'/'||new.b); END`,
		`UPDATE t1 SET b=5`,
	}, `SELECT * FROM t1`, `SELECT * FROM log`)

	// A multi-tuple VALUES body: accepted by C SQLite, and this engine's
	// compound-shape rule decides it -- whichever way, both sides must agree on
	// what the firing statement does.
	flLockstep(t, "values-body-multi-tuple", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER r2 AFTER INSERT ON t1 BEGIN VALUES(1),(2); END`,
		`INSERT INTO t1 VALUES(7,8)`,
	}, `SELECT * FROM t1 ORDER BY a`)
}
