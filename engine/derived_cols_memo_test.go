package engine

import (
	"fmt"
	"testing"
)

// TestDerivedColsMemo: derived tables nested in derived tables, under a
// compound, a window and GROUP BY, answer with the same names, types and rows
// whether a body's columns are kept from an earlier compile or not. A CTE in
// scope, used or not, turns the keeping off, so each query is asked both ways.
func TestDerivedColsMemo(t *testing.T) {
	stmts := []string{`CREATE TABLE t (x INTEGER, y REAL, d TEXT)`}
	for i := 0; i < 60; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d.5, 'd%d')`, i%9, i%13, i%4))
	}
	p := newSegPair(t, stmts...)
	before := derivedColsReuses.Load()
	for _, q := range []string{
		`SELECT * FROM (SELECT a, b FROM (SELECT x AS a, y + 1 AS b FROM t) WHERE a > 1) ORDER BY 1, 2`,
		`SELECT c, k, r FROM (SELECT 'w' AS c, s.k, s.r FROM (SELECT k, r, rank() OVER (ORDER BY r) rk FROM
 (SELECT x k, CAST(sum(y) AS REAL) / count(*) r FROM t GROUP BY x) i) s WHERE s.rk <= 3
 UNION SELECT 'v', s2.k, s2.r FROM (SELECT x k, y r FROM (SELECT x, y FROM t WHERE d = 'd1') s3) s2) ORDER BY 1, 2, 3`,
		`SELECT typeof(v), v FROM (SELECT v FROM (SELECT 1 AS v UNION ALL SELECT '2' UNION ALL SELECT 3.0)) ORDER BY 2`,
		`SELECT * FROM (SELECT g FROM (SELECT d COLLATE nocase AS g FROM t)) WHERE g = 'D1' ORDER BY 1 LIMIT 3`,
		`SELECT * FROM (SELECT * FROM (SELECT x, y FROM t) a JOIN (SELECT x AS x2, d FROM (SELECT x, d FROM t)) b ON a.x = b.x2) ORDER BY 1, 2, 4 LIMIT 7`,
		`SELECT * FROM (SELECT x + 0, d FROM (SELECT x, d FROM (SELECT x, d, y FROM t))) ORDER BY 1, 2 LIMIT 4`,
		`SELECT a.v, b.w FROM (SELECT d AS v FROM t) a, (SELECT x AS w FROM t WHERE x < 2) b WHERE a.v = 'd1' AND b.w = '1' LIMIT 3`,
		`SELECT * FROM (SELECT v FROM (SELECT d AS v FROM t)) a, (SELECT w FROM (SELECT x AS w FROM t)) b WHERE b.w = '3' AND a.v > 'd2' LIMIT 3`,
		`SELECT * FROM (SELECT v FROM (SELECT d COLLATE nocase AS v FROM t)) a, (SELECT w FROM (SELECT d AS w FROM t)) b WHERE a.v = 'D1' AND b.w = 'D1'`,
		`SELECT count(*) FROM (SELECT v FROM (SELECT d COLLATE nocase AS v FROM t)) a, (SELECT w FROM (SELECT d AS w FROM t)) b WHERE a.v = b.w`,
		`SELECT * FROM (SELECT m FROM (SELECT y AS m FROM t)) a, (SELECT n FROM (SELECT d AS n FROM t)) b WHERE a.m = '3.5' AND b.n = 'd3' LIMIT 2`,
		`SELECT * FROM (SELECT * FROM (SELECT x FROM t) WHERE x = '4') a, (SELECT * FROM (SELECT d FROM t) WHERE d = 'd0') b LIMIT 2`,
		`SELECT p.*, q.* FROM (SELECT x AS k FROM t) p JOIN (SELECT d AS k FROM t) q ON p.k = q.k`,
		`SELECT * FROM (SELECT k FROM (SELECT x AS k FROM t)) p JOIN (SELECT k FROM (SELECT CAST(x AS TEXT) AS k FROM t)) q ON p.k = q.k ORDER BY 1 LIMIT 3`,
	} {
		gc, gr, gerr := p.fast(q)
		wc, wr, werr := p.fast(`WITH unused AS (SELECT 1) ` + q)
		if gerr != nil || werr != nil {
			t.Fatalf("%s: %v / %v", q, gerr, werr)
		}
		if g, w := fmt.Sprint(gc, typedRows(gr)), fmt.Sprint(wc, typedRows(wr)); g != w {
			t.Errorf("%s:\n kept %.300s\n each %.300s", q, g, w)
		}
	}
	if derivedColsReuses.Load() == before {
		t.Error("no derived table's columns kept")
	}
}
