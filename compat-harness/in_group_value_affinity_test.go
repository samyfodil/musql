package compat

import "testing"

// TestInConvertsGroupValue: C applies an IN subquery's comparison affinity to
// the group's own register, so a later read of that value in the aggregate's
// result sees it converted (#126): in the IN's own item, in HAVING then the
// select list, in an ORDER BY key coded after the columns, in a subquery body
// reading the group. The order the items and their operands are coded in is
// C's, so the conversion shows exactly where it does there.
func TestInConvertsGroupValue(t *testing.T) {
	base := []string{
		`CREATE TABLE s(id INTEGER PRIMARY KEY, n INTEGER, x TEXT)`,
		`CREATE TABLE t(a, b INTEGER, r REAL)`,
		`INSERT INTO s VALUES(1,1,'1'),(2,2,'2')`,
		`INSERT INTO t VALUES(2,1,2.0),(2.0,1,3.0),('2',2,2.5),(1,3,1.0),('1',3,1)`,
	}
	exact := []string{
		`SELECT count(*) FROM t GROUP BY a HAVING a IN (SELECT n FROM s) ORDER BY 1`,
		`SELECT a, typeof(a), count(*) FROM t GROUP BY a HAVING a IN (SELECT id FROM s)`,
		`SELECT a, typeof(a) FROM t GROUP BY a HAVING a IN (SELECT x FROM s)`,
		`SELECT a IN (SELECT rowid FROM s ORDER BY 1), typeof(a) FROM t GROUP BY a`,
		`SELECT a IN (SELECT s.rowid FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT a IN (SELECT q.id FROM s AS q), typeof(a) FROM t GROUP BY a`,
		`SELECT b, sum(r), typeof(b) FROM t GROUP BY b HAVING b IN (SELECT n FROM s)`,
		`SELECT r, typeof(r) FROM t GROUP BY r HAVING r IN (1, 2, 3)`,
		`SELECT (a, b) IN (SELECT n, n FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT a, typeof(a) FROM t WHERE a IN (SELECT n FROM s)`,
		`SELECT count(*), typeof(a) FROM t WHERE a IN (SELECT n FROM s) GROUP BY b`,
		`SELECT b, count(*) FROM t GROUP BY b HAVING max(r) IN (SELECT n FROM s)`,
		`SELECT b, count(*) FROM t GROUP BY a, b HAVING a IN (SELECT n FROM s) ORDER BY 1, 2`,
		`SELECT a+0, typeof(a+0) FROM t GROUP BY a+0 HAVING (a+0) IN (SELECT n FROM s)`,
		`SELECT count(*) FROM t HAVING max(a) IN (SELECT n FROM s)`,
	}
	for _, q := range exact {
		differ(t, "in-group-value", append(append([]string{}, base...), q))
	}
	converts := []string{
		`SELECT a, count(*) FROM t GROUP BY a HAVING a IN (SELECT n FROM s) ORDER BY 2, 1`,
		`SELECT a, a IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT typeof(a), a IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a ORDER BY 1`,
		`SELECT a IN (SELECT n FROM s) AND typeof(a) = 'integer' FROM t GROUP BY a`,
		`SELECT +a COLLATE nocase IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT a IN (SELECT id FROM s WHERE 1), typeof(a) FROM t GROUP BY a`,
		`SELECT b, max(a), typeof(max(a)) FROM t GROUP BY b HAVING max(a) IN (SELECT n FROM s)`,
		`SELECT max(a), typeof(max(a)) FROM t HAVING max(a) IN (SELECT n FROM s)`,
		`SELECT r, typeof(r) FROM t GROUP BY r HAVING r IN (SELECT n FROM s)`,
		`SELECT typeof(a) FROM t GROUP BY a, typeof(a) HAVING a IN (SELECT n FROM s)`,
		`SELECT b, typeof(a) FROM t GROUP BY b HAVING a IN (SELECT n FROM s)`,
		`SELECT b, max(b), typeof(max(b)) FROM t GROUP BY b HAVING max(b) IN (SELECT r FROM t)`,
		`SELECT +a IN (SELECT x FROM s), typeof(a) FROM t GROUP BY a`,
	}
	for _, q := range converts {
		differ(t, "in-group-value-converts", append(append([]string{}, base...), q))
	}
	// NULL, a BLOB and a TEXT no number reads, in groups of every shape.
	more := []string{
		`CREATE TABLE s(id INTEGER PRIMARY KEY, n INTEGER, x TEXT)`,
		`CREATE TABLE t(a, b INTEGER, r REAL)`,
		`INSERT INTO s VALUES(1,1,'1'),(2,2,'2')`,
		`INSERT INTO t VALUES(2,1,2.0),(2.0,1,3.0),('2',2,2.5),(1,3,1.0),('1',3,1),(NULL,4,NULL),(x'31',5,4.0),('abc',6,2.0)`,
	}
	for _, q := range []string{
		`SELECT DISTINCT typeof(a), a IN (SELECT n FROM s) FROM t GROUP BY a ORDER BY 1, 2`,
		`SELECT a, typeof(a) FROM t GROUP BY a HAVING a IN (SELECT n FROM s) LIMIT 2 OFFSET 1`,
		`SELECT typeof(a) AS ta, a IN (SELECT n FROM s) AS i, typeof(a) FROM t GROUP BY a ORDER BY ta`,
		`SELECT typeof(a), a IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a ORDER BY 1, 1`,
		`SELECT typeof(a), a IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a ORDER BY a`,
		`SELECT typeof(a), a IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a ORDER BY 3 DESC, 2`,
		`SELECT a, a NOT IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT a, a IN (SELECT n FROM s WHERE 0), typeof(a) FROM t GROUP BY a`,
		`SELECT a, a IN (SELECT n FROM s WHERE n > 5), typeof(a) FROM t GROUP BY a`,
		`SELECT r, r IN (SELECT n FROM s), typeof(r) FROM t GROUP BY r`,
		`SELECT r, r IN (SELECT x FROM s), typeof(r) FROM t GROUP BY r`,
		`SELECT a, a COLLATE nocase IN (SELECT x FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT b, CASE WHEN max(a) IN (SELECT n FROM s) THEN typeof(max(a)) END, typeof(max(a)) FROM t GROUP BY b`,
		`SELECT b, max(a) IN (SELECT n FROM s), typeof(max(a)), typeof(min(a)) FROM t GROUP BY b`,
		`SELECT count(*), typeof(a) FROM t GROUP BY b HAVING a IN (SELECT n FROM s)`,
		`SELECT b, a, typeof(a) FROM t GROUP BY b HAVING a IN (SELECT x FROM s) ORDER BY 1`,
		`SELECT a, json_type(a), a IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT a, a IN (SELECT n FROM s) OR typeof(a) = 'text', typeof(a) FROM t GROUP BY a`,
		`SELECT a, typeof(a) = 'text' OR a IN (SELECT n FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT a, (a IN (SELECT n FROM s)) + 0, typeof(a) FROM t GROUP BY a`,
		`SELECT a, a IN (SELECT n FROM s), a IN (SELECT x FROM s), typeof(a) FROM t GROUP BY a`,
		`SELECT a, typeof(a) FROM t GROUP BY a HAVING a IN (SELECT n FROM s) OR b > 4`,
		`SELECT a, typeof(a) FROM t GROUP BY a HAVING b > 1 OR a IN (SELECT n FROM s)`,
		`SELECT sum(b), typeof(a), a FROM t GROUP BY a HAVING a IN (SELECT n FROM s)`,
		`SELECT max(a), typeof(max(a)) FROM t WHERE b < 4 HAVING max(a) IN (SELECT x FROM s)`,
		`SELECT max(a), typeof(max(a)), max(a) IN (SELECT n FROM s), typeof(max(a)) FROM t WHERE b = 2`,
		`SELECT a, row_number() OVER (), typeof(a) FROM t GROUP BY a HAVING a IN (SELECT n FROM s)`,
		`SELECT a, (SELECT typeof(a)), a IN (SELECT n FROM s) FROM t GROUP BY a`,
		`SELECT a, a IN (SELECT n FROM s), (SELECT typeof(a)) FROM t GROUP BY a`,
		`SELECT b, sum(r) IN (SELECT x FROM s), typeof(sum(r)) FROM t GROUP BY b`,
		`SELECT a || '', a IN (SELECT n FROM s), a || '', typeof(a || '') FROM t GROUP BY a`,
		`SELECT (a IN (SELECT n FROM s)) || typeof(a), typeof(a) FROM t GROUP BY a`,
		`SELECT typeof(a) || (a IN (SELECT n FROM s)), typeof(a) FROM t GROUP BY a`,
		`SELECT (a IN (SELECT n FROM s)) + length(typeof(a)), typeof(a) FROM t GROUP BY a`,
		`SELECT NULL + (a IN (SELECT n FROM s)), typeof(a) FROM t GROUP BY a`,
		`SELECT (a IN (SELECT n FROM s)) + NULL, typeof(a) FROM t GROUP BY a`,
		`SELECT 1 + (a IN (SELECT n FROM s)), typeof(a) FROM t GROUP BY a`,
		`SELECT (a IN (SELECT n FROM s)) = (typeof(a) = 'integer'), typeof(a) FROM t GROUP BY a`,
		`SELECT (a IN (SELECT n FROM s)) IS (typeof(a) = 'integer'), typeof(a) FROM t GROUP BY a`,
		`SELECT CASE WHEN typeof(a) = 'text' THEN a IN (SELECT n FROM s) END, typeof(a) FROM t GROUP BY a`,
		`SELECT CASE a IN (SELECT n FROM s) WHEN 1 THEN typeof(a) ELSE 'no' END, typeof(a) FROM t GROUP BY a`,
		`SELECT coalesce(a IN (SELECT n FROM s), 9), typeof(a) FROM t GROUP BY a`,
		`SELECT a, typeof(a) FROM t GROUP BY a HAVING (a IN (SELECT n FROM s)) = 1`,
		`SELECT a, typeof(a) FROM t GROUP BY a HAVING typeof(a) = 'text' AND a IN (SELECT n FROM s)`,
		`SELECT a, typeof(a) FROM t GROUP BY a HAVING NOT (a IN (SELECT n FROM s)) OR typeof(a) = 'integer'`,
		`SELECT a, typeof(a), count(*) FROM t GROUP BY a HAVING a IN (SELECT n FROM s) ORDER BY 2 DESC, 3`,
		`SELECT a, typeof(a) FROM t GROUP BY a HAVING a IN (SELECT n FROM s) ORDER BY a DESC`,
		`SELECT b, typeof(a) FROM t GROUP BY b HAVING a IN (SELECT n FROM s) ORDER BY b DESC`,
	} {
		differ(t, "in-group-value-shapes", append(append([]string{}, more...), q))
	}
}
