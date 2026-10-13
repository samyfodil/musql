package compat

import (
	"fmt"
	"testing"
)

// TestFKActionNestedBeforeAfterTriggerSweep: C codes a row's nested foreign
// key actions ahead of its AFTER trigger (sqlite3FkActions before the AFTER
// sqlite3CodeRowTrigger, delete.c and update.c), so a cascade chain whose
// levels carry AFTER triggers interleaves depth-first.
func TestFKActionNestedBeforeAfterTriggerSweep(t *testing.T) {
	var cases [][]string
	for _, act := range []string{"ON DELETE CASCADE", "ON DELETE SET NULL", "ON UPDATE CASCADE", "ON UPDATE SET NULL ON DELETE CASCADE"} {
		for _, trig := range []string{
			"AFTER DELETE ON ch BEGIN INSERT INTO lg VALUES('ch '||OLD.id||' '||(SELECT count(*) FROM gc)||' '||(SELECT group_concat(cid) FROM gc)); END",
			"AFTER UPDATE ON ch BEGIN INSERT INTO lg VALUES('ch '||OLD.id||' '||(SELECT group_concat(cid) FROM gc)); END",
			"AFTER UPDATE OF pid ON ch BEGIN UPDATE ch SET id = id + 100 WHERE id = NEW.id; END",
			"BEFORE DELETE ON ch BEGIN INSERT INTO lg VALUES('b '||OLD.id||' '||(SELECT count(*) FROM gc)); END",
		} {
			for _, stmt := range []string{"DELETE FROM par WHERE id=1", "UPDATE par SET id=9 WHERE id=1", "DELETE FROM par"} {
				cases = append(cases, []string{
					"PRAGMA foreign_keys=ON",
					"CREATE TABLE par(id INTEGER PRIMARY KEY)",
					"CREATE TABLE ch(id INTEGER PRIMARY KEY, pid REFERENCES par " + act + ")",
					"CREATE TABLE gc(id INTEGER PRIMARY KEY, cid REFERENCES ch " + act + ")",
					"CREATE TABLE lg(x)",
					"CREATE TRIGGER tch " + trig,
					"CREATE TRIGGER tgc AFTER DELETE ON gc BEGIN INSERT INTO lg VALUES('gc '||OLD.id||' '||(SELECT count(*) FROM ch)); END",
					"INSERT INTO par VALUES(1),(2)",
					"INSERT INTO ch VALUES(10,1),(11,1),(12,2)",
					"INSERT INTO gc VALUES(100,10),(101,11),(102,11),(103,12)",
					stmt,
					"SELECT x FROM lg ORDER BY rowid",
					"SELECT * FROM ch ORDER BY id",
					"SELECT * FROM gc ORDER BY id",
				})
			}
		}
	}
	// Each action is a trigger program: a chain deeper than
	// SQLITE_MAX_TRIGGER_DEPTH is "too many levels of trigger recursion".
	for _, n := range []int{999, 1000, 1001, 1500} {
		for _, act := range []string{"ON DELETE CASCADE", "ON DELETE SET NULL", "ON UPDATE CASCADE"} {
			cases = append(cases, []string{"PRAGMA foreign_keys=ON",
				"CREATE TABLE n(id INTEGER PRIMARY KEY, p REFERENCES n " + act + ")",
				fmt.Sprintf("WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < %d) INSERT INTO n SELECT i, NULLIF(i-1, 0) FROM c", n),
				"DELETE FROM n WHERE id = 1", "UPDATE n SET id = 0 WHERE id = 1",
				"SELECT count(*), count(p), sum(id) FROM n"})
		}
	}
	sweepAll(t, cases)
}

// TestAttachedWriteInTransactionSweep: a write to an ATTACHed database joins
// the open transaction and commits or rolls back with it, and DETACH of a
// database the transaction wrote is "database aux is locked" until it ends.
func TestAttachedWriteInTransactionSweep(t *testing.T) {
	var cases [][]string
	pre := []string{"ATTACH ':memory:' AS aux", "CREATE TABLE aux.t2(y UNIQUE)", "CREATE TABLE m(x)", "INSERT INTO m VALUES(1)"}
	for _, begin := range []string{"BEGIN", "BEGIN IMMEDIATE", "SAVEPOINT s"} {
		for _, w := range []string{
			"INSERT INTO aux.t2 VALUES(1)",
			"INSERT INTO t2 VALUES(2)",
			"INSERT INTO aux.t2 SELECT x FROM m",
			"INSERT INTO m SELECT 5 FROM aux.t2",
			"CREATE TABLE aux.t3(z)",
			"INSERT INTO aux.t2 VALUES(1),(1)",
		} {
			for _, end := range []string{"COMMIT", "ROLLBACK", "RELEASE s", "ROLLBACK TO s"} {
				s := append(append([]string(nil), pre...), begin, w, "SELECT * FROM aux.t2", "DETACH aux")
				s = append(s, "SAVEPOINT i", "INSERT INTO aux.t2 VALUES(7)", "ROLLBACK TO i", "INSERT INTO aux.t2 VALUES(8)")
				s = append(s, end, "COMMIT", "SELECT * FROM aux.t2 ORDER BY y", "SELECT * FROM m",
					"SELECT name FROM aux.sqlite_master", "DETACH aux", "SELECT count(*) FROM pragma_database_list")
				cases = append(cases, s)
			}
		}
	}
	// A table created in the transaction resolves ahead of an attachment's.
	cases = append(cases, []string{"ATTACH ':memory:' AS aux", "CREATE TABLE aux.t2(y)", "BEGIN",
		"CREATE TABLE t2(z)", "INSERT INTO t2 VALUES(1)", "SELECT * FROM main.t2", "SELECT * FROM aux.t2",
		"DROP TABLE t2", "INSERT INTO t2 VALUES(5)", "COMMIT", "SELECT * FROM aux.t2"})
	// RAISE(ROLLBACK) in an attachment's trigger unwinds main's writes too.
	cases = append(cases, []string{"ATTACH ':memory:' AS aux", "CREATE TABLE aux.t(a)",
		"CREATE TRIGGER aux.tr AFTER INSERT ON t BEGIN SELECT RAISE(ROLLBACK, 'no'); END",
		"CREATE TABLE m(x)", "BEGIN", "INSERT INTO m VALUES(1)", "INSERT INTO aux.t VALUES(1)", "COMMIT", "SELECT count(*) FROM m"})
	// A deferred foreign key in an attachment holds the COMMIT open.
	cases = append(cases, []string{"PRAGMA foreign_keys=ON", "ATTACH ':memory:' AS aux",
		"CREATE TABLE aux.p(id INTEGER PRIMARY KEY)", "CREATE TABLE aux.c(pid REFERENCES p DEFERRABLE INITIALLY DEFERRED)",
		"BEGIN", "INSERT INTO aux.c VALUES(7)", "COMMIT", "INSERT INTO aux.p VALUES(7)", "COMMIT", "SELECT * FROM aux.c"})
	sweepAll(t, cases)
}

// TestNthValueLateInvalidNSweep: C checks nth_value's N on the row being
// returned (windowReturnOneRow), so a LIMIT that stops first never reaches an
// invalid one -- unless a sorter after the window code, or the window's own
// order, reaches it first.
func TestNthValueLateInvalidNSweep(t *testing.T) {
	var cases [][]string
	pre := []string{"CREATE TABLE w(id INTEGER PRIMARY KEY, v TEXT, n, g)",
		"INSERT INTO w VALUES(1,'first',1,'a'),(2,'second',2,'b'),(3,'doom',0,'a'),(4,'x','2','b')"}
	for _, over := range []string{"ORDER BY id", "ORDER BY id DESC", "", "PARTITION BY g ORDER BY id", "ORDER BY n",
		"ORDER BY id ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING",
		"ORDER BY id ROWS BETWEEN CURRENT ROW AND CURRENT ROW EXCLUDE CURRENT ROW",
		"ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW"} {
		for _, tail := range []string{"", "ORDER BY id", "ORDER BY id DESC", "ORDER BY g", "ORDER BY g, id", "ORDER BY 1"} {
			for _, lim := range []string{"", "LIMIT 0", "LIMIT 1", "LIMIT 2", "LIMIT 3", "LIMIT 1 OFFSET 1", "LIMIT 1 OFFSET 2"} {
				for _, dist := range []string{"", "DISTINCT "} {
					cases = append(cases, append(append([]string(nil), pre...),
						fmt.Sprintf("SELECT %sid, nth_value(v, n) OVER (%s) FROM w %s %s", dist, over, tail, lim)))
				}
			}
		}
	}
	sweepAll(t, cases)
}

// TestScalarSubqueryStarOverUsingSweep: "*" over a USING/NATURAL join shows
// each common column once, so such a subquery's width is what C counts.
func TestScalarSubqueryStarOverUsingSweep(t *testing.T) {
	pre := []string{"CREATE TABLE a(x, y)", "CREATE TABLE b(x, z)", "INSERT INTO a VALUES(1,2),(3,4)", "INSERT INTO b VALUES(1,5),(3,6)",
		"CREATE TABLE v0(c1 TEXT)", "INSERT INTO v0 VALUES('a'),('a'),('b')"}
	var cases [][]string
	for _, q := range []string{
		"SELECT (SELECT * FROM a JOIN b USING(x)) = 1",
		"SELECT (SELECT * FROM a JOIN b USING(x)) = (1,2,5)",
		"SELECT (1,2,5) = (SELECT * FROM a NATURAL JOIN b)",
		"SELECT (1,2) = (SELECT * FROM a NATURAL JOIN b)",
		"SELECT 1 IN (SELECT * FROM a JOIN b USING(x))",
		"SELECT (SELECT * FROM a AS a1 JOIN a AS a2 USING(x, y)) = (1, 2)",
		"SELECT (SELECT * FROM a LEFT JOIN b USING(x) ORDER BY y DESC) = (3,4,6)",
		"SELECT * FROM a WHERE (x, y, 5) = (SELECT * FROM a JOIN b USING(x))",
		"UPDATE v0 SET c1 = c1 || '!' WHERE (SELECT * FROM v0 AS a4 JOIN v0 AS a5 USING (c1) JOIN v0 AS a6 USING (c1) ORDER BY c1, sum(1e18) OVER (ORDER BY c1)) = 0",
		"UPDATE v0 SET c1 = c1 || '!' WHERE (SELECT * FROM v0 AS a4 JOIN v0 AS a5 USING (c1) ORDER BY c1 DESC) = 'b'",
	} {
		cases = append(cases, append(append([]string(nil), pre...), q, "SELECT * FROM v0 ORDER BY rowid"))
	}
	sweepAll(t, cases)
}

// TestRenameColumnNestedViewSweep: RENAME COLUMN resolves each reference in a
// view whose SELECTs nest the way C's resolver does -- a bare name to the
// innermost SELECT whose FROM tables carry it -- so a same-named column of an
// outer table is left alone.
func TestRenameColumnNestedViewSweep(t *testing.T) {
	var cases [][]string
	for _, view := range []string{
		"SELECT u.b FROM u WHERE EXISTS (SELECT 1 FROM t WHERE b = 1)",
		"SELECT u.b FROM u WHERE EXISTS (SELECT 1 FROM t WHERE EXISTS (SELECT 1 WHERE b = 1))",
		"SELECT b FROM u WHERE EXISTS (SELECT 1 FROM t WHERE t.b = u.b AND b > 0) AND b IN (SELECT b FROM t x WHERE x.b = 2)",
		"SELECT (SELECT max(b) FROM t WHERE a = u.a) AS m, u.b FROM u JOIN t AS tt ON tt.a = u.a WHERE u.b > (SELECT b FROM t LIMIT 1)",
		"SELECT u.b AS b FROM u WHERE EXISTS (SELECT 1 FROM t WHERE b = 1)",
		"SELECT b FROM u WHERE b IN (SELECT a FROM t WHERE t.a = u.a)",
		"SELECT u.a FROM u WHERE (SELECT count(*) FROM t AS b WHERE b.b = u.b) > 0",
	} {
		for _, alter := range []string{"ALTER TABLE t RENAME COLUMN b TO c", "ALTER TABLE u RENAME COLUMN b TO c", "ALTER TABLE t RENAME COLUMN a TO c"} {
			cases = append(cases, []string{"CREATE TABLE t(a, b)", "CREATE TABLE u(b, a)",
				"INSERT INTO t VALUES(1, 2), (2, 1)", "INSERT INTO u VALUES(1, 1), (2, 2)",
				"CREATE VIEW v AS " + view, alter,
				"SELECT sql FROM sqlite_master WHERE name = 'v'", "SELECT * FROM v"})
		}
	}
	sweepAll(t, cases)
}

// TestRenameColumnTempTriggerAttachedSweep: a TEMP trigger's body reaches
// MAIN and every attachment (sqlite3FindTable), when it fires and when an
// ALTER of an attached table re-checks and rewrites it.
func TestRenameColumnTempTriggerAttachedSweep(t *testing.T) {
	var cases [][]string
	for _, body := range []string{
		"INSERT INTO lg SELECT b FROM aux.src WHERE a = new.x",
		"INSERT INTO lg SELECT b FROM src WHERE a = new.x",
		"UPDATE lg SET v = (SELECT b FROM aux.src WHERE a = new.x)",
	} {
		for _, alter := range []string{"", "ALTER TABLE aux.src RENAME COLUMN b TO c", "ALTER TABLE lg RENAME COLUMN v TO w"} {
			s := []string{"ATTACH ':memory:' AS aux", "CREATE TABLE aux.src(a INTEGER PRIMARY KEY, b)",
				"CREATE TABLE lg(v)", "CREATE TEMP TABLE drv(x)", "INSERT INTO aux.src VALUES(1, 900)",
				"CREATE TRIGGER tg AFTER INSERT ON temp.drv BEGIN " + body + "; END"}
			if alter != "" {
				s = append(s, alter)
			}
			s = append(s, "INSERT INTO temp.drv VALUES(1)", "SELECT * FROM lg", "SELECT sql FROM temp.sqlite_master WHERE type = 'trigger'")
			cases = append(cases, s)
		}
	}
	sweepAll(t, cases)
}

// TestRenameColumnOrderByBesideStarSweep: an ORDER BY term beside a bare "*"
// binds to the expanded column of that name ahead of any table column, so a
// rename leaves it alone -- and when the renamed table supplied it, the
// trigger fails its re-parse.
func TestRenameColumnOrderByBesideStarSweep(t *testing.T) {
	var cases [][]string
	for _, body := range []string{
		"SELECT 1 NOT IN (NOT EXISTS (SELECT * FROM src WHERE c1 ORDER BY b ASC, CASE WHEN b THEN 'x' WHEN b THEN 1 END))",
		"SELECT * FROM src ORDER BY b",
		"SELECT * FROM oth, src ORDER BY b",
		"SELECT * FROM oth ORDER BY z",
		"SELECT * FROM src ORDER BY c1",
	} {
		for _, temp := range []string{"", "TEMP "} {
			cases = append(cases, []string{"CREATE TEMP TABLE drv(n)", "CREATE TABLE src(c1, c2, b)", "CREATE TABLE oth(q, z)",
				"CREATE " + temp + "TRIGGER tg AFTER INSERT ON drv BEGIN " + body + "; END",
				"ALTER TABLE src RENAME COLUMN b TO bb", "ALTER TABLE oth RENAME COLUMN z TO zz",
				"SELECT sql FROM sqlite_temp_master WHERE type = 'trigger'", "SELECT sql FROM sqlite_master WHERE type = 'trigger'"})
		}
	}
	sweepAll(t, cases)
}
