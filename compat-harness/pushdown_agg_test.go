package compat

import (
	"fmt"
	"testing"
)

// TestPushDownIntoAggregate: a parent's WHERE term on a GROUP BY column of an
// aggregate subquery, or of a CTE its statement names once, is pushed into it
// as C pushes it (pushDownWhereTerms onto the HAVING, then havingToWhere onto
// the WHERE), and the answers are C's (TPC-DS q78's shape). A term on a
// column the GROUP BY does not hold, or holds under another collation, stays
// in the HAVING; a CTE named twice or AS MATERIALIZED takes nothing. Where
// each term lands is TestPushDownIntoAggregateBody's (engine).
func TestPushDownIntoAggregate(t *testing.T) {
	schema := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, g INT, k INT, v INT, w TEXT COLLATE NOCASE)`,
		`CREATE INDEX t_g ON t(g)`,
		`CREATE INDEX t_w ON t(w)`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, x TEXT)`,
	}
	for i := 1; i <= 300; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %d, %d, '%c')`, i, i*7%9, i%4, i*13%23, "aBcAb"[i%5]))
	}
	for i := 1; i <= 6; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO u VALUES(%d, 'u%d')`, i, i))
	}
	for _, q := range []string{
		`WITH s AS (SELECT g, k, sum(v) AS sv FROM t GROUP BY g, k) SELECT * FROM s WHERE g = 3 ORDER BY k`,
		`WITH s AS (SELECT g AS yr, k, sum(v) AS sv, group_concat(id) AS ids FROM t GROUP BY g, k) SELECT * FROM s WHERE yr = 2 ORDER BY k`,
		`WITH s AS (SELECT g, group_concat(v) AS gc FROM t GROUP BY g) SELECT * FROM s WHERE g = 4`,
		`WITH s AS (SELECT g, sum(v) AS sv FROM t GROUP BY g) SELECT * FROM s a, s b WHERE a.g = 2 AND b.g = a.g`,
		`WITH s AS MATERIALIZED (SELECT g, sum(v) AS sv FROM t GROUP BY g) SELECT * FROM s WHERE g = 2`,
		`WITH s AS NOT MATERIALIZED (SELECT g, sum(v) AS sv FROM t GROUP BY g) SELECT * FROM s WHERE g = 2`,
		`WITH s AS (SELECT g, sum(v) AS sv FROM t GROUP BY g), r AS (SELECT * FROM s) SELECT * FROM r WHERE g = 2`,
		`SELECT * FROM (SELECT g, count(*) AS c FROM t GROUP BY g) WHERE g >= 3 ORDER BY g`,
		`SELECT * FROM (SELECT w, count(*) AS c FROM t GROUP BY w) WHERE w = 'a'`,
		`SELECT * FROM (SELECT g, k, sum(v) FROM t GROUP BY g) WHERE k = 1 ORDER BY g`,
		`SELECT * FROM (SELECT g, k, sum(v) FROM t GROUP BY g) WHERE k = 1 AND g > 2 ORDER BY g`,
		`SELECT * FROM (SELECT max(g) AS m, k FROM t) WHERE k = 2`,
		`SELECT * FROM (SELECT max(g) AS m, k FROM t) WHERE k = 3`,
		`WITH a AS (SELECT g, k, sum(v) AS sv FROM t GROUP BY g, k), b AS (SELECT g, k, count(*) AS c FROM t WHERE v > 3 GROUP BY g, k)
		  SELECT a.g, a.k, sv, c FROM a LEFT JOIN b ON b.g = a.g AND b.k = a.k WHERE (coalesce(c, 0) > 3 OR sv > 400) AND a.g = 1 ORDER BY 1, 2`,
		`SELECT * FROM u LEFT JOIN (SELECT g, count(*) AS c FROM t GROUP BY g) s ON s.g = u.id AND s.g > 1 ORDER BY u.id`,
		`SELECT * FROM (SELECT g, count(*) AS c FROM t GROUP BY g HAVING c > 30) WHERE g = 1`,
		`SELECT * FROM (SELECT g, count(*) AS c FROM t GROUP BY g) WHERE c > 30 ORDER BY g`,
		`SELECT * FROM (SELECT g + 0 AS g2, count(*) AS c FROM t GROUP BY g + 0) WHERE g2 = 1`,
	} {
		differ(t, "pushdown-agg", append(append([]string{}, schema...), q))
	}
}
