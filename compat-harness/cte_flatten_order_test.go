package compat

import (
	"fmt"
	"testing"
)

// TestCTEFlattenedOrder: aggregates reading the order rows arrive in, over a
// CTE that C flattens into the query (#280), answer as C's. A flattened copy
// of a column that may carry a JSON subtype loses it, as C's does.
func TestCTEFlattenedOrder(t *testing.T) {
	schema := []string{`CREATE TABLE u(a INT, s TEXT)`}
	for i := 1; i <= 120; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO u VALUES(%d, '%d')`, i, i%9))
	}
	for _, q := range []string{
		`WITH c AS NOT MATERIALIZED (SELECT a % 7 k, a v FROM u) SELECT a.v, group_concat(b.v) FROM c a, c b WHERE b.k = a.v % 9 AND a.v < 12 GROUP BY a.v`,
		`WITH c AS (SELECT a % 7 k, a v FROM u) SELECT a.v, b.v FROM c a, c b WHERE b.k = a.v % 9 AND a.v < 20 GROUP BY a.v`,
		`WITH c AS (SELECT a % 7 k, a v FROM u WHERE a < 40) SELECT group_concat(c.v) FROM c, c d WHERE d.v = c.k`,
		`WITH c AS (SELECT a % 7 k, a v FROM u) SELECT u.a, group_concat(c.v) FROM u, c WHERE c.k = u.s AND u.a < 10 GROUP BY u.a`,
		`SELECT json_group_array(v) FROM (SELECT json_object('a', a) v FROM u WHERE a < 3)`,
		`WITH c AS (SELECT a, json_object('a', a) v FROM u WHERE a < 3) SELECT json_group_object(a, v) FROM c`,
		`WITH c AS (SELECT a, json_array(a, s) v FROM u WHERE a < 4) SELECT json_array(c.v, d.v) FROM c, c d WHERE d.a = c.a + 1`,
	} {
		differ(t, "cte-flatten-order", append(append([]string{}, schema...), q))
	}
}
