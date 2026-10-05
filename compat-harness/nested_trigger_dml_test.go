package compat

import "testing"

// Tests nested triggers where a trigger body deletes or updates a table that
// itself carries triggers.
func TestNestedTriggerDML(t *testing.T) {
	trigDiff(t, []trigDiffCase{
		{"body-delete-fires-before-and-after", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO u VALUES(1),(2),(3)`,
			`CREATE TRIGGER ud AFTER DELETE ON u BEGIN INSERT INTO log(x) VALUES('a'||old.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN DELETE FROM u WHERE a<>2; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM u ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"body-delete-before-trigger-declines-cleanly", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO u VALUES(1),(2)`,
			`CREATE TRIGGER ub BEFORE DELETE ON u BEGIN INSERT INTO log(x) VALUES('b'||old.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN DELETE FROM u WHERE a=1; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM u ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"body-update-fires-inner-update-trigger", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO u VALUES(1,'p'),(2,'p')`,
			`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN INSERT INTO log(x) VALUES(old.a||':'||old.b||'>'||new.b); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET b='q'; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a,b FROM u ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"body-update-rowid-move", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(id INTEGER PRIMARY KEY, b)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO u VALUES(1,'p')`,
			`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN INSERT INTO log(x) VALUES(old.id||'>'||new.id); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET id=9 WHERE id=1; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT id,b FROM u`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"outer-delete-body-deletes-triggered", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO t VALUES(1),(2)`, `INSERT INTO u VALUES(1),(2)`,
			`CREATE TRIGGER ud AFTER DELETE ON u BEGIN INSERT INTO log(x) VALUES('u'||old.a); END`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN DELETE FROM u WHERE a=old.a; END`,
			`DELETE FROM t`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"outer-update-body-updates-triggered", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO t VALUES(1,'p'),(2,'p')`, `INSERT INTO u VALUES(1,'m'),(2,'m')`,
			`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN INSERT INTO log(x) VALUES('u'||old.a||new.b); END`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN UPDATE u SET b=new.b WHERE a=new.a; END`,
			`UPDATE t SET b='q'`,
		}, []string{`SELECT a,b FROM t ORDER BY 1`, `SELECT a,b FROM u ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"delete-cycle-rec-off", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO t VALUES(1)`, `INSERT INTO u VALUES(1)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN DELETE FROM u WHERE a=old.a; END`,
			`CREATE TRIGGER ud AFTER DELETE ON u BEGIN DELETE FROM t WHERE a=old.a; INSERT INTO log(x) VALUES(old.a); END`,
			`DELETE FROM t WHERE a=1`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"delete-cycle-rec-on", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO t VALUES(1)`, `INSERT INTO u VALUES(1)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN DELETE FROM u WHERE a=old.a; END`,
			`CREATE TRIGGER ud AFTER DELETE ON u BEGIN DELETE FROM t WHERE a=old.a; INSERT INTO log(x) VALUES(old.a); END`,
			`DELETE FROM t WHERE a=1`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"update-cycle-rec-on-bounded", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a,b)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO t VALUES(1,0)`,
			`CREATE TRIGGER tu AFTER UPDATE ON t WHEN new.b<4 BEGIN UPDATE t SET b=new.b+1 WHERE a=new.a; INSERT INTO log(x) VALUES(new.b); END`,
			`UPDATE t SET b=1`,
		}, []string{`SELECT a,b FROM t`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"body-deletes-row-being-scanned", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO t VALUES(1),(2),(3)`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log(x) VALUES(old.a||'>'||new.a); END`,
			`UPDATE t SET a=a+10`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"three-way-mixed-dml-chain", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a,b)`, `CREATE TABLE v(a)`, `CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`INSERT INTO u VALUES(1,'p')`, `INSERT INTO v VALUES(1),(2)`,
			`CREATE TRIGGER vd AFTER DELETE ON v BEGIN INSERT INTO log(x) VALUES('vd'||old.a); END`,
			`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN DELETE FROM v WHERE a=new.a; INSERT INTO log(x) VALUES('uu'||new.b); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET b='q' WHERE a=new.a; INSERT INTO log(x) VALUES('tt'||new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a,b FROM u`, `SELECT a FROM v ORDER BY 1`, `SELECT seq,x FROM log ORDER BY seq`}},

		{"body-update-where-no-match", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1,'p')`,
			`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN INSERT INTO log VALUES(new.b); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET b='q' WHERE a=99; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a,b FROM u`, `SELECT x FROM log`}},

		{"body-delete-where-no-match", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1)`,
			`CREATE TRIGGER ud AFTER DELETE ON u BEGIN INSERT INTO log VALUES(old.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN DELETE FROM u WHERE a=99; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM u`, `SELECT x FROM log`}},

		{"body-update-declared-conflict-table", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a UNIQUE ON CONFLICT IGNORE, b)`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1,'p'),(2,'q')`,
			`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN INSERT INTO log VALUES(new.a||new.b); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET a=1 WHERE a=2; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a,b FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`}},

		// The row a trigger already deleted must not be deleted or fired for a
		// SECOND time when the outer scan reaches it. C SQLite re-seeks and
		// bypasses the whole row (delete.c:768-774); this engine had no such
		// guard, and the shape only became reachable when the nested promotion
		// stopped declining a body DELETE against a triggered table. See
		// OpNotExists (engine/vdbe_op.go).
		{"outer-scan-reaches-a-row-the-trigger-deleted", []string{
			`CREATE TABLE t1(a,b)`, `CREATE TABLE log(n)`,
			`INSERT INTO t1 VALUES(1,'a'),(2,'b'),(3,'c'),(4,'d')`,
			`CREATE TRIGGER r1 AFTER DELETE ON t1 FOR EACH ROW BEGIN DELETE FROM t1 WHERE a=old.a+2; INSERT INTO log VALUES(old.a); END`,
			`DELETE FROM t1 WHERE a=1 OR a=3`,
		}, []string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT n FROM log ORDER BY n`, `SELECT changes()`}},

		{"outer-scan-reaches-rows-a-cascade-deleted", []string{
			`CREATE TABLE t1(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(n)`,
			`INSERT INTO t1 VALUES(1),(2),(3),(4),(5)`, `INSERT INTO u VALUES(1)`,
			`CREATE TRIGGER ud AFTER DELETE ON u BEGIN DELETE FROM t1 WHERE a>2; END`,
			`CREATE TRIGGER td AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES(old.a); DELETE FROM u WHERE a=old.a; END`,
			`DELETE FROM t1`,
		}, []string{`SELECT a FROM t1 ORDER BY 1`, `SELECT n FROM log ORDER BY n`}},

		// The mirror of the DELETE case for an UPDATE scan: this one already
		// agreed before batch J (its body DELETE targets a table with no DELETE
		// trigger, so it compiled all along) and is pinned here so the two
		// loops are held to the same rule.
		{"outer-update-scan-reaches-a-row-the-trigger-deleted", []string{
			`CREATE TABLE t1(a,b)`,
			`INSERT INTO t1 VALUES(1,'a'),(2,'b'),(3,'c'),(4,'d')`,
			`CREATE TRIGGER r1 AFTER UPDATE ON t1 FOR EACH ROW BEGIN DELETE FROM t1 WHERE a=old.a+2; END`,
			`UPDATE t1 SET b='x-' || b WHERE a=1 OR a=3`,
		}, []string{`SELECT a,b FROM t1 ORDER BY a`}},

		// A BEFORE trigger whose cascade writes back into the table the
		// enclosing INSERT is filling. This compiler used to reserve the outer
		// rowid BEFORE running the BEFORE program, where insert.c:1494/1536-1539
		// runs the program FIRST -- so the body statement's own OpNewRowid got
		// the reserved rowid back and the outer insert failed "UNIQUE constraint
		// failed: tbl.rowid", and the nested slice declined the shape rather
		// than answer differently. emitInsertRowBody splits the two allocations
		// now (vdbe_write.go, insert.c:1531-1540); this case pins that the
		// ORACLE's answer is what comes out.
		{"before-insert-cascade-into-its-own-table", []string{
			`CREATE TABLE tbl(a,b,c)`,
			`CREATE TRIGGER tbl_trig BEFORE INSERT ON tbl BEGIN INSERT INTO tbl VALUES(new.a,new.b,new.c); END`,
			`INSERT INTO tbl VALUES(1,2,3)`,
		}, []string{`SELECT a,b,c FROM tbl`, `SELECT count(*) FROM tbl`}},

		{"before-insert-cascade-into-its-own-table-via-u", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO t VALUES(new.a+100); END`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`}},

		{"deep-chain-five-levels", []string{
			`CREATE TABLE t1(a)`, `CREATE TABLE t2(a)`, `CREATE TABLE t3(a)`, `CREATE TABLE t4(a)`, `CREATE TABLE t5(a)`,
			`CREATE TABLE log(seq INTEGER PRIMARY KEY, x)`,
			`CREATE TRIGGER g5 AFTER INSERT ON t5 BEGIN INSERT INTO log(x) VALUES('5:'||new.a); END`,
			`CREATE TRIGGER g4 AFTER INSERT ON t4 BEGIN INSERT INTO t5 VALUES(new.a+1); INSERT INTO log(x) VALUES('4:'||new.a); END`,
			`CREATE TRIGGER g3 AFTER INSERT ON t3 BEGIN INSERT INTO t4 VALUES(new.a+1); INSERT INTO log(x) VALUES('3:'||new.a); END`,
			`CREATE TRIGGER g2 AFTER INSERT ON t2 BEGIN INSERT INTO t3 VALUES(new.a+1); INSERT INTO log(x) VALUES('2:'||new.a); END`,
			`CREATE TRIGGER g1 AFTER INSERT ON t1 BEGIN INSERT INTO t2 VALUES(new.a+1); INSERT INTO log(x) VALUES('1:'||new.a); END`,
			`INSERT INTO t1 VALUES(0)`,
		}, []string{`SELECT seq,x FROM log ORDER BY seq`, `SELECT a FROM t5`}},
	})
}
