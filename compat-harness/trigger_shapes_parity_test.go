// Trigger WHEN guards, body reads, cascading writes, and OR REPLACE conflicts.
package compat

import "testing"

// TestTriggerWhenSubqueryParity -- a subquery inside a trigger's WHEN guard.
func TestTriggerWhenSubqueryParity(t *testing.T) {
	trigDiff(t, []trigDiffCase{
		{"when-count-other-table", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `CREATE TABLE s(x)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM s)>0 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO s VALUES(9)`,
			`INSERT INTO t VALUES(2,'y')`,
		}, []string{`SELECT x FROM log ORDER BY 1`}},

		{"when-counts-own-table-live", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM t)>1 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2),(3)`,
		}, []string{`SELECT x FROM log ORDER BY 1`}},

		{"when-guard-sees-body-writes", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM s)<2 BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2),(3),(4)`,
		}, []string{`SELECT x FROM log ORDER BY 1`, `SELECT a FROM s ORDER BY 1`}},

		{"when-not-exists", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE log(x)`,
			`INSERT INTO s VALUES(2)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN NOT EXISTS(SELECT 1 FROM s WHERE a=new.a) BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2),(3)`,
		}, []string{`SELECT x FROM log ORDER BY 1`}},

		{"when-subquery-delete", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1),(2),(3)`,
			`CREATE TRIGGER tw AFTER DELETE ON t WHEN (SELECT count(*) FROM t)<2 BEGIN INSERT INTO log VALUES(old.a); END`,
			`DELETE FROM t`,
		}, []string{`SELECT x FROM log ORDER BY 1`}},
	})
}

// TestTriggerBodySelectParity -- a trigger body statement that READS tables.
func TestTriggerBodySelectParity(t *testing.T) {
	trigDiff(t, []trigDiffCase{
		{"insert-select-from-other", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `CREATE TABLE s(x)`,
			`INSERT INTO s VALUES(7),(8)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT x FROM s; END`,
			`INSERT INTO t VALUES(1,'x')`,
		}, []string{`SELECT x FROM log ORDER BY 1`}},

		{"insert-select-count-own-table", []string{
			`CREATE TABLE t1(a)`, `CREATE TABLE t2(n)`,
			`CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN INSERT INTO t2 SELECT count(*) FROM t1; END`,
			`INSERT INTO t1 VALUES(1)`, `INSERT INTO t1 VALUES(2)`,
		}, []string{`SELECT n FROM t2 ORDER BY 1`}},

		{"bare-select-body-over-table", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(y)`,
			`INSERT INTO s VALUES(1)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT raise(ABORT,'no') FROM s WHERE x=new.a; END`,
			`INSERT INTO t VALUES(2)`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`}},

		{"update-set-subquery-in-body", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE c(k,n)`, `CREATE TABLE s(x)`,
			`INSERT INTO c VALUES(1,0)`, `INSERT INTO s VALUES(5),(6)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN UPDATE c SET n=(SELECT count(*) FROM s) WHERE k=1; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT k,n FROM c`}},

		{"delete-where-subquery-in-body", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE d(v)`, `CREATE TABLE s(x)`,
			`INSERT INTO d VALUES(1),(2),(3)`, `INSERT INTO s VALUES(2)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN DELETE FROM d WHERE v IN (SELECT x FROM s); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT v FROM d ORDER BY 1`}},
	})
}

// TestTriggerNestedBaseline -- a trigger body writes a table that is itself triggered.
func TestTriggerNestedBaseline(t *testing.T) {
	trigDiff(t, []trigDiffCase{
		{"two-level-insert", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,new.b); END`,
			`INSERT INTO t VALUES(1,'x')`, `INSERT INTO t VALUES(2,'y')`,
		}, []string{`SELECT x FROM log ORDER BY 1`, `SELECT a,b FROM u ORDER BY 1`}},

		{"three-level-insert", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE v(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tv AFTER INSERT ON v BEGIN INSERT INTO log VALUES(new.a*100); END`,
			`CREATE TRIGGER tu AFTER INSERT ON u BEGIN INSERT INTO v VALUES(new.a*10); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT x FROM log`, `SELECT a FROM u`, `SELECT a FROM v`}},

		{"nested-delete-from-insert-body", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1),(2)`,
			`CREATE TRIGGER ud AFTER DELETE ON u BEGIN INSERT INTO log VALUES(old.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN DELETE FROM u WHERE a=new.a; END`,
			`INSERT INTO t VALUES(2)`,
		}, []string{`SELECT x FROM log`, `SELECT a FROM u ORDER BY 1`}},

		{"nested-update-from-insert-body", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1,'p')`,
			`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN INSERT INTO log VALUES(old.b || '>' || new.b); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET b='q' WHERE a=new.a; END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT x FROM log`, `SELECT a,b FROM u`}},

		{"self-recursive-default-off", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO t VALUES(new.a+1); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`}},

		{"mutual-recursion-default-off", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a+1); END`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO t VALUES(new.a+1); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`}},

		{"nested-when-guard", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u WHEN new.a>2 BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`, `INSERT INTO t VALUES(3)`,
		}, []string{`SELECT x FROM log ORDER BY 1`}},

		{"nested-raise-abort", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN SELECT raise(ABORT,'nope'); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`}},

		{"nested-raise-ignore", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN SELECT raise(IGNORE); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM t`, `SELECT a FROM u`, `SELECT x FROM log`}},

		{"nested-changes-counter", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(changes()); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT x FROM log`, `SELECT changes()`, `SELECT total_changes()`}},

		{"nested-last-insert-rowid", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u(rowid,a) VALUES(500,new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT last_insert_rowid()`}},
	})
}

// TestTriggerReplaceVictimDeleteParity -- OR REPLACE deleting a conflicting row.
func TestTriggerReplaceVictimDeleteParity(t *testing.T) {
	trigDiff(t, []trigDiffCase{
		{"replace-rec-on", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT OR REPLACE INTO t VALUES(1,'y')`,
		}, []string{`SELECT a,b FROM t`, `SELECT x FROM log`}},

		{"replace-rec-off", []string{
			`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT OR REPLACE INTO t VALUES(1,'y')`,
		}, []string{`SELECT a,b FROM t`, `SELECT x FROM log`}},

		{"replace-before-delete-rec-on", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER td BEFORE DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT OR REPLACE INTO t VALUES(1,'y')`,
		}, []string{`SELECT a,b FROM t`, `SELECT x FROM log`}},

		{"replace-ipk-rec-on", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT OR REPLACE INTO t VALUES(1,'y')`,
		}, []string{`SELECT a,b FROM t`, `SELECT x FROM log`}},

		{"update-or-replace-rec-on", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO t VALUES(1,'x'),(2,'y')`,
			`UPDATE OR REPLACE t SET a=1 WHERE a=2`,
		}, []string{`SELECT a,b FROM t ORDER BY 1`, `SELECT x FROM log`}},
	})
}
