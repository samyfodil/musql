package compat

import "testing"

// TestIssue203Shapes pins issue #203's two queries at a smaller size: an
// EXCEPT of one INTEGER column, whose rows dedup in an int64 set, and a
// recursive step over bit operators and a CTE join, which is inlined into
// the queue program.
func TestIssue203Shapes(t *testing.T) {
	queries := []string{
		`WITH RECURSIVE s(v) AS (SELECT 2 UNION ALL SELECT v + 1 FROM s WHERE v < 3000)
		SELECT count(*) FROM (SELECT v FROM s EXCEPT SELECT t1.v * t2.v FROM s t1, s t2 WHERE t1.v <= t2.v AND t1.v <= 55 AND t2.v <= 1500)`,
		`WITH RECURSIVE s(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM s WHERE v < 40)
		SELECT v FROM s UNION SELECT v * 1.0 FROM s INTERSECT SELECT v FROM s WHERE v % 3 = 0`,
		`WITH RECURSIVE n(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM n WHERE n < 5),
		a AS (SELECT n1.n x, n2.n y, 1 << (row_number() OVER (ORDER BY n1.n, n2.n) - 1) m FROM n n1, n n2),
		c AS (SELECT a1.m, a1.x, a1.y, sum(a2.m) m2 FROM a a1, a a2 WHERE (a1.x - a2.x) * (a1.y - a2.y) <= 0 GROUP BY a1.m, a1.x, a1.y),
		b(cnt, m, m2) AS (SELECT 1, c.m, c.m2 FROM c UNION ALL SELECT b.cnt + 1, b.m + c.m, b.m2 & c.m2 FROM b, c WHERE c.m > b.m AND b.m2 & c.m > 0)
		SELECT count(*) FROM (SELECT b.*, rank() OVER (ORDER BY cnt DESC) rnk FROM b) WHERE rnk = 1`,
	}
	for _, q := range queries {
		differ(t, q, []string{q})
	}
}
