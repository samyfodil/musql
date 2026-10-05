package compat

import (
	"fmt"
	"testing"
)

// Tests nested scans inside aggregate queries use the correct nQueryLoop value.
// This affects automatic index creation and key order in group_concat and LIMIT results.
func TestNestedUnderAggregatePlansAsC(t *testing.T) {
	base := []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER, g INTEGER, c)",
		"CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER, z, w)", "CREATE INDEX i2y ON t2(y)", "CREATE INDEX i2zw ON t2(z, w)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t1 SELECT n, n % 7, n % 3, 40 - n FROM s",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<60) INSERT INTO t2 SELECT n, n % 9, n % 4, 60 - n FROM s",
		"ANALYZE",
	}
	qs := []string{}
	for _, inner := range []string{
		"SELECT group_concat(z) FROM t2 WHERE w = mid.b",
		"SELECT group_concat(z) FROM t2 WHERE w = mid.b + 50",
		"SELECT group_concat(y || z) FROM t2 WHERE w = mid.c",
		"SELECT z FROM t2 WHERE w = mid.c LIMIT 1",
	} {
		for _, mid := range []string{"mid.a = 3", "mid.a = t1.g", "mid.c > 5", "mid.b = 2", "mid.a < 3", "mid.a IN (2, 3)"} {
			qs = append(qs,
				fmt.Sprintf("SELECT g FROM t1 GROUP BY g HAVING sum(b) > (SELECT length((%s)) FROM t1 AS mid WHERE %s)", inner, mid),
				fmt.Sprintf("SELECT g, (SELECT group_concat((%s)) FROM t1 AS mid WHERE %s) FROM t1 GROUP BY g", inner, mid),
				fmt.Sprintf("SELECT sum(b), (SELECT group_concat((%s)) FROM t1 AS mid WHERE %s) FROM t1", inner, mid),
				fmt.Sprintf("SELECT (SELECT group_concat((%s)) FROM t1 AS mid WHERE %s) FROM t1 WHERE a < 3", inner, mid))
		}
	}
	for _, inner := range []string{
		"SELECT group_concat(m.a) FROM t1 AS m WHERE m.b = 3",
		"SELECT group_concat(x) FROM t2 WHERE w > 10 AND z = 2",
		"SELECT group_concat(x) FROM t2 WHERE y = 3 AND w > 5",
		"SELECT group_concat(x) FROM t2 WHERE z = 1",
		"SELECT x FROM t2 WHERE w > 10 AND z = 2 LIMIT 1",
		"SELECT x FROM t2 WHERE w < 50 LIMIT 1",
		"SELECT group_concat(m.a) FROM t1 AS m WHERE m.c > 10 AND m.b = 2",
	} {
		qs = append(qs,
			fmt.Sprintf("SELECT g, sum(b), (%s) FROM t1 GROUP BY g", inner),
			fmt.Sprintf("SELECT g FROM t1 GROUP BY g HAVING (%s) IS NOT NULL", inner),
			fmt.Sprintf("SELECT g, (SELECT count(*) FROM t1 AS mid WHERE mid.a IN (%s)) FROM t1 GROUP BY g", "SELECT x FROM t2 WHERE z = 2"),
			fmt.Sprintf("SELECT sum(b), (%s) FROM t1", inner),
			fmt.Sprintf("SELECT g FROM t1 GROUP BY g HAVING sum(b) > (SELECT length((%s)) FROM t1 AS mid WHERE mid.a = g)", inner),
			fmt.Sprintf("SELECT g, (SELECT (%s) FROM t1 AS mid WHERE mid.a = 1) FROM t1 GROUP BY g", inner),
			fmt.Sprintf("SELECT (SELECT (%s) FROM t1 AS mid WHERE mid.a = t1.a) FROM t1 WHERE a < 4", inner),
			fmt.Sprintf("SELECT (SELECT (%s) FROM t1 AS mid WHERE mid.b = t1.b LIMIT 1) FROM t1 WHERE a < 4", inner),
		)
	}
	// The same shapes where an automatic index's order is OBSERVABLE: w repeats,
	// and z is not monotonic in rowid within it.
	base2 := append(append([]string{}, base[:5]...),
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<60) INSERT INTO t2 SELECT n, n % 9, (n * 7) % 11, n % 5 FROM s",
		"ANALYZE")
	for i, q := range qs {
		differ(t, fmt.Sprintf("nested %d", i), append(append([]string{}, base...), q))
		differ(t, fmt.Sprintf("nested %d/w-repeats", i), append(append([]string{}, base2...), q))
	}
}

// TestSegGroupDeclineIsAllOrNothing: the columnar GROUP BY walk declined on a
// log row it would not read -- here the NULL -- after stepping every row
// before it, and the ordinary loop then stepped them all again into the same
// hash table: "count(*) ... GROUP BY a" answered 1,3,4,2 against C's 1,2,2,1.
func TestSegGroupDeclineIsAllOrNothing(t *testing.T) {
	fx := []string{"CREATE TABLE t(a, b, c)", "INSERT INTO t VALUES(3,'x',10)", "INSERT INTO t VALUES(1,'Y',20)",
		"INSERT INTO t VALUES(2,'x',30)", "INSERT INTO t VALUES(2,'z',40)", "INSERT INTO t VALUES(NULL,'w',50)", "INSERT INTO t VALUES(1.0,'X',60)"}
	for _, q := range []string{"SELECT count(*) FROM t GROUP BY a", "SELECT a, count(*) FROM t GROUP BY a ORDER BY a", "SELECT sum(a) FROM t GROUP BY a"} {
		differ(t, q, append(append([]string{}, fx...), q))
	}
}

// TestNestedUnderWindowProjection: a window query's projection is C's
// selectInnerLoop, coded after sqlite3WhereEnd (window.c:3039) has restored the
// pre-loop nQueryLoop (where.c:7881), so a subquery there is planned under the
// window query's own starting value. It used to be distrusted, and the inner
// scan's automatic index -- which wins at some seeds and loses at others --
// declined the statement.
func TestNestedUnderWindowProjection(t *testing.T) {
	base := []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER, g INTEGER, c)",
		"CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER, z, w)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t1 SELECT n, n % 7, n % 3, n % 5 FROM s",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<60) INSERT INTO t2 SELECT n, n % 9, (n * 7) % 11, n % 5 FROM s",
		"ANALYZE",
	}
	for i, q := range []string{
		"SELECT row_number() OVER (), (SELECT z FROM t2 WHERE w = t1.c LIMIT 1) FROM t1",
		"SELECT a, sum(b) OVER (ORDER BY a) FROM t1 ORDER BY (SELECT z FROM t2 WHERE w = t1.c LIMIT 1), a",
		"SELECT a, rank() OVER (PARTITION BY g ORDER BY b), (SELECT count(*) FROM t2 WHERE w = t1.c AND y > t1.b) FROM t1",
		"SELECT t1.a, count(*) OVER (), (SELECT z FROM t2 WHERE w = t1.c LIMIT 1) FROM t1, t2 AS o WHERE o.x = t1.a",
		"SELECT (SELECT max(r) FROM (SELECT row_number() OVER () AS r, (SELECT z FROM t2 WHERE w = t1.c LIMIT 1) AS q FROM t1 WHERE t1.g = o.y)) FROM t2 AS o WHERE o.x < 5",
		"SELECT g, count(*), (SELECT z FROM t2 WHERE w = t1.g LIMIT 1) FROM t1 GROUP BY g",
	} {
		differ(t, fmt.Sprintf("window projection %d", i), append(append([]string{}, base...), q))
	}
}

// TestNestedUnderMultiOrJoin: the shape the seed-invariance decline used to be
// pinned on. The enclosing two-table OR is planned now (WHERE_MULTI_OR inside a
// join), so the nested scan's nQueryLoop is C's and the answer is compared.
func TestNestedUnderMultiOrJoin(t *testing.T) {
	differ(t, "nested under multi-or join", []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER, g INTEGER, c)",
		"CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER, z, w)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t1 SELECT n, n % 7, n % 3, n % 5 FROM s",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<60) INSERT INTO t2 SELECT n, n % 9, (n * 7) % 11, n % 5 FROM s",
		"ANALYZE",
		"SELECT (SELECT z FROM t2 WHERE w = t1.c LIMIT 1) FROM t1, t2 AS o WHERE (t1.a = o.x OR t1.b = o.y) AND o.w = 1",
	})
}
