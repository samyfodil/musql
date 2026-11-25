package compat

import "testing"

// TestRecursiveCTEStepsAsC: recursive CTEs whose step and outer query the
// VDBE runs rather than native code answer as C does: #407's sum over a
// million-row count, a filtered aggregate, a step joining a table, a CTE
// joined back to a table, and a LIMIT.
func TestRecursiveCTEStepsAsC(t *testing.T) {
	differ(t, "recursive-cte-steps", []string{
		`CREATE TABLE e(a INTEGER, b INTEGER)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 300) INSERT INTO e SELECT v, v + 1 FROM g`,
		`WITH RECURSIVE t AS (SELECT 1 a UNION ALL SELECT a+1 FROM t WHERE a<1000000) SELECT sum(a) FROM t`,
		`WITH RECURSIVE t(a, s) AS (SELECT 1, 1 UNION ALL SELECT a+1, s+a*2 FROM t WHERE a<20000) SELECT count(*), max(s), sum(a % 7) FROM t WHERE a % 3 = 0`,
		`WITH RECURSIVE t(a) AS (SELECT 1 UNION ALL SELECT b FROM t JOIN e ON e.a = t.a) SELECT count(*), sum(a) FROM t`,
		`WITH RECURSIVE t(a) AS (SELECT 1 UNION ALL SELECT a+1 FROM t WHERE a<500) SELECT t.a, e.b FROM t, e WHERE e.a = t.a AND t.a % 97 = 0 ORDER BY 1 DESC`,
		`WITH RECURSIVE t(a) AS (SELECT 5 UNION ALL SELECT a*2 FROM t LIMIT 12) SELECT group_concat(a) FROM t`,
	})
}
