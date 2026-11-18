package compat

import (
	"fmt"
	"testing"
)

// TestScanBelowOneRowSource: a table scanned below sources of at most one row
// each (an aggregate with no GROUP BY, as a subquery or a CTE) streams rather
// than materializes, and its rows still arrive as C's do: with no ORDER BY,
// a group_concat and the plain rows show their order.
func TestScanBelowOneRowSource(t *testing.T) {
	schema := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a INT, b TEXT)`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, a INT)`,
		`CREATE INDEX u_a ON u(a)`,
	}
	for i := 1; i <= 120; i++ {
		schema = append(schema, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, 'b%d')`, i*7%121, i%11, i))
		schema = append(schema, fmt.Sprintf(`INSERT INTO u VALUES(%d, %d)`, i, i%6))
	}
	for _, q := range []string{
		`SELECT t.id, t.b FROM (SELECT max(a) AS m FROM u) s, t WHERE t.a > s.m`,
		`SELECT group_concat(t.b) FROM (SELECT avg(a) AS m FROM u) s, t WHERE t.a < s.m`,
		`WITH m AS (SELECT max(a) AS mx FROM u) SELECT t.id, t.a FROM m, t WHERE t.a < m.mx - 2`,
		`WITH m AS (SELECT max(a) AS mx FROM u WHERE a > 100) SELECT t.id FROM m, t WHERE t.a < m.mx`,
		`WITH m AS (SELECT count(*) AS n FROM u) SELECT t.a, count(*), group_concat(t.id) FROM m, t WHERE t.a <= m.n % 7 GROUP BY t.a`,
		`SELECT t.id, u.id FROM (SELECT max(a) AS m FROM u) s, t, u WHERE t.a > s.m + 3 AND u.a = t.a - 5`,
		`SELECT t.id FROM (SELECT max(a) AS m FROM u) s LEFT JOIN t ON t.a = s.m + 4`,
		`SELECT u.id, u.a FROM (SELECT max(a) AS m FROM t) s, u WHERE u.a = s.m - 6`,
		`WITH m AS (SELECT min(a) AS lo FROM t) SELECT group_concat(u.id) FROM m, u WHERE u.a BETWEEN m.lo + 1 AND m.lo + 2`,
	} {
		differ(t, "stream-below-one-row", append(append([]string{}, schema...), q))
	}
}
