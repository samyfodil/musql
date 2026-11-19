package compat

import (
	"fmt"
	"testing"
)

// TestCTESortedSeek: a CTE joined on an automatic index's seek (#277), which
// native code makes once the rows are sorted. Runs of one key and of several,
// keys that are not all INTEGERs, probes that are not INTEGERs, misses, an
// empty CTE and a LEFT JOIN's NULL row answer as C's.
func TestCTESortedSeek(t *testing.T) {
	schema := []string{`CREATE TABLE u(a INT, s TEXT)`}
	for i := 1; i <= 60; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO u VALUES(%d, '%d')`, i, i%9))
	}
	g := func(n int) string {
		return fmt.Sprintf(`g(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM g WHERE i < %d)`, n)
	}
	fifth := `SELECT i, i*i*i*i*i i5 FROM g`
	five := `SELECT a.i, b.i, c.i, d.i, e.i FROM t a, t b, t c, t d, t e
		WHERE a.i < b.i AND b.i < c.i AND c.i < d.i AND d.i < e.i AND a.i5 + b.i5 + c.i5 + d.i5 = e.i5`
	four := `SELECT a.i, b.i, c.i, d.i FROM t a, t b, t c, t d WHERE a.i < b.i AND b.i < c.i AND a.i5 + b.i5 + c.i5 - %d = d.i5`
	for _, q := range []string{
		fmt.Sprintf(`WITH RECURSIVE %s, t AS MATERIALIZED (%s) %s`, g(45), fifth, five),
		fmt.Sprintf(`WITH RECURSIVE %s, t AS NOT MATERIALIZED (%s) %s`, g(45), fifth, five),
		fmt.Sprintf(`WITH RECURSIVE %s, t AS MATERIALIZED (%s) `+four, g(30), fifth, 0),
		fmt.Sprintf(`WITH RECURSIVE %s, t AS (%s) `+four, g(30), fifth, 1),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT i %% 7 k, i v FROM g)
			SELECT a.v, count(*), sum(b.v), min(b.v), max(b.v) FROM c a, c b WHERE b.k = a.v %% 9 GROUP BY a.v`, g(120)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT i %% 7 k, i v FROM g)
			SELECT a.v, b.v FROM c a, c b WHERE b.k = a.v - 100`, g(120)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT i %% 7 k, i v FROM g)
			SELECT a.v, b.v FROM c a, c b WHERE b.k = a.v %% 9`, g(40)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT CASE WHEN i %% 5 = 0 THEN NULL ELSE i %% 7 END k, i v FROM g)
			SELECT a.v, count(*), sum(b.v), min(b.v), max(b.v) FROM c a, c b WHERE b.k = a.v %% 9 GROUP BY a.v`, g(80)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT CASE WHEN i %% 5 = 0 THEN '3' ELSE i %% 7 END k, i v FROM g)
			SELECT a.v, count(*), sum(b.v), min(b.v), max(b.v) FROM c a, c b WHERE b.k = a.v %% 9 GROUP BY a.v`, g(80)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT i %% 7 k, i v, (i %% 4) || '' s FROM g)
			SELECT a.v, count(*), sum(b.v), min(b.v), max(b.v) FROM c a, c b WHERE b.k = a.s GROUP BY a.v`, g(80)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT i %% 7 k, i v FROM g)
			SELECT a.v, count(*), sum(b.v), min(b.v), max(b.v) FROM c a, c b WHERE b.k = a.v * 0.5 GROUP BY a.v`, g(80)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT i * -3000000000 k, i v FROM g)
			SELECT a.v, b.v FROM c a, c b WHERE b.k = a.v * -6000000000`, g(80)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT i k FROM g WHERE i > 1000), d AS MATERIALIZED (SELECT i k FROM g)
			SELECT x.k, y.k, c.k FROM d x, d y, c WHERE c.k = x.k + y.k`, g(40)),
		fmt.Sprintf(`WITH RECURSIVE %s, c AS MATERIALIZED (SELECT i %% 7 k, i v FROM g)
			SELECT a.v, b.v FROM c a LEFT JOIN c b ON b.k = a.v %% 11 WHERE a.v < 30`, g(60)),
		`WITH c AS MATERIALIZED (SELECT a % 7 k, a v FROM u) SELECT u.a, count(*), sum(c.v), min(c.v) FROM u, c WHERE c.k = u.a % 5 GROUP BY u.a`,
		`WITH c AS MATERIALIZED (SELECT a % 7 k, a v FROM u) SELECT u.a, count(*), sum(c.v), min(c.v) FROM u, c WHERE c.k = u.s GROUP BY u.a`,
	} {
		differ(t, "cte-sorted-seek", append(append([]string{}, schema...), q))
	}
}
