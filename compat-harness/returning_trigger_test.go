// Tests RETURNING against tables with triggers on INSERT/UPDATE/DELETE.
package compat

import "testing"

// TestReturningTrigger covers RETURNING against a table with triggers, for
// INSERT/UPDATE/DELETE, over TEMP tables and TEMP triggers (returning1.test's
// own 11.1-11.7 setup, reproduced verbatim -- its expected TCL output is
// {1 2 | happy glad |} / {1 9 x} / {1 9 @ happy glad @} /
// {I1 1 2 I1 happy glad U1 1 9 D1 1 9 D1 happy glad} for the first four
// cases below). Every case asserts the trigger's LOG TABLE too, not just the
// returned rows -- an accepted RETURNING whose triggers silently stopped
// firing would still pass a rows-only check.
func TestReturningTrigger(t *testing.T) {
	seed := []string{
		`CREATE TEMP TABLE t1(a,b)`,
		`CREATE TEMP TABLE t2(c,d)`,
		`CREATE TEMP TABLE t3(e,f)`,
		`CREATE TEMP TABLE log(op,x,y)`,
		`CREATE TEMP TRIGGER t1r1 AFTER INSERT ON t1 BEGIN INSERT INTO log(op,x,y) VALUES('I1',new.a,new.b); END`,
		`CREATE TEMP TRIGGER t1r2 BEFORE DELETE ON t1 BEGIN INSERT INTO log(op,x,y) VALUES('D1',old.a,old.b); END`,
		`CREATE TEMP TRIGGER t2r3 AFTER UPDATE ON t1 BEGIN INSERT INTO log(op,x,y) VALUES('U1',new.a,new.b); END`,
		`CREATE TEMP TRIGGER t2r1 BEFORE INSERT ON t2 BEGIN INSERT INTO log(op,x,y) VALUES('I2',new.c,new.d); END`,
		`CREATE TEMP TRIGGER t3r1 AFTER DELETE ON t3 BEGIN INSERT INTO log(op,x,y) VALUES('D3',old.e,old.f); END`,
		`CREATE TEMP TRIGGER t3r2 BEFORE UPDATE ON t3 BEGIN INSERT INTO log(op,x,y) VALUES('U3',new.e,new.f); END`,
	}
	// returning1.test 11.1/11.4: INSERT into a table with an AFTER INSERT
	// trigger, then UPDATE and DELETE it (a BEFORE DELETE trigger fires this
	// time) -- RETURNING's rows AND the trigger log both checked.
	differ(t, "mined: insert/update/delete returning over triggered table", append(append([]string{}, seed...),
		`INSERT INTO t1(a,b) VALUES(1,2),('happy','glad') RETURNING a, b, '|'`,
		`UPDATE t1 SET b=9 WHERE a=1 RETURNING a, b, 'x'`,
		`DELETE FROM t1 WHERE a<>'xray' RETURNING a, b, '@'`,
		`SELECT * FROM log`,
	))
	// returning1.test 11.5/11.6: INSERT into a table with a BEFORE INSERT
	// trigger (the trigger's own write is what RETURNING must NOT be
	// confused by).
	differ(t, "mined: insert returning over table with before-insert trigger", append(append([]string{}, seed...),
		`INSERT INTO t2 VALUES('bravo','charlie') RETURNING d, c, 'z'`,
		`SELECT * FROM log`,
	))
	// returning1.test 11.7: INSERT/UPDATE/DELETE all in one statement list
	// against a table whose AFTER DELETE and BEFORE UPDATE triggers both log.
	differ(t, "mined: full lifecycle returning over triggered table", append(append([]string{}, seed...),
		`INSERT INTO t3(e) VALUES(1),(2),(3) RETURNING 'I', e`,
		`UPDATE t3 SET f=e+100 RETURNING 'U', e, f`,
		`DELETE FROM t3 WHERE f>100 RETURNING 'D', e, f`,
		`SELECT * FROM log`,
	))
	// An AFTER trigger's own cascaded write to the SAME table must never
	// appear as an extra RETURNING row, for INSERT and UPDATE alike.
	differ(t, "after trigger cascade to same table is not returned", []string{
		`CREATE TABLE c1(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TRIGGER cr1 AFTER INSERT ON c1 WHEN NEW.a < 100 BEGIN INSERT INTO c1 VALUES(NEW.a + 100, 'cascaded'); END`,
		`INSERT INTO c1(a,b) VALUES(1,'one'),(2,'two') RETURNING a, b`,
		`SELECT * FROM c1 ORDER BY a`,
		`CREATE TABLE c2(a INTEGER PRIMARY KEY, b TEXT, c INTEGER DEFAULT 0)`,
		`CREATE TRIGGER cr2 AFTER UPDATE ON c2 WHEN NEW.c < 5 BEGIN UPDATE c2 SET c = c + 1 WHERE a = NEW.a; END`,
		`INSERT INTO c2(a,b) VALUES(1,'one'),(2,'two')`,
		`UPDATE c2 SET b = b || '!' RETURNING a, b, c`,
		`SELECT * FROM c2 ORDER BY a`,
	})
	// A BEFORE UPDATE trigger's own cascaded write to the SAME row DOES show
	// through into the captured row (it runs before this row's own store).
	differ(t, "before trigger cascade to same row is returned", []string{
		`CREATE TABLE t4(a INTEGER PRIMARY KEY, b TEXT, c INTEGER)`,
		`CREATE TRIGGER tr4 BEFORE UPDATE ON t4 BEGIN UPDATE t4 SET c = c + 100 WHERE a = NEW.a; END`,
		`INSERT INTO t4 VALUES(1,'one',1),(2,'two',2)`,
		`UPDATE t4 SET b = 'updated' WHERE a IN (1,2) RETURNING a, b, c`,
	})
	// RAISE(IGNORE) from a BEFORE trigger skips the row entirely: no write,
	// no RETURNING row, no AFTER trigger firing. Checked for INSERT and
	// DELETE, log table included.
	differ(t, "before raise ignore excludes the row from returning", []string{
		`CREATE TABLE t5(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TABLE log5(x)`,
		`CREATE TRIGGER tr5b BEFORE INSERT ON t5 WHEN NEW.a = 2 BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER tr5a AFTER INSERT ON t5 BEGIN INSERT INTO log5 VALUES('after:' || NEW.a); END`,
		`INSERT INTO t5(a,b) VALUES(1,'one'),(2,'two'),(3,'three') RETURNING a, b`,
		`SELECT * FROM t5 ORDER BY a`,
		`SELECT * FROM log5`,
		`CREATE TABLE t6(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TRIGGER tr6 BEFORE DELETE ON t6 WHEN OLD.a = 2 BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t6 VALUES(1,'one'),(2,'two'),(3,'three')`,
		`DELETE FROM t6 RETURNING a, b`,
		`SELECT * FROM t6 ORDER BY a`,
	})
	// RAISE(IGNORE) from an AFTER trigger, by contrast, does NOT exclude the
	// row: it already wrote and stays, and capture already ran before AFTER
	// fired. INSERT, UPDATE, and DELETE all checked.
	differ(t, "after raise ignore still returns the row", []string{
		`CREATE TABLE t7(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TRIGGER tr7 AFTER INSERT ON t7 BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t7(a,b) VALUES(1,'one'),(2,'two') RETURNING a, b`,
		`SELECT * FROM t7 ORDER BY a`,
		`CREATE TABLE t8(a INTEGER PRIMARY KEY, b TEXT)`,
		`INSERT INTO t8 VALUES(1,'one'),(2,'two')`,
		`CREATE TRIGGER tr8 AFTER UPDATE ON t8 BEGIN SELECT RAISE(IGNORE); END`,
		`UPDATE t8 SET b = b || '!' RETURNING a, b`,
		`CREATE TABLE t9(a INTEGER PRIMARY KEY, b TEXT)`,
		`INSERT INTO t9 VALUES(1,'one'),(2,'two')`,
		`CREATE TRIGGER tr9 AFTER DELETE ON t9 BEGIN SELECT RAISE(IGNORE); END`,
		`DELETE FROM t9 RETURNING a, b`,
	})
	// Multi-row ordering under a trigger: rows come back in ascending-rowid
	// order regardless of insertion/matching order.
	differ(t, "triggered table returning order is ascending rowid", []string{
		`CREATE TABLE t10(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TRIGGER tr10 AFTER UPDATE ON t10 BEGIN SELECT 1; END`,
		`INSERT INTO t10 VALUES(3,'c'),(1,'a'),(2,'b')`,
		`UPDATE t10 SET b = b || '!' RETURNING a, b`,
	})
	// A table with triggers AND a self-referential subquery in RETURNING
	// combined -- both shapes' machinery threaded through the same trigger-
	// aware capture point (see also returning_subquery_test.go's
	// TestReturningSubquery for the subquery shape on its own).
	differ(t, "triggered table with self-referential returning subquery", []string{
		`CREATE TABLE t11(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t11log(x)`,
		`CREATE TRIGGER tr11 AFTER INSERT ON t11 BEGIN INSERT INTO t11log VALUES(NEW.a); END`,
		`INSERT INTO t11 VALUES(1,10)`,
		`INSERT INTO t11(a,b) VALUES(2,20),(3,30) RETURNING a, (SELECT count(*) FROM t11)`,
		`SELECT * FROM t11log`,
	})
	differ(t, "triggered table before-cascade with returning subquery", []string{
		`CREATE TABLE t12(a INTEGER PRIMARY KEY, b TEXT, c INTEGER)`,
		`CREATE TRIGGER tr12 BEFORE UPDATE ON t12 BEGIN UPDATE t12 SET c = c + 100 WHERE a = NEW.a; END`,
		`INSERT INTO t12 VALUES(1,'one',1),(2,'two',2)`,
		`UPDATE t12 SET b = 'updated' WHERE a IN (1,2) RETURNING a, b, c, (SELECT sum(c) FROM t12)`,
	})
}

// TestReturningTriggerConflictClause is the promoted half: RETURNING beside a
// conflict clause on a TRIGGERED table, measured against the oracle rather than
// asserted to decline.
//
// The row an IGNORE skips must produce NO RETURNING row and fire NO AFTER
// trigger, and both follow from one address: insertPlan.skipAddr is real
// SQLite's ignoreDest, which sqlite3Insert passes as endOfLoop
// (insert.c:1569-1571) and resolves at :1613 -- below the row counter (:1601)
// and below the AFTER call (:1604-1607), where RETURNING is coded as one of
// those AFTER triggers (codeReturningTrigger, trigger.c:1507). update.c does
// the same with labelContinue (:1030-1032, resolved at :1128).
func TestReturningTriggerConflictClause(t *testing.T) {
	differ(t, "or-ignore returning on a triggered table", []string{
		`CREATE TABLE t2(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2log(x)`,
		`CREATE TRIGGER tr2 AFTER INSERT ON t2 BEGIN INSERT INTO t2log VALUES(NEW.a); END`,
		`INSERT INTO t2(a,b) VALUES(1,1)`,
		`INSERT OR IGNORE INTO t2(a,b) VALUES(1,99),(2,2) RETURNING a, b`,
		`SELECT * FROM t2 ORDER BY a`,
		`SELECT * FROM t2log ORDER BY rowid`,
	})
	differ(t, "or-replace returning on a triggered table", []string{
		`CREATE TABLE t3(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t3log(x)`,
		`CREATE TRIGGER tr3 AFTER INSERT ON t3 BEGIN INSERT INTO t3log VALUES(NEW.a); END`,
		`INSERT INTO t3(a,b) VALUES(1,1)`,
		`INSERT OR REPLACE INTO t3(a,b) VALUES(1,99),(2,2) RETURNING a, b`,
		`SELECT * FROM t3 ORDER BY a`,
		`SELECT * FROM t3log ORDER BY rowid`,
	})
	differ(t, "declared on-conflict returning on a triggered table", []string{
		`CREATE TABLE t4(a INTEGER PRIMARY KEY ON CONFLICT IGNORE, b INTEGER)`,
		`CREATE TABLE t4log(x)`,
		`CREATE TRIGGER tr4 AFTER INSERT ON t4 BEGIN INSERT INTO t4log VALUES(NEW.a); END`,
		`INSERT INTO t4(a,b) VALUES(1,1)`,
		`INSERT INTO t4(a,b) VALUES(1,99),(2,2) RETURNING a, b`,
		`SELECT * FROM t4 ORDER BY a`,
		`SELECT * FROM t4log ORDER BY rowid`,
	})
	differ(t, "or-ignore update returning on a triggered table", []string{
		`CREATE TABLE t5(a INTEGER PRIMARY KEY, b INTEGER UNIQUE)`,
		`CREATE TABLE t5log(x)`,
		`CREATE TRIGGER tr5 AFTER UPDATE ON t5 BEGIN INSERT INTO t5log VALUES(NEW.b); END`,
		`INSERT INTO t5 VALUES(1,1),(2,2),(3,13)`,
		`UPDATE OR IGNORE t5 SET b=b+10 RETURNING a, b`,
		`SELECT * FROM t5 ORDER BY a`,
		`SELECT * FROM t5log ORDER BY rowid`,
	})
	// The UPSERT spelling, which was TestReturningTriggerStillDeclined -- a
	// separate test asserting `kind == "error"` on this exact statement, since
	// "upsert ... RETURNING" declined in the compiler with no trigger in sight.
	// emitUpsertTail emits the block itself now, so the shape has an ANSWER to
	// compare and a decline-assertion would only hide it being wrong. Both arms
	// and the log: the AFTER INSERT program fires for the candidate that really
	// inserted and not for the one the DO UPDATE arm took (insert.c:2361-2371
	// leaves through ignoreDest, BELOW the AFTER call at :1604-1607). The
	// full crossing, including UPDATE triggers and a BEFORE program's edit, is
	// upsert_returning_codegen_test.go.
	differ(t, "upsert returning on a triggered table", []string{
		`CREATE TABLE t6(a INTEGER PRIMARY KEY, b INTEGER DEFAULT 0)`,
		`CREATE TABLE t6log(x)`,
		`CREATE TRIGGER tr6 AFTER INSERT ON t6 BEGIN INSERT INTO t6log VALUES(NEW.a); END`,
		`INSERT INTO t6(a,b) VALUES(1,1) ON CONFLICT(a) DO UPDATE SET b=t6.b+1 RETURNING a, b`,
		`INSERT INTO t6(a,b) VALUES(1,1),(2,2) ON CONFLICT(a) DO UPDATE SET b=t6.b+1 RETURNING a, b`,
		`SELECT * FROM t6 ORDER BY a`,
		`SELECT * FROM t6log ORDER BY rowid`,
	})
}
