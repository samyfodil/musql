package compat

import (
	"fmt"
	"testing"
)

// TestGroupByHashDescKeys: a GROUP BY whose ORDER BY is as long as it pushes
// its DESC bits onto the grouping keys (sqlite3CopySortOrder), so the groups
// come out descending in those keys and the ORDER BY's ties keep that order.
// The ORDER BY keys tie heavily, the grouping keys mix storage classes and
// NULLs, and the shapes include a join, a HAVING subquery, LIMIT/OFFSET and a
// UTF-16 database.
func TestGroupByHashDescKeys(t *testing.T) {
	base := []string{
		`CREATE TABLE s(id INTEGER PRIMARY KEY, n INTEGER)`,
		`CREATE TABLE t(a, b, s INTEGER, v REAL)`,
	}
	for i := 1; i <= 6; i++ {
		base = append(base, fmt.Sprintf(`INSERT INTO s VALUES(%d, %d)`, i, i%3))
	}
	as := []string{"1", "2", "2.0", "'2'", "'x'", "'X'", "x'01'", "NULL", "-1", "3.5", "''", "10"}
	for i := 0; i < 400; i++ {
		base = append(base, fmt.Sprintf(`INSERT INTO t VALUES(%s, %d, %d, %d.25)`, as[i%len(as)], i%4, i%6+1, i%7))
	}
	queries := []string{
		`SELECT a, count(*) FROM t GROUP BY a ORDER BY count(*) DESC`,
		`SELECT a, count(*) AS c FROM t GROUP BY a ORDER BY c`,
		`SELECT a, b, sum(v) FROM t GROUP BY a, b ORDER BY count(*) DESC, b`,
		`SELECT a, b, sum(v) FROM t GROUP BY a, b ORDER BY b, count(*) DESC`,
		`SELECT a, b, sum(v) FROM t GROUP BY a, b ORDER BY count(*) DESC, sum(v) DESC`,
		`SELECT b, a, total(v) FROM t GROUP BY b, a ORDER BY b % 2 DESC, 1 DESC`,
		`SELECT a, count(*) FROM t GROUP BY a ORDER BY count(*) DESC LIMIT 4 OFFSET 2`,
		`SELECT t.a, sum(t.v * s.n) AS value FROM t, s WHERE t.s = s.id AND s.n = 1 GROUP BY t.a
			HAVING sum(t.v * s.n) > (SELECT sum(t.v * s.n) * 0.05 FROM t, s WHERE t.s = s.id AND s.n = 1) ORDER BY value DESC`,
		`SELECT t.b, count(*) FROM t JOIN s ON s.id = t.s GROUP BY t.b ORDER BY s.n DESC`,
		`SELECT a, max(v) FROM t WHERE b > 0 GROUP BY a ORDER BY 'k' DESC`,
		`SELECT a, b, count(*) FROM t GROUP BY a HAVING count(*) > (SELECT count(*) FROM s) / 2 ORDER BY count(*) DESC`,
		`SELECT a, count(*) FROM t GROUP BY a HAVING (SELECT max(n) FROM s WHERE s.id = t.s) > 0 ORDER BY count(*) DESC`,
		`SELECT a, sum(v) FROM t GROUP BY a HAVING sum(v) > (SELECT sum(v) FROM t WHERE b = 1) / 12 ORDER BY 2 DESC`,
		`SELECT a, count(*) FROM t GROUP BY a HAVING EXISTS (SELECT 1 FROM s WHERE n = 2) ORDER BY (SELECT count(*) FROM s) DESC, 2`,
		`SELECT a, count(*) FROM t GROUP BY a HAVING count(*) % 7 IN (SELECT n FROM s) OR max(s) IN (SELECT id FROM s WHERE n = 0) ORDER BY 2 DESC`,
		`SELECT a, count(*), (SELECT count(*) FROM s WHERE s.id = t.s) FROM t GROUP BY a HAVING count(*) > (SELECT 3) ORDER BY 2 DESC`,
		`SELECT b, min(v), s FROM t GROUP BY b HAVING max(v) > (SELECT min(n) FROM s) ORDER BY b DESC`,
	}
	for _, q := range []string{
		`SELECT key, group_concat(json_quote(value)) FROM json_each('[[7],[8,9]]') GROUP BY key ORDER BY key DESC`,
		`SELECT key, json_group_array(value) FROM json_each('[[7],{"a":1}]') GROUP BY key ORDER BY key DESC`,
	} {
		differ(t, "groupby-hash-desc-subtype", []string{q})
	}
	for _, q := range queries {
		differ(t, "groupby-hash-desc", append(append([]string{}, base...), q))
		differ(t, "groupby-hash-desc-utf16", append(append([]string{`PRAGMA encoding = 'UTF-16le'`}, base...), q))
	}
}
