package compat

import "testing"

// The edges of batch J's nested-trigger promotion: catalogs, table shapes and
// row counts a cascade can now reach that it could not before. Every case is
// here because the promotion made a NEW code path reachable for it, not because
// the shape is otherwise interesting.
func TestNestedTriggerEdges(t *testing.T) {
	trigDiff(t, []trigDiffCase{
		{"inner-table-without-rowid", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.k||':'||new.v); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES('k'||new.a, new.a); END`,
			`INSERT INTO t VALUES(1),(2)`,
		}, []string{`SELECT k,v FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`}},

		{"inner-table-with-unique-index", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a,b)`, `CREATE UNIQUE INDEX ux ON u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,'v'); END`,
			`INSERT INTO t VALUES(1),(2)`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a,b FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`}},

		{"temp-inner-main-outer", []string{
			`CREATE TABLE t(a)`, `CREATE TEMP TABLE u(a)`, `CREATE TEMP TABLE log(x)`,
			`CREATE TEMP TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TEMP TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`,
		}, []string{`SELECT a FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`}},

		// A same-named table in BOTH catalogs, with the cascade running in temp.
		// Deliberately unqualified: a schema-qualified body statement is a
		// separate, pre-existing gap -- C SQLite allows one inside a TEMP
		// trigger and rejects it elsewhere ("qualified table names are not
		// allowed on INSERT, UPDATE, and DELETE statements within triggers"),
		// while this engine rejects it in both, which has nothing to do with
		// nesting and is not what this battery is measuring.
		{"same-name-main-and-temp-tables", []string{
			`CREATE TABLE u(a)`, `CREATE TABLE t(a)`,
			`CREATE TEMP TABLE u(a)`, `CREATE TEMP TABLE log(x)`,
			`CREATE TEMP TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a*2); END`,
			`CREATE TEMP TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
		}, []string{`SELECT a FROM main.u ORDER BY 1`, `SELECT a FROM temp.u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`}},

		{"many-rows-through-two-levels", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a*3); END`,
			`INSERT INTO t VALUES(1),(2),(3),(4),(5),(6),(7),(8),(9),(10)`,
		}, []string{`SELECT count(*),sum(a) FROM u`, `SELECT count(*),sum(x) FROM log`}},

		{"cascade-into-autoincrement-then-reopen", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(id INTEGER PRIMARY KEY AUTOINCREMENT, a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.id); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u(a) VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2),(3)`,
			`DELETE FROM u`,
			`INSERT INTO t VALUES(4)`,
		}, []string{`SELECT id,a FROM u`, `SELECT x FROM log ORDER BY 1`, `SELECT seq FROM sqlite_sequence WHERE name='u'`}},

		{"cascade-with-collation-and-affinity", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a TEXT COLLATE NOCASE UNIQUE)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES('Ab')`,
			`INSERT INTO t VALUES('aB')`,
		}, []string{`SELECT a FROM u`, `SELECT x FROM log`}},

		{"cascade-inside-explicit-transaction-committed", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`BEGIN`, `INSERT INTO t VALUES(1)`, `INSERT INTO t VALUES(2)`, `COMMIT`,
		}, []string{`SELECT a FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`, `PRAGMA integrity_check`}},

		{"cascade-then-savepoint-rollback", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`,
			`SAVEPOINT s1`, `INSERT INTO t VALUES(2)`, `ROLLBACK TO s1`, `RELEASE s1`,
			`INSERT INTO t VALUES(3)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`}},

		{"cascade-writes-a-table-with-a-check", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a CHECK(a<3))`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`,
			`INSERT INTO t VALUES(9)`,
		}, []string{`SELECT a FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`, `SELECT x FROM log ORDER BY 1`}},

		{"cascade-updates-the-outer-table-mid-scan", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE u(a)`,
			`INSERT INTO t VALUES(1,'p'),(2,'p'),(3,'p')`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN UPDATE t SET b='z' WHERE a=new.a+1; END`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`UPDATE t SET b='q' WHERE a=1`,
		}, []string{`SELECT a,b FROM t ORDER BY 1`, `SELECT a FROM u ORDER BY 1`}},
	})
}
