package compat

import "testing"

// TestInConvertsGroupValue: C applies an IN subquery's comparison affinity to
// the group's own register, so a later read of that value in the aggregate's
// result sees it converted. Where nothing converts (a rowid search, BLOB
// affinity, values already of the type) or nothing reads the value again, the
// answers match; where a conversion would show, musql declines rather than
// print the unconverted value.
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
		differAllowingDeclines(t, "in-group-value-converts", append(append([]string{}, base...), q))
	}
}
