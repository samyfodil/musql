// Tests INSERT ... ON CONFLICT ... DO UPDATE with UPDATE triggers.
// The DO UPDATE arm must fire the target's BEFORE and AFTER UPDATE triggers,
// and trigger side effects must not be silently lost.
package compat

import "testing"

func TestUpsertDoUpdateFiresUpdateTriggers(t *testing.T) {
	differ(t, "upsert DO UPDATE fires BEFORE and AFTER UPDATE triggers", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c DEFAULT 0)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t1 BEGIN INSERT INTO log VALUES('before-upd'); END`,
		`CREATE TRIGGER tu AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('after-upd:'||old.c||'->'||new.c); END`,
		`INSERT INTO t1(a,b) VALUES(1,2)`,
		`SELECT a,b,c FROM t1`,
		`SELECT count(*) AS n FROM log`,
		// Conflict: row changes and triggers fire.
		`INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=t1.c+1`,
		`SELECT a,b,c FROM t1`,
		`SELECT m FROM log ORDER BY rowid`,
		// WHERE false: no change and no trigger fire.
		`INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1 WHERE c<0`,
		`SELECT a,b,c FROM t1`,
		`SELECT m FROM log ORDER BY rowid`,
		// WHERE true: triggers fire.
		`INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+10 WHERE c>0`,
		`SELECT a,b,c FROM t1`,
		`SELECT m FROM log ORDER BY rowid`,
	})
	// Non-conflicting insert: no UPDATE trigger fire.
	differ(t, "a non-conflicting upsert fires no UPDATE trigger", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c DEFAULT 0)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('upd'); END`,
		`INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`,
		`INSERT INTO t1(a,b) VALUES(2,3) ON CONFLICT(a) DO UPDATE SET c=c+1`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
		`SELECT count(*) AS n FROM log`,
	})
	// UPDATE OF filtering: only set columns fire their triggers.
	differ(t, "upsert DO UPDATE honours UPDATE OF", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c DEFAULT 0)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tc AFTER UPDATE OF c ON t1 BEGIN INSERT INTO log VALUES('of-c'); END`,
		`CREATE TRIGGER tbb AFTER UPDATE OF b ON t1 BEGIN INSERT INTO log VALUES('of-b'); END`,
		`INSERT INTO t1(a,b) VALUES(1,2)`,
		`INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`,
		`SELECT m FROM log ORDER BY rowid`,
		`SELECT a,b,c FROM t1`,
	})
	// INSERT and UPDATE triggers on same table: each arm fires its triggers.
	differ(t, "upsert with both INSERT and UPDATE triggers", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c DEFAULT 0)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER ti AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES('ins:'||new.a); END`,
		`CREATE TRIGGER tu AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('upd:'||new.c); END`,
		`INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`,
		`INSERT INTO t1(a,b) VALUES(1,9) ON CONFLICT(a) DO UPDATE SET c=c+1`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
		`SELECT m FROM log ORDER BY rowid`,
	})
	// Trigger body writing to target and RAISE(IGNORE) in BEFORE trigger.
	differ(t, "upsert DO UPDATE with a self-writing and an IGNORE trigger", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c DEFAULT 0)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER tw AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('w:'||(SELECT count(*) FROM t1)); END`,
		`INSERT INTO t1(a,b) VALUES(1,2)`,
		`INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`,
		`SELECT m FROM log ORDER BY rowid`,
		`SELECT a,b,c FROM t1`,
		`DROP TRIGGER tw`,
		`CREATE TRIGGER tig BEFORE UPDATE ON t1 BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+100`,
		`SELECT a,b,c FROM t1`,
	})
}
