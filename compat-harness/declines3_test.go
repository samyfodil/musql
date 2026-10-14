package compat

import (
	"fmt"
	"testing"
)

// returningWithPre is t updated under a BEFORE trigger that moves s, the table
// the CTEs read, so a CTE read at the statement's start, at the first row and
// at every row give three different answers.
var returningWithPre = []string{
	"CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)",
	"CREATE TABLE s(id INTEGER PRIMARY KEY, bump INTEGER)",
	"INSERT INTO t VALUES(1,0),(2,0),(3,0)",
	"INSERT INTO s VALUES(1,1),(2,2),(3,3)",
	"CREATE TRIGGER bu BEFORE UPDATE ON t WHEN NEW.id = 1 BEGIN UPDATE s SET bump = bump + 100; END",
	"CREATE TRIGGER bd BEFORE DELETE ON t WHEN OLD.id = 1 BEGIN UPDATE s SET bump = bump + 100; END",
}

// TestReturningWithCTESweep: a RETURNING subquery sees the statement's WITH
// clause (pParse->pWith), reading the CTE where C codes it -- flattened or a
// co-routine where the subquery runs, materialized at its first run, or
// sharing UPDATE ... FROM's pass-one materialization (select.c:8075).
func TestReturningWithCTESweep(t *testing.T) {
	ctes := []string{
		"c(id, tot) AS (SELECT id, bump FROM s)",
		"c(id, tot) AS (SELECT id, sum(bump) FROM s GROUP BY id)",
		"c(id, tot) AS MATERIALIZED (SELECT id, bump FROM s)",
		"c(id, tot) AS NOT MATERIALIZED (SELECT id, bump FROM s)",
		"c(id, tot) AS (SELECT DISTINCT id, bump FROM s)",
		"c(id, tot) AS (SELECT 2, 42)",
	}
	rets := []string{
		"(SELECT tot FROM c WHERE c.id = t.id)",
		"(SELECT max(tot) FROM c)",
		"(SELECT count(*) FROM c)",
		"(SELECT sum(c.tot) FROM c, c AS d WHERE d.id = c.id)",
		"(SELECT c.tot FROM c, c AS d WHERE c.id = t.id AND d.id = c.id)",
		"(SELECT tot FROM (SELECT 1), c WHERE c.id = t.id)",
	}
	stmts := []string{
		"UPDATE t SET v = 1 RETURNING id, %s",
		"UPDATE t SET v = 1 FROM c WHERE c.id = t.id RETURNING id, %s",
		"UPDATE t SET v = c.tot FROM c WHERE t.id = c.id + 0 RETURNING id, v, %s",
		"DELETE FROM t WHERE id < 3 RETURNING id, %s",
	}
	var cases [][]string
	for _, cte := range ctes {
		for _, r := range rets {
			for _, st := range stmts {
				cases = append(cases, append(append([]string(nil), returningWithPre...),
					"WITH "+cte+" "+fmt.Sprintf(st, r), "SELECT * FROM t ORDER BY id", "SELECT * FROM s ORDER BY id"))
			}
		}
	}
	for _, q := range []string{
		"WITH c(x) AS (SELECT 42) UPDATE t SET v = v + s.bump FROM s WHERE t.id = s.id RETURNING id, (SELECT x FROM c)",
		"WITH c(sum_bump) AS (SELECT sum(bump) FROM s) UPDATE t SET v = 0 FROM c WHERE t.id > 0 RETURNING id, v, (SELECT sum_bump FROM c)",
		"WITH c(sum_bump) AS (SELECT sum(bump) FROM s) UPDATE t SET v = 0 WHERE t.id > 0 RETURNING id, v, (SELECT sum_bump FROM c)",
		"WITH c(id, bump) AS (SELECT id, bump FROM s) UPDATE t SET v = src.bump FROM c AS src WHERE t.id = src.id RETURNING id, (SELECT bump FROM c WHERE c.id = t.id)",
		"WITH s AS (SELECT 99 AS bump) UPDATE t SET v = 1 RETURNING id, (SELECT bump FROM s)",
		"WITH s AS (SELECT 99 AS bump) DELETE FROM t WHERE id = 2 RETURNING id, (SELECT bump FROM s)",
		"WITH c(id, tot) AS (SELECT id, sum(bump) FROM s GROUP BY id) UPDATE t SET v = 6 RETURNING id, (SELECT tot FROM c WHERE c.id = t.id), (SELECT max(tot) FROM c)",
		"WITH c(id, tot) AS (SELECT id, sum(bump) FROM s GROUP BY id) UPDATE t SET v = 7 RETURNING id, (SELECT max(tot) FROM c), (SELECT min(tot) FROM c)",
		"WITH c(id, tot) AS (SELECT id, bump FROM s), d AS (SELECT 1) UPDATE t SET v = 9 FROM d RETURNING id, (SELECT tot FROM c WHERE c.id = t.id)",
		"WITH c(x) AS (SELECT 5) DELETE FROM t WHERE id = 3 RETURNING id, (SELECT x FROM c)",
		"WITH c(x) AS (SELECT 7) INSERT INTO t VALUES(9, 9) RETURNING (SELECT x FROM c)",
		"WITH c(x, y) AS (SELECT 1, 2) INSERT INTO t VALUES(9, 9) RETURNING (SELECT x FROM c), (SELECT y FROM c)",
		"WITH c(x) AS (SELECT 42) INSERT INTO t VALUES(9, (SELECT x FROM c)) RETURNING (SELECT x + 1 FROM c)",
		"WITH c(x) AS (SELECT count(*) FROM s) INSERT INTO t VALUES(8, 0), (9, 0) RETURNING id, (SELECT x FROM c)",
		"WITH c(x) AS (SELECT count(*) FROM t) INSERT INTO t VALUES(8, 0), (9, 0) RETURNING id, (SELECT x FROM c), (SELECT x + 1 FROM c)",
		"WITH c(x) AS (SELECT 1) INSERT INTO t SELECT 9, 9 RETURNING (SELECT x FROM c)",
		"WITH s AS (SELECT 77 AS bump) INSERT INTO t VALUES(8, 8) RETURNING (SELECT bump FROM s)",
		"WITH c(id, tot) AS (SELECT id, bump FROM s LIMIT 5) UPDATE t SET v = 1 RETURNING id, (SELECT tot FROM c WHERE c.id = t.id)",
		"WITH c(id, tot) AS (SELECT id, bump FROM s LIMIT 5) DELETE FROM t WHERE id < 3 RETURNING id, (SELECT tot FROM c WHERE c.id = t.id)",
		// Nothing is pushed into an AS MATERIALIZED CTE.
		"WITH c(id, tot) AS MATERIALIZED (SELECT id, sum(bump) FROM s GROUP BY id) UPDATE t SET v = c.tot FROM c WHERE t.id > 0 AND c.id = 1 RETURNING id, v, (SELECT tot FROM c WHERE c.id = t.id), (SELECT count(*) FROM c)",
	} {
		cases = append(cases, append(append([]string(nil), returningWithPre...), q, "SELECT * FROM t ORDER BY id"))
	}
	sweepAll(t, cases)
}

// TestReturningWithCTEDeclines holds the RETURNING-over-WITH shapes still
// declined: the moment C fills a materialization another part of the statement
// shares, other than UPDATE ... FROM's single FROM item, is not followed, nor
// is a materialized CTE read per row beside a live table.
func TestReturningWithCTEDeclines(t *testing.T) {
	for i, q := range []string{
		"WITH c(id, tot) AS (SELECT id, sum(bump) FROM s GROUP BY id) UPDATE t SET v = 8 RETURNING id, (SELECT x.tot FROM s, c AS x WHERE x.id = s.id AND s.id = t.id)",
		"WITH c(id, tot) AS (SELECT id, sum(bump) FROM s GROUP BY id) DELETE FROM t WHERE id IN (SELECT id FROM c) RETURNING id, (SELECT tot FROM c WHERE c.id = t.id)",
		"WITH c(x) AS (SELECT count(*) FROM t) INSERT INTO t VALUES((SELECT x FROM c), 0), (6, 0) RETURNING id, (SELECT x FROM c)",
		"INSERT INTO t WITH c(x) AS (SELECT 1) SELECT 9, 9 RETURNING (SELECT x FROM c)",
		// Whether pass one flattens a CTE with a LIMIT is not decided.
		"WITH c(id, tot) AS (SELECT id, bump FROM s LIMIT 5) UPDATE t SET v = 1 FROM c WHERE c.id = t.id RETURNING id, (SELECT tot FROM c WHERE c.id = t.id)",
		// Pass one pushed "c.id = 1" into the materialization RETURNING shares.
		"WITH c(id, tot) AS (SELECT id, sum(bump) FROM s GROUP BY id) UPDATE t SET v = c.tot FROM c WHERE t.id > 0 AND c.id = 1 RETURNING id, v, (SELECT tot FROM c WHERE c.id = t.id)",
	} {
		differAllowingDeclines(t, fmt.Sprintf("decline-%d", i), append(append([]string(nil), returningWithPre...), q))
	}
}

// TestNthValueReaderReachSweep: a window select read through a FROM clause
// is a co-routine of its reader, and a scalar subquery or EXISTS reads one
// row, so an invalid nth_value N in a row the reader never takes raises
// nothing (windowReturnOneRow).
func TestNthValueReaderReachSweep(t *testing.T) {
	pre := []string{"CREATE TABLE w(id INTEGER PRIMARY KEY, v TEXT, n, g)",
		"INSERT INTO w VALUES(1,'first',1,'a'),(2,'second',2,'b'),(3,'doom',0,'a'),(4,'x','2','b')"}
	var cases [][]string
	for _, over := range []string{"ORDER BY id", "ORDER BY id DESC", "", "PARTITION BY g ORDER BY id", "ORDER BY n",
		"ORDER BY id ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING"} {
		win := fmt.Sprintf("SELECT id, nth_value(v, n) OVER (%s) AS nv FROM w", over)
		win1 := fmt.Sprintf("SELECT nth_value(v, n) OVER (%s) FROM w", over)
		for _, q := range []string{
			"SELECT id FROM (%[1]s)", "SELECT id FROM (%[1]s) LIMIT 1", "SELECT id FROM (%[1]s) LIMIT 2",
			"SELECT id FROM (%[1]s) LIMIT 3", "SELECT id FROM (%[1]s) LIMIT 0", "SELECT id FROM (%[1]s) LIMIT 1 OFFSET 1",
			"SELECT id FROM (%[1]s) LIMIT 2 OFFSET 1", "SELECT id, nv FROM (%[1]s) LIMIT 2", "SELECT id FROM (%[1]s ORDER BY id) LIMIT 2",
			"SELECT id FROM (%[1]s LIMIT 3) LIMIT 2", "SELECT id FROM (SELECT id FROM (%[1]s)) LIMIT 2",
			"SELECT count(*) FROM (%[1]s)", "SELECT id FROM (%[1]s LIMIT 1) LIMIT 2", "SELECT id FROM (%[1]s LIMIT -1 OFFSET 1) LIMIT 2",
			"SELECT EXISTS (%[1]s)", "SELECT NOT EXISTS (%[1]s)", "SELECT (SELECT id FROM (%[1]s))",
			"SELECT (%[2]s)", "SELECT 1 IN (%[2]s)", "SELECT (SELECT nv FROM (%[1]s) LIMIT 1 OFFSET 1)",
		} {
			cases = append(cases, append(append([]string(nil), pre...), fmt.Sprintf(q, win, win1)))
		}
	}
	// The outer ORDER BY repeating the window's sort is dropped
	// (sqlite3WindowRewrite) by resolved column, however it is qualified.
	for _, sp := range [][2]string{
		{"ORDER BY id", "w.id"}, {"ORDER BY w.id", "id"}, {"ORDER BY w.id", "main.w.id"}, {"ORDER BY id", "1"},
		{"ORDER BY id DESC", "w.id DESC"}, {"ORDER BY id", "w.id DESC"}, {"PARTITION BY g ORDER BY id", "w.g, w.id"},
		{"PARTITION BY g ORDER BY id", "g, w.id"}, {"PARTITION BY g ORDER BY id", "w.id"}, {"ORDER BY id", "w.n"},
	} {
		for _, lim := range []string{"LIMIT 1", "LIMIT 2", "LIMIT 3", ""} {
			cases = append(cases, append(append([]string(nil), pre...),
				fmt.Sprintf("SELECT id, nth_value(v, n) OVER (%s) FROM w ORDER BY %s %s", sp[0], sp[1], lim)))
		}
		for _, lim := range []string{"LIMIT 1", "LIMIT 2", "LIMIT 3"} {
			cases = append(cases, append(append([]string(nil), pre...),
				fmt.Sprintf("SELECT q.id, nth_value(v, n) OVER (%s) FROM w AS q ORDER BY q.id %s", sp[0], lim)))
		}
	}
	sweepAll(t, cases)
}

// TestNthValueReaderReachDeclines: readers whose row count is not fixed by
// their text -- a WHERE, a join, a CTE -- still take every row.
func TestNthValueReaderReachDeclines(t *testing.T) {
	pre := []string{"CREATE TABLE w(id INTEGER PRIMARY KEY, v TEXT, n, g)",
		"INSERT INTO w VALUES(1,'first',1,'a'),(2,'second',2,'b'),(3,'doom',0,'a'),(4,'x','2','b')"}
	for i, q := range []string{
		"SELECT id FROM (SELECT id, nth_value(v, n) OVER (ORDER BY id) AS nv FROM w) WHERE id < 3 LIMIT 2",
		"SELECT x.id FROM w AS x, (SELECT id, nth_value(v, n) OVER (ORDER BY id) AS nv FROM w) AS y WHERE x.id = y.id LIMIT 1",
		"SELECT DISTINCT id FROM (SELECT id, nth_value(v, n) OVER (ORDER BY id) AS nv FROM w) LIMIT 2",
		"WITH c AS (SELECT id, nth_value(v, n) OVER (ORDER BY id) AS nv FROM w) SELECT id FROM c LIMIT 2",
	} {
		differAllowingDeclines(t, fmt.Sprintf("decline-%d", i), append(append([]string(nil), pre...), q))
	}
}

// TestRenameColumnStarCollisionSweep: an ORDER BY term beside a bare "*"
// binds to the first expanded column of its name (resolveAsName), so a
// rename leaves it whichever FROM tables carry the name; when the renamed
// table was the only one, the re-parse fails.
func TestRenameColumnStarCollisionSweep(t *testing.T) {
	var cases [][]string
	for _, from := range []string{"oth, src", "src, oth", "src AS x, oth", "oth JOIN src ON oth.z = src.c1"} {
		for _, alter := range []string{"ALTER TABLE src RENAME COLUMN b TO bb", "ALTER TABLE oth RENAME COLUMN b TO bb"} {
			for _, both := range []bool{false, true} {
				s := []string{"CREATE TEMP TABLE drv(n)", "CREATE TABLE src(c1, c2, b)", "CREATE TABLE oth(b, z)",
					"CREATE VIEW v AS SELECT * FROM " + from + " ORDER BY b",
					"CREATE TRIGGER tg AFTER INSERT ON drv BEGIN SELECT * FROM " + from + " ORDER BY b; END",
					alter}
				if both {
					s = append(s, "ALTER TABLE src RENAME COLUMN c2 TO b")
				}
				// Not "SELECT * FROM v": with two expanded b's this engine
				// reports the ORDER BY ambiguous where C takes the first.
				s = append(s, "SELECT name, sql FROM sqlite_master ORDER BY name", "SELECT sql FROM sqlite_temp_master", "INSERT INTO drv VALUES(1)")
				cases = append(cases, s)
			}
		}
	}
	sweepAll(t, cases)
}

// TestRenameColumnDerivedAliasSweep: a view's reference to a derived table's
// column that the subquery names by an alias binds to that column, which the
// rename leaves (alter.c:1022), while references inside the subquery to the
// renamed table are rewritten.
func TestRenameColumnDerivedAliasSweep(t *testing.T) {
	var cases [][]string
	for _, view := range []string{
		"SELECT x.b FROM (SELECT a AS b FROM t) AS x",
		"SELECT b FROM (SELECT a AS b FROM t)",
		"SELECT x.b, t.b FROM (SELECT a AS b FROM t) AS x, t",
		"SELECT x.b FROM (SELECT b AS b FROM t) AS x",
		"SELECT x.b FROM (SELECT t.b + 1 AS b FROM t WHERE b > 0) AS x WHERE x.b > 0 ORDER BY b",
		"SELECT x.b FROM (SELECT a AS b FROM t) AS x WHERE EXISTS (SELECT 1 FROM t WHERE t.b = x.b)",
		"SELECT y.b FROM (SELECT a AS b FROM t) AS x, (SELECT d AS b FROM u) AS y",
		"SELECT b FROM (SELECT a AS b, b AS q FROM t)",
		"SELECT x.b FROM (SELECT b FROM t) AS x",
		"SELECT x.b FROM (SELECT * FROM t) AS x",
		"SELECT (SELECT x.b FROM (SELECT t.a AS b) AS x) FROM t",
		"SELECT (SELECT x.b FROM (SELECT a AS b FROM t) AS x) FROM u",
		"SELECT x.b FROM (SELECT a AS b FROM t) AS x GROUP BY b",
		"SELECT x.b, b FROM (SELECT a AS b FROM t) AS x ORDER BY b",
	} {
		for _, alter := range []string{"ALTER TABLE t RENAME COLUMN b TO c", "ALTER TABLE t RENAME COLUMN a TO c", "ALTER TABLE u RENAME COLUMN b TO c"} {
			cases = append(cases, []string{"CREATE TABLE t(a, b)", "CREATE TABLE u(b, d)",
				"INSERT INTO t VALUES(1, 2)", "INSERT INTO u VALUES(3, 4)",
				"CREATE VIEW v AS " + view, alter,
				"SELECT sql FROM sqlite_master WHERE name = 'v'", "SELECT * FROM v"})
		}
	}
	sweepAll(t, cases)
}
