package compat

import "testing"

// TestLiveTriggerNewOldMatchesCSQLite verifies NEW/OLD references inside
// trigger bodies. Trigger reads must see rows the same statement already wrote,
// and declared collations must be preserved.
func TestLiveTriggerNewOldMatchesCSQLite(t *testing.T) {
	flLockstep(t, "NEW.* inside a body VALUES subquery", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
		`INSERT INTO s VALUES(1),(1),(2)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM s WHERE x=new.a)); END`,
		`INSERT INTO t VALUES(1)`, `INSERT INTO t VALUES(2)`, `INSERT INTO t VALUES(9)`,
	}, `SELECT n FROM log ORDER BY rowid`, `SELECT a FROM t ORDER BY a`)

	flLockstep(t, "NEW.* inside a WHEN guard's subquery", []string{
		`CREATE TABLE king(a,b,PRIMARY KEY(a))`, `CREATE TABLE prince(c,d)`,
		`CREATE TRIGGER kt AFTER INSERT ON prince WHEN NOT EXISTS (SELECT a FROM king WHERE a=new.c) BEGIN INSERT INTO king VALUES(new.c,NULL); END`,
		`INSERT INTO prince VALUES(1,2)`, `INSERT INTO prince VALUES(1,3)`, `INSERT INTO prince VALUES(2,4)`,
	}, `SELECT a,b FROM king ORDER BY a`, `SELECT c,d FROM prince ORDER BY c,d`)

	flLockstep(t, "OLD.* inside a body statement's WHERE subquery", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE s(x)`,
		`INSERT INTO t VALUES(1),(2),(3)`, `INSERT INTO s VALUES(1),(2),(3)`,
		`CREATE TRIGGER tb AFTER DELETE ON t BEGIN DELETE FROM s WHERE x IN (SELECT a FROM t WHERE a<>old.a); END`,
		`DELETE FROM t`,
	}, `SELECT x FROM s ORDER BY x`, `SELECT a FROM t ORDER BY a`)

	// Body reads see rows the same statement already wrote.
	flLockstep(t, "NEW.* subquery reads the firing instant", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t WHERE a<=new.a)); END`,
		`INSERT INTO t VALUES(1),(2),(3)`,
	}, `SELECT n FROM log ORDER BY rowid`)

	// Declared collations are preserved.
	flLockstep(t, "OLD.* subquery keeps the declared collation", []string{
		`CREATE TABLE t1(x COLLATE NOCASE PRIMARY KEY)`, `CREATE TABLE t2(y)`, `CREATE TABLE log(m)`,
		`INSERT INTO t1 VALUES('A'),('B')`, `INSERT INTO t2 VALUES('a'),('b')`,
		`CREATE TRIGGER tt1 AFTER DELETE ON t1 WHEN EXISTS (SELECT 1 FROM t2 WHERE old.x = y) BEGIN INSERT INTO log VALUES(old.x); END`,
		`DELETE FROM t1`,
	}, `SELECT m FROM log ORDER BY m`)
}

// TestLiveTriggerNewOldWriteShapesMatchCSQLite verifies NEW/OLD in write
// operations: UPDATE...FROM, bare SELECT, and INSTEAD OF view triggers.
func TestLiveTriggerNewOldWriteShapesMatchCSQLite(t *testing.T) {
	flLockstep(t, "body UPDATE...FROM whose join names NEW (triggerupfrom.test 1.0)", []string{
		`CREATE TABLE t1(a,c)`, `CREATE TABLE mp(k,v)`, `CREATE TABLE src(a)`,
		`INSERT INTO t1 VALUES(1,'old1'),(2,'old2')`, `INSERT INTO mp VALUES(1,'new1'),(2,'new2')`,
		`CREATE TRIGGER tr AFTER INSERT ON src BEGIN UPDATE t1 SET c=v FROM mp WHERE k=new.a AND a=new.a; END`,
		`INSERT INTO src VALUES(1)`, `INSERT INTO src VALUES(2)`,
	}, `SELECT a,c FROM t1 ORDER BY a`, `SELECT a FROM src ORDER BY a`)

	flLockstep(t, "body bare SELECT naming NEW", []string{
		`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`, `INSERT INTO s VALUES(1),(2)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s WHERE x=new.a; END`,
		`INSERT INTO t VALUES(1,2)`, `INSERT INTO t VALUES(7,8)`,
	}, `SELECT a,b FROM t ORDER BY a`, `SELECT x FROM s ORDER BY x`)

	flLockstep(t, "body bare SELECT naming OLD", []string{
		`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`INSERT INTO t VALUES(1,2),(3,4)`, `INSERT INTO s VALUES(1),(3)`,
		`CREATE TRIGGER tb AFTER DELETE ON t BEGIN SELECT x FROM s WHERE x=old.a; END`,
		`DELETE FROM t`,
	}, `SELECT a,b FROM t ORDER BY a`, `SELECT x FROM s ORDER BY x`)

	flLockstep(t, "body UPDATE...FROM on a VIEW whose join names NEW", []string{
		`CREATE TABLE fire(z)`, `CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TABLE m(k,nv)`,
		`INSERT INTO b VALUES(1,'b1'),(2,'b2')`, `INSERT INTO m VALUES(1,'m1'),(2,'m2')`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`,
		`CREATE TRIGGER tf AFTER INSERT ON fire BEGIN UPDATE v1 SET v=m.nv FROM m WHERE m.k=new.z; END`,
		`INSERT INTO fire VALUES(1)`,
	}, `SELECT k,v FROM b ORDER BY k`, `SELECT z FROM fire ORDER BY z`)
}
