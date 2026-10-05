package compat

import (
	"fmt"
	"testing"
)

// TestExistsToJoinPlansAsC: a top-level WHERE EXISTS is planned as the join
// existsToJoin (select.c:7326) makes of it. The subquery's terms then price the
// OUTER tables' loops -- "EXISTS (SELECT 1 FROM u WHERE t.c > 30)" hands t an
// index range -- and u is one more item in the join order, so where the port
// used to decline the whole statement, every order-sensitive output here now
// has to come out in C's order.
func TestExistsToJoinPlansAsC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)", "CREATE INDEX td ON t(d)", "CREATE INDEX tbd ON t(b, d)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t SELECT n, n % 5, 41 - n, n % 7 FROM s",
		"CREATE TABLE u(x, y, z)", "CREATE INDEX uy ON u(y)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<12) INSERT INTO u SELECT n % 6, n % 4, n FROM s",
		"CREATE TABLE v(p INTEGER PRIMARY KEY, q)", "INSERT INTO v VALUES(1, 3), (2, 5), (3, 1), (4, 2)",
	}
	wheres := []string{
		"EXISTS (SELECT 1 FROM u WHERE u.x = t.d)",
		"c > 3 AND EXISTS (SELECT 1 FROM u WHERE u.x = t.d)",
		"EXISTS (SELECT 1 FROM u WHERE t.c > 30)",
		"EXISTS (SELECT 1 FROM u WHERE u.y = 2)",
		"EXISTS (SELECT 1 FROM u WHERE u.x = t.d AND t.c < 10)",
		"EXISTS (SELECT * FROM u WHERE x = d)",
		"EXISTS (SELECT 1 FROM u AS t WHERE t.x = 3)",
		"EXISTS (SELECT 1 FROM t AS t2 WHERE t2.a = t.b)",
		"EXISTS (SELECT 1 FROM u WHERE u.x = t.d) AND EXISTS (SELECT 1 FROM v WHERE v.p = t.b)",
		"EXISTS (SELECT 1 FROM v WHERE v.p = t.b AND v.q > 1) AND d > 2",
		"b = 2 AND EXISTS (SELECT 1 FROM u WHERE u.y = t.d)",
		"EXISTS (SELECT 1 FROM u WHERE u.y = t.b ORDER BY z)",
		"EXISTS (SELECT 1 FROM u WHERE u.x = t.d LIMIT 1)",
		"EXISTS (SELECT max(x) FROM u WHERE u.x = t.d)",
		"NOT EXISTS (SELECT 1 FROM u WHERE u.x = t.d)",
		"(EXISTS (SELECT 1 FROM u WHERE u.x = t.d) OR c = 5)",
	}
	outs := []string{
		"SELECT group_concat(a) FROM t WHERE %s",
		"SELECT a FROM t WHERE %s LIMIT 3",
		"SELECT b, group_concat(a) FROM t WHERE %s GROUP BY b",
		"SELECT a, b FROM t WHERE %s ORDER BY b LIMIT 5",
		"SELECT group_concat(t.a || ':' || v.q) FROM t, v WHERE v.p = t.b AND %s",
	}
	for _, stat := range []bool{false, true} {
		seed := append([]string{}, base...)
		if stat {
			seed = append(seed, "ANALYZE")
		}
		for i, w := range wheres {
			for j, out := range outs {
				differ(t, fmt.Sprintf("exists %d/%d stat=%v", i, j, stat), append(append([]string{}, seed...), fmt.Sprintf(out, w)))
			}
		}
	}
}

// TestExistsToJoinPlansTheEnclosingScan: an EXISTS the port could not plan
// left its enclosing scan unplanned, so a subquery nested under it had no known
// nQueryLoop -- and over a t2 with no index at all, where C builds an automatic
// index on (w, z) and answers 0 for every row, a rowid walk answered 7,3,10,...
// Planned as the join, the enclosing scan's row count is C's again.
func TestExistsToJoinPlansTheEnclosingScan(t *testing.T) {
	base := []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER, g INTEGER, c)",
		"CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER, z, w)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t1 SELECT n, n % 7, n % 3, n % 5 FROM s",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<60) INSERT INTO t2 SELECT n, n % 9, (n * 7) % 11, n % 5 FROM s",
		"ANALYZE",
	}
	for i, q := range []string{
		"SELECT (SELECT group_concat((SELECT z FROM t2 WHERE w = mid.c LIMIT 1)) FROM t1 AS mid WHERE mid.a < t1.a + 20) FROM t1 WHERE t1.a = 2 AND EXISTS (SELECT 1 FROM t2 WHERE t2.x = t1.a)",
		"SELECT (SELECT z FROM t2 WHERE w = t1.c LIMIT 1) FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.x = t1.a)",
	} {
		differ(t, fmt.Sprintf("enclosing %d", i), append(append([]string{}, base...), q))
	}
}
