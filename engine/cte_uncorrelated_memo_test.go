package engine

import (
	"fmt"
	"testing"
)

// TestCTECorrelationProbeKept: a CTE referenced from several scopes, some of
// which hold a column of the body's own names, answers as the same query
// with each reference written out as a subquery, whether its body binds an
// enclosing column or not.
func TestCTECorrelationProbeKept(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1 (a INTEGER, b INTEGER)`,
		`CREATE TABLE t2 (a INTEGER, b INTEGER)`,
	}
	for i := 0; i < 40; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t1 VALUES (%d, %d)`, i, i%7), fmt.Sprintf(`INSERT INTO t2 VALUES (%d, %d)`, i%9, i))
	}
	p := newSegPair(t, stmts...)
	for _, tc := range []struct{ cte, sub string }{
		{`WITH c AS (SELECT a, b FROM t1 WHERE b > 2)
SELECT t2.a, (SELECT count(*) FROM c WHERE c.a = t2.b), (SELECT max(a) FROM c), (SELECT sum(b) FROM c, t2 AS u WHERE u.a = c.b)
FROM t2 ORDER BY 1, 2, 3, 4`,
			`SELECT t2.a, (SELECT count(*) FROM (SELECT a, b FROM t1 WHERE b > 2) c WHERE c.a = t2.b), (SELECT max(a) FROM (SELECT a, b FROM t1 WHERE b > 2) c),
 (SELECT sum(b) FROM (SELECT a, b FROM t1 WHERE b > 2) c, t2 AS u WHERE u.a = c.b)
FROM t2 ORDER BY 1, 2, 3, 4`},
		{`SELECT t2.b, (WITH c AS (SELECT t1.a + t2.a AS s FROM t1 WHERE t1.b = t2.b % 7) SELECT sum(s) FROM c),
 (WITH c AS (SELECT a AS s FROM t1 WHERE b = 3) SELECT sum(s) FROM c),
 (WITH c AS (SELECT t1.a * t2.a AS s FROM t1 WHERE t1.b = t2.a % 7) SELECT (SELECT sum(s) FROM c) + (SELECT count(*) FROM c, t1 WHERE c.s = t1.a)) FROM t2 ORDER BY 1`,
			`SELECT t2.b, (SELECT sum(s) FROM (SELECT t1.a + t2.a AS s FROM t1 WHERE t1.b = t2.b % 7)),
 (SELECT sum(s) FROM (SELECT a AS s FROM t1 WHERE b = 3)),
 (SELECT (SELECT sum(s) FROM (SELECT t1.a * t2.a AS s FROM t1 WHERE t1.b = t2.a % 7)) +
   (SELECT count(*) FROM (SELECT t1.a * t2.a AS s FROM t1 WHERE t1.b = t2.a % 7) c, t1 WHERE c.s = t1.a)) FROM t2 ORDER BY 1`},
		{`WITH c AS (SELECT a FROM t1), d AS (SELECT a FROM c WHERE a % 2 = 0)
SELECT (SELECT count(*) FROM d), (SELECT count(*) FROM d, c WHERE d.a = c.a), (SELECT count(*) FROM c)`,
			`SELECT (SELECT count(*) FROM (SELECT a FROM t1 WHERE a % 2 = 0)),
 (SELECT count(*) FROM (SELECT a FROM t1 WHERE a % 2 = 0) d, t1 c WHERE d.a = c.a), (SELECT count(*) FROM t1)`},
	} {
		got, want := p.mustFast(tc.cte), p.mustFast(tc.sub)
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n cte %.300s\n sub %.300s", tc.cte, g, w)
		}
	}
}
