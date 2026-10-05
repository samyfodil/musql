package compat

import "testing"

// TestViewInsteadOfUpdateDeleteCodegen verifies VIEW INSTEAD OF UPDATE and
// DELETE triggers compile correctly, testing materialization of view images,
// WHERE filtering, NEW row computation, trigger firing order, and WHEN guards.
func TestViewInsteadOfUpdateDeleteCodegen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		// DELETE tests.
		{"delete-all-rows-fire", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3)`,
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`}},
		{"delete-where-filters", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a,old.c); END`,
			`INSERT INTO b VALUES(1,'p'),(2,'q'),(3,'r')`,
			`DELETE FROM v WHERE a>1 AND c<>'r'`, `SELECT x,y FROM log ORDER BY rowid`}},
		// View target aliases interact with materialization.
		{"declined-target-alias-qualified-errors", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2)`,
			`DELETE FROM v AS q WHERE q.a=2`, `SELECT count(*) FROM log`}},
		{"declined-target-alias-unqualified-works", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2)`,
			`DELETE FROM v AS q WHERE a=2`, `SELECT x FROM log`}},
		{"update-target-alias-qualified-errors", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`,
			`UPDATE v AS q SET a=2 WHERE q.a=1`, `SELECT count(*) FROM log`}},
		// Materialization artifact specific to views.
		{"table-target-alias-qualified-works", []string{
			`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1),(2)`,
			`DELETE FROM t AS q WHERE q.a=1`, `SELECT a FROM t`}},
		{"delete-where-subquery", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE s(z)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3)`, `INSERT INTO s VALUES(1)`,
			`DELETE FROM v WHERE a > (SELECT count(*) FROM s)`, `SELECT x FROM log ORDER BY rowid`}},
		// Rows fire even though the body empties the base table.
		{"delete-body-empties-base-all-still-fire", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b; INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3)`,
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`, `SELECT count(*) FROM b`}},
		{"delete-body-read-is-live", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; INSERT INTO log SELECT count(*) FROM b; END`,
			`INSERT INTO b VALUES(1),(2),(3)`,
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`}},
		{"delete-when-guard", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v WHEN old.a > 2 BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3),(4)`,
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`}},
		{"delete-when-guard-subquery", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE s(z)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v WHEN (SELECT count(*) FROM s)>0 BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2)`,
			`DELETE FROM v`, `INSERT INTO s VALUES(1)`, `DELETE FROM v`,
			`SELECT x FROM log ORDER BY rowid`}},
		{"delete-raise-ignore-skips-one-row", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN SELECT RAISE(IGNORE) WHERE old.a=2; INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3)`,
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`}},
		{"delete-two-triggers-newest-first", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER v1 INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES('one'); END`,
			`CREATE TRIGGER v2 INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES('two'); END`,
			`INSERT INTO b VALUES(1)`,
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`}},
		{"delete-changes-is-zero", []string{
			`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`,
			`INSERT INTO b VALUES(1),(2)`,
			`DELETE FROM v`, `SELECT changes()`, `SELECT count(*) FROM b`}},

		// UPDATE tests.
		{"update-new-is-old-with-sets-applied", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a,new.c); END`,
			`INSERT INTO b VALUES(1,2),(3,4)`,
			`UPDATE v SET c=9`, `SELECT x,y FROM log ORDER BY rowid`}},
		{"update-sets-are-simultaneous", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a,new.c); END`,
			`INSERT INTO b VALUES(1,2)`,
			`UPDATE v SET a=c, c=a`, `SELECT x,y FROM log`}},
		{"update-unassigned-column-carries-over", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(old.a,new.a); END`,
			`INSERT INTO b VALUES(7,8)`,
			`UPDATE v SET c=1`, `SELECT x,y FROM log`}},
		// UPDATE applies view column affinity.
		{"update-applies-view-affinity", []string{
			`CREATE TABLE b(t TEXT, n INT)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v AS SELECT t,n FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(typeof(new.t),typeof(new.n)); END`,
			`INSERT INTO b VALUES('q',1)`,
			`UPDATE v SET t=5, n='7'`, `SELECT x,y FROM log`}},
		{"update-when-sees-the-converted-value", []string{
			`CREATE TABLE b(t TEXT)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT t FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v WHEN new.t='5' BEGIN INSERT INTO log VALUES('fired'); END`,
			`INSERT INTO b VALUES('q')`,
			`UPDATE v SET t=5`, `SELECT x FROM log`}},
		{"update-where-filters", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(old.a,new.c); END`,
			`INSERT INTO b VALUES(1,'p'),(2,'q'),(3,'r')`,
			`UPDATE v SET c=c||'!' WHERE a>1`, `SELECT x,y FROM log ORDER BY rowid`}},
		// UPDATE processes rows one at a time.
		{"update-is-row-at-a-time", []string{
			`CREATE TABLE base(k INTEGER PRIMARY KEY, a TEXT)`,
			`CREATE VIEW v AS SELECT k,a FROM base`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE base SET a=new.a WHERE k=old.k; END`,
			`INSERT INTO base VALUES(1,'a1'),(2,'a2'),(3,'a3')`,
			`UPDATE v SET a = 'tc=' || total_changes()`, `SELECT k,a FROM base ORDER BY k`}},
		{"update-changes-is-zero", []string{
			`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET a=new.a; END`,
			`INSERT INTO b VALUES(1),(2)`,
			`UPDATE v SET a=5`, `SELECT changes()`, `SELECT a FROM b ORDER BY rowid`}},
		{"update-last-insert-rowid-does-not-advance", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`, `SELECT last_insert_rowid()`,
			`UPDATE v SET a=2`, `SELECT last_insert_rowid()`}},

		// View shape tests.
		{"view-rename-list", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v(p,q) AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.p,new.q); END`,
			`INSERT INTO b VALUES(1,2)`,
			`UPDATE v SET q=9 WHERE p=1`, `SELECT x,y FROM log`}},
		{"view-over-a-join", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE d(z)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v AS SELECT a, z FROM b, d`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a,old.z); END`,
			`INSERT INTO b VALUES(1),(2)`, `INSERT INTO d VALUES(8),(9)`,
			`DELETE FROM v`, `SELECT x,y FROM log ORDER BY x, y`}},
		{"view-over-a-view", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW inner1 AS SELECT a FROM b`, `CREATE VIEW v AS SELECT a FROM inner1`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(4),(5)`,
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`}},
		{"view-with-an-aggregate-body", []string{
			`CREATE TABLE b(g,n)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v AS SELECT g, sum(n) AS s FROM b GROUP BY g`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.g, old.s); END`,
			`INSERT INTO b VALUES('a',1),('a',2),('b',5)`,
			`DELETE FROM v`, `SELECT x,y FROM log ORDER BY x`}},
		{"temp-view-temp-trigger", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE TEMP VIEW v AS SELECT a FROM b`,
			`CREATE TEMP TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(3)`,
			`DELETE FROM v`, `SELECT x FROM log`}},
		{"schema-qualified-target", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(6)`,
			`DELETE FROM main.v`, `SELECT x FROM log`}},
		{"cascade-into-another-view", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`, `CREATE VIEW w AS SELECT a FROM b`,
			`CREATE TRIGGER wd INSTEAD OF DELETE ON w BEGIN INSERT INTO log VALUES(old.a*10); END`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM w WHERE a=old.a; END`,
			`INSERT INTO b VALUES(4)`,
			`DELETE FROM v`, `SELECT x FROM log`}},
		// View write in trigger body.
		{"view-delete-inside-a-trigger-body", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a+100); END`,
			`CREATE TRIGGER ti AFTER INSERT ON t BEGIN DELETE FROM v; END`,
			`INSERT INTO b VALUES(1)`, `INSERT INTO t VALUES(1)`,
			`INSERT INTO b VALUES(2)`, `INSERT INTO t VALUES(2)`,
			`SELECT x FROM log ORDER BY rowid`}},

		// Empty view and no-op tests.
		{"empty-view-fires-nothing", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`DELETE FROM v`, `SELECT count(*) FROM log`, `SELECT changes()`}},
		{"where-matches-nothing", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2)`,
			`DELETE FROM v WHERE a=99`, `SELECT count(*) FROM log`}},
		// A NULL WHERE is FALSE, not TRUE.
		{"where-null-does-not-match", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(coalesce(old.a,-1)); END`,
			`INSERT INTO b VALUES(1),(NULL),(3)`,
			`DELETE FROM v WHERE a>1`, `SELECT x FROM log ORDER BY rowid`}},

		// Declined shapes must match the oracle.
		{"declined-or-clause", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`,
			`UPDATE OR IGNORE v SET a=2`, `SELECT x FROM log`}},
		{"declined-update-from", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE m(a,nv)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(old.a,new.c); END`,
			`INSERT INTO b VALUES(1,'p'),(2,'q')`, `INSERT INTO m VALUES(1,'Z')`,
			`UPDATE v SET c=m.nv FROM m WHERE m.a=v.a`, `SELECT x,y FROM log ORDER BY rowid`}},
		{"declined-no-matching-trigger", []string{
			`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`,
			`INSERT INTO v VALUES(1)`,
			`DELETE FROM v`, `UPDATE v SET a=2`, `SELECT a FROM b`, `SELECT changes()`}},
		{"declined-rowid-in-where", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1)`,
			`DELETE FROM v WHERE rowid=1`, `SELECT count(*) FROM log`}},
		{"declined-unknown-set-target", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`,
			`UPDATE v SET zz=1`, `SELECT count(*) FROM log`}},
		{"declined-view-over-missing-table", []string{
			`CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM nosuch`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`DELETE FROM v`, `SELECT count(*) FROM log`}},
	} {
		t.Run(tc.name, func(t *testing.T) { differ(t, tc.name, tc.stmts) })
	}
}
