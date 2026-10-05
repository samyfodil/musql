package compat

import (
	"fmt"
	"testing"
)

// THE PLANNER READS sqlite_stat1 THE WAY C DOES (engine's where_plan_stat1.go).
// Every statement below is one whose answer depends on the plan: a one-pass
// UPDATE whose SET reads the rows before it, group_concat and a bare column in
// scan order, and a LIMIT with no ORDER BY. Before the port each one declined
// or planned as though ANALYZE had never run. The statistics come from ANALYZE
// and from hand-written rows, which reach the planner only through "ANALYZE
// sqlite_master" -- C's own idiom, and the load point being tested.
func TestStat1DrivesThePlan(t *testing.T) {
	fill := func(n int, expr string) string {
		return fmt.Sprintf("WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<%d) INSERT INTO t SELECT %s FROM s", n, expr)
	}
	base := []string{
		"CREATE TABLE t(a, b, c, d)",
		"CREATE INDEX tc ON t(c)", "CREATE INDEX tbd ON t(b, d)", "CREATE INDEX tab ON t(a, b)",
		fill(60, "n, n % 7, 61 - n, n % 3"),
	}
	queries := []string{
		"SELECT group_concat(a) FROM t WHERE c > 10",
		"SELECT group_concat(a) FROM t WHERE c > 10 AND b > 2",
		"SELECT group_concat(a) FROM t WHERE b = 3",
		"SELECT group_concat(a) FROM t WHERE d = 1",
		"SELECT group_concat(a) FROM t WHERE b IN (1, 2) AND d > 0",
		"SELECT group_concat(a) FROM t WHERE a > 30 AND c > 20",
		"SELECT a, c, max(d) FROM t WHERE c > 5 AND b = 2",
		"SELECT a FROM t WHERE c > 5 AND b > 1 LIMIT 3",
		"SELECT b, group_concat(a) FROM t WHERE c > 0 GROUP BY b",
	}
	update := []string{
		"UPDATE t SET d = (SELECT count(*) FROM t t2 WHERE t2.d = t.d) WHERE c > 10",
		"SELECT group_concat(d) FROM (SELECT d FROM t ORDER BY a)",
	}
	stats := []struct {
		name string
		stmt []string
	}{
		{"analyze", []string{"ANALYZE"}},
		{"tiny table", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = '5 1' WHERE idx = 'tc'", "ANALYZE sqlite_master"}},
		{"huge unselective c", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = '1000000 500000' WHERE idx = 'tc'", "ANALYZE sqlite_master"}},
		{"skip-scan on b", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = '100000 20000 2' WHERE idx = 'tbd'", "ANALYZE sqlite_master"}},
		{"noskipscan", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = '100000 20000 2 noskipscan' WHERE idx = 'tbd'", "ANALYZE sqlite_master"}},
		{"unordered c", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = stat || ' unordered' WHERE idx = 'tc'", "ANALYZE sqlite_master"}},
		{"wide index rows", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = stat || ' sz=200' WHERE idx = 'tab'", "ANALYZE sqlite_master"}},
		{"a table-count row", []string{"ANALYZE", "DELETE FROM sqlite_stat1", "INSERT INTO sqlite_stat1 VALUES('t', NULL, '7')", "ANALYZE sqlite_master"}},
		{"an index with no row", []string{"ANALYZE", "DELETE FROM sqlite_stat1 WHERE idx = 'tab'", "UPDATE sqlite_stat1 SET stat = '20 3' WHERE idx = 'tc'", "ANALYZE sqlite_master"}},
		{"a row naming no index", []string{"ANALYZE", "INSERT INTO sqlite_stat1 VALUES('t', 'nosuch', '3')", "ANALYZE sqlite_master"}},
		// Written but NOT loaded: C keeps planning with the ANALYZE's rows.
		{"stale until reloaded", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = '1000000 500000' WHERE idx = 'tc'"}},
		{"IN prefers a scan", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = '100000 50000 25000' WHERE idx = 'tbd'", "ANALYZE sqlite_master"}},
	}
	for _, st := range stats {
		for i, q := range queries {
			stmts := append(append(append([]string{}, base...), st.stmt...), q)
			differ(t, fmt.Sprintf("%s/q%d", st.name, i), stmts)
		}
		stmts := append(append(append([]string{}, base...), st.stmt...), update...)
		differ(t, st.name+"/one-pass update", stmts)
	}
}

// Joins: the loop ORDER is what sqlite_stat1 moves most, and group_concat over
// a join is in nested-loop order.
func TestStat1DrivesTheJoinOrder(t *testing.T) {
	base := []string{
		"CREATE TABLE p(id INTEGER PRIMARY KEY, k, v)", "CREATE INDEX pk ON p(k)",
		"CREATE TABLE q(id INTEGER PRIMARY KEY, k, w)", "CREATE INDEX qk ON q(k)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO p SELECT n, n % 5, n FROM s",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<12) INSERT INTO q SELECT n, n % 5, n * 10 FROM s",
	}
	queries := []string{
		"SELECT group_concat(p.id || ':' || q.id) FROM p, q WHERE p.k = q.k AND p.v > 30",
		"SELECT group_concat(p.id || ':' || q.id) FROM p JOIN q ON p.k = q.k WHERE q.w < 60",
		"SELECT p.id, q.id FROM p, q WHERE p.k = q.k LIMIT 4",
	}
	stats := []struct {
		name string
		stmt []string
	}{
		{"analyze", []string{"ANALYZE"}},
		{"p looks small", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = '3 1' WHERE idx = 'pk'", "ANALYZE sqlite_master"}},
		{"q looks huge", []string{"ANALYZE", "UPDATE sqlite_stat1 SET stat = '900000 300000' WHERE idx = 'qk'", "ANALYZE sqlite_master"}},
		{"autoindex refused", []string{"DROP INDEX qk", "CREATE INDEX qkw ON q(w, k)", "ANALYZE",
			"UPDATE sqlite_stat1 SET stat = '12 6 3' WHERE idx = 'qkw'", "ANALYZE sqlite_master"}},
	}
	for _, st := range stats {
		for i, q := range queries {
			differ(t, fmt.Sprintf("%s/q%d", st.name, i), append(append(append([]string{}, base...), st.stmt...), q))
		}
	}
}

// Each cost-model arm sqlite_stat1 switches on, over statistics consistent
// enough for the arm to decide the winning index -- a skip-scan only pays when
// every index agrees the table is big, for instance. Each case is one whose
// scan order differs between the plan with the arm and the plan without it.
func TestStat1CostArmsMatchC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)",
		"CREATE INDEX tab ON t(a, b)", "CREATE INDEX tb ON t(b)", "CREATE INDEX tbd ON t(b, d)", "CREATE INDEX tc ON t(c)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<60) INSERT INTO t SELECT n % 9, n % 7, 61 - n, n % 3 FROM s",
		"ANALYZE", "DELETE FROM sqlite_stat1",
	}
	with := func(stats string, q ...string) []string {
		return append(append(append([]string{}, base...),
			"INSERT INTO sqlite_stat1 VALUES"+stats, "ANALYZE sqlite_master"), q...)
	}
	in := func(k int) string {
		s := "1"
		for i := 2; i <= k; i++ {
			s += fmt.Sprintf(",%d", i)
		}
		return s
	}
	// The IN operator's statistics test (where.c:3365-3409): a second IN on
	// the same index is dropped when scanning its prefix is cheaper, and then
	// tb can win where tab(a,b) with both INs would have.
	for _, ta := range []string{"5", "10", "50"} {
		for _, tb := range []string{"2", "20", "200"} {
			for _, ka := range []int{2, 6, 20} {
				for _, kb := range []int{2, 6, 20} {
					stats := fmt.Sprintf("('t','tab','100000 %s 1'),('t','tb','100000 %s'),('t','tbd','100000 %s 1'),('t','tc','100000 1')", ta, tb, tb)
					differ(t, fmt.Sprintf("IN ta=%s tb=%s ka=%d kb=%d", ta, tb, ka, kb),
						with(stats, fmt.Sprintf("SELECT group_concat(c) FROM t WHERE a IN (%s) AND b IN (%s)", in(ka), in(kb))))
				}
			}
		}
	}
	big := "('t','tab','100000 10 1'),('t','tb','100000 20000'),('t','tc','100000 1'),"
	for _, tbd := range []string{"'100000 20000 2'", "'100000 20000 2 noskipscan'", "'100000 50 2'"} {
		for _, q := range []string{
			"SELECT group_concat(c) FROM t WHERE d = 1",
			"SELECT group_concat(c) FROM t WHERE d > 1",
			"SELECT group_concat(c) FROM t WHERE d = 1 AND c > 20",
		} {
			differ(t, "skip-scan "+tbd+" "+q, with(big+"('t','tbd',"+tbd+")", q))
		}
	}
	// sz=: a covering index is scanned in place of the table only while its
	// rows are narrower (where.c:4239).
	for _, tb := range []string{"'100000 3'", "'100000 3 sz=500'", "'100000 3 sz=1'"} {
		differ(t, "sz "+tb, with("('t','tab','100000 10 1'),('t','tbd','100000 3 1'),('t','tc','100000 1'),('t','tb',"+tb+")",
			"SELECT group_concat(rowid) FROM t", "SELECT group_concat(rowid) FROM t WHERE b > 2",
			"SELECT group_concat(a) FROM t WHERE b > 2"))
	}
	differ(t, "sz on the table", with("('t',NULL,'100000 sz=1'),('t','tb','100000 3')", "SELECT group_concat(rowid) FROM t"))
}

// columnIsGoodIndexCandidate's second rule (where.c:884): an automatic index is
// not built on a column an analyzed index holds in a later position with more
// than 4 rows per prefix value.
func TestStat1AutoIndexCandidacyMatchesC(t *testing.T) {
	base := []string{
		"CREATE TABLE p(id INTEGER PRIMARY KEY, k, v)",
		"CREATE TABLE q(id INTEGER PRIMARY KEY, k, w)", "CREATE INDEX qwk ON q(w, k)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO p SELECT n, n % 5, n FROM s",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<30) INSERT INTO q SELECT n, n % 5, n % 4 FROM s",
		"ANALYZE", "DELETE FROM sqlite_stat1",
	}
	for _, qwk := range []string{"'30 8 1'", "'30 8 3'", "'30 8 6'", "'30 2 1'"} {
		for _, pn := range []string{"40", "4000"} {
			differ(t, "autoindex qwk="+qwk+" p="+pn, append(append([]string{}, base...),
				"INSERT INTO sqlite_stat1 VALUES('q','qwk',"+qwk+"),('p',NULL,'"+pn+"')", "ANALYZE sqlite_master",
				"SELECT group_concat(p.id || ':' || q.id) FROM p, q WHERE p.k = q.k AND p.v > 3"))
		}
	}
}

// A correlated subquery is planned as run once per outer row (pParse->nQueryLoop,
// where.c:7198), so statistics that make its table look small make an automatic
// index pay -- and then its rows arrive in that index's key order, and a
// one-pass UPDATE's subquery reads the table as the index captured it at its
// first run (OP_Once). Both were wrong here: the key was not built, and the
// read was live.
func TestStat1CorrelatedSubqueriesMatchC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<60) INSERT INTO t SELECT n, 61 - n, n % 7, n % 3 FROM s",
		"CREATE TABLE u(x, y)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<25) INSERT INTO u SELECT n % 3, 26 - n FROM s",
		"ANALYZE",
	}
	stats := [][]string{
		nil,
		{"DELETE FROM sqlite_stat1", "INSERT INTO sqlite_stat1 VALUES('t', NULL, '7')", "ANALYZE sqlite_master"},
		{"DELETE FROM sqlite_stat1", "INSERT INTO sqlite_stat1 VALUES('t', NULL, '7'), ('u', NULL, '3')", "ANALYZE sqlite_master"},
		{"DELETE FROM sqlite_stat1", "INSERT INTO sqlite_stat1 VALUES('t', 'tc', '100000 20'), ('u', NULL, '5')", "ANALYZE sqlite_master"},
		{"PRAGMA automatic_index = OFF", "DELETE FROM sqlite_stat1", "INSERT INTO sqlite_stat1 VALUES('t', NULL, '7')", "ANALYZE sqlite_master"},
		{"PRAGMA reverse_unordered_selects = ON", "DELETE FROM sqlite_stat1", "INSERT INTO sqlite_stat1 VALUES('t', NULL, '7'), ('u', NULL, '3')", "ANALYZE sqlite_master"},
	}
	queries := [][]string{
		{"SELECT a, (SELECT group_concat(t2.b) FROM t t2 WHERE t2.d = t.d) FROM t WHERE c > 3 ORDER BY a LIMIT 4"},
		{"SELECT a, (SELECT group_concat(t2.a || '.' || t2.b) FROM t t2 WHERE t2.d = t.d AND t2.c > 1) FROM t WHERE c = 2 ORDER BY a"},
		{"SELECT x, (SELECT group_concat(t.b) FROM t WHERE t.d = u.x) FROM u ORDER BY y LIMIT 3"},
		{"SELECT y, (SELECT group_concat(u2.y) FROM u u2 WHERE u2.x = u.x) FROM u ORDER BY y LIMIT 3"},
		{"SELECT a FROM t WHERE b = (SELECT max(t2.b) FROM t t2 WHERE t2.d = t.d AND t2.c < t.c) ORDER BY a"},
		{"UPDATE t SET b = (SELECT count(*) FROM t t2 WHERE t2.d = t.d AND t2.b > 20) WHERE c > 3",
			"SELECT group_concat(b) FROM (SELECT b FROM t ORDER BY a)"},
		{"UPDATE t SET b = (SELECT group_concat(t2.a) FROM t t2 WHERE t2.d = t.d AND t2.c = 1) WHERE c > 3",
			"SELECT group_concat(b, ';') FROM (SELECT b FROM t ORDER BY a)"},
		{"UPDATE u SET y = (SELECT sum(u2.y) FROM u u2 WHERE u2.x = u.x)",
			"SELECT group_concat(y) FROM (SELECT y FROM u ORDER BY rowid)"},
	}
	for si, st := range stats {
		for qi, q := range queries {
			differ(t, fmt.Sprintf("stats%d/q%d", si, qi), append(append(append([]string{}, base...), st...), q...))
		}
	}
}

// The Table and Index objects OUTLIVE a load (see engine's planStat1): a
// table's estimate, and an index's "unordered", "noskipscan" and "sz=", stay
// as an earlier load left them when the row that set them is gone; a DROP
// discards them; a CREATE starts from sqlite3DefaultRowEst of the table's
// estimate at that moment.
func TestStat1ObjectsOutliveALoad(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)", "CREATE INDEX tbd ON t(b, d)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<60) INSERT INTO t SELECT n, 61 - n, n % 7, n % 3 FROM s",
		"CREATE TABLE u(x, y)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<25) INSERT INTO u SELECT n % 3, 26 - n FROM s",
	}
	queries := []string{
		"SELECT y, (SELECT group_concat(u2.y) FROM u u2 WHERE u2.x = u.x) FROM u ORDER BY y LIMIT 3",
		"SELECT group_concat(a) FROM t WHERE c > 3",
		"SELECT group_concat(a) FROM t WHERE d = 1",
		"SELECT group_concat(rowid) FROM t WHERE b > 2",
	}
	steps := map[string][]string{
		"row deleted": {"ANALYZE", "DELETE FROM sqlite_stat1", "ANALYZE sqlite_master"},
		"unordered outlives its row": {"ANALYZE", "UPDATE sqlite_stat1 SET stat = stat || ' unordered' WHERE idx = 'tc'",
			"ANALYZE sqlite_master", "DELETE FROM sqlite_stat1 WHERE idx = 'tc'", "ANALYZE sqlite_master"},
		"skip-scan stats outlive their row": {"ANALYZE", "DELETE FROM sqlite_stat1",
			"INSERT INTO sqlite_stat1 VALUES('t','tbd','100000 20000 2'),('t','tc','100000 1')", "ANALYZE sqlite_master",
			"DELETE FROM sqlite_stat1 WHERE idx = 'tc'", "ANALYZE sqlite_master"},
		"sz outlives its row": {"ANALYZE", "UPDATE sqlite_stat1 SET stat = stat || ' sz=900' WHERE idx = 'tbd'",
			"ANALYZE sqlite_master", "UPDATE sqlite_stat1 SET stat = '60 9 3' WHERE idx = 'tbd'", "ANALYZE sqlite_master"},
		"dropped and recreated": {"ANALYZE", "UPDATE sqlite_stat1 SET stat = '100000 20000 2' WHERE idx = 'tbd'",
			"ANALYZE sqlite_master", "DROP INDEX tbd", "CREATE INDEX tbd ON t(b, d)"},
		"created after the load": {"ANALYZE", "DELETE FROM sqlite_stat1", "INSERT INTO sqlite_stat1 VALUES('t', NULL, '7')",
			"ANALYZE sqlite_master", "CREATE INDEX td ON t(d)"},
		// u has no index at the load, so nothing raised its 3-row estimate;
		// the CREATE INDEX does (sqlite3DefaultRowEst's "x<99" clamp).
		"created on an unindexed table": {"ANALYZE", "DELETE FROM sqlite_stat1",
			"INSERT INTO sqlite_stat1 VALUES('u', NULL, '3'), ('t', NULL, '7')", "ANALYZE sqlite_master", "CREATE INDEX uy ON u(y)"},
		"a fresh load forgets": {"ANALYZE", "DELETE FROM sqlite_stat1", "ANALYZE sqlite_master", "ALTER TABLE u ADD COLUMN z"},
		"rollback of DDL reloads": {"ANALYZE", "DELETE FROM sqlite_stat1", "ANALYZE sqlite_master",
			"BEGIN", "CREATE TABLE junk(q)", "ROLLBACK"},
	}
	for name, st := range steps {
		for qi, q := range queries {
			differ(t, fmt.Sprintf("%s/q%d", name, qi), append(append(append([]string{}, base...), st...), q))
		}
	}
}
