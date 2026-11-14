package compat

import "testing"

// TestRecursiveCTEOver300kRows pins recursions a WHERE ends that produce
// more rows than this engine's old 300,000-row cap, which C answers: they
// used to decline (issue #202).
func TestRecursiveCTEOver300kRows(t *testing.T) {
	queries := []string{
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x < 400000) SELECT count(*), sum(x) FROM c",
		"WITH RECURSIVE c(x, s) AS (VALUES(1, 'a') UNION ALL SELECT x+1, substr(s || x, -6) FROM c WHERE x < 1000000) SELECT count(*), max(s), sum(length(s)) FROM c",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 25), r(lv, m) AS (SELECT 1, 1 << i FROM n UNION ALL SELECT lv+1, m | (1 << i) FROM r, n WHERE lv < 4 AND (m & (1 << i)) = 0) SELECT lv, count(*), sum(m) FROM r GROUP BY lv",
	}
	for _, q := range queries {
		differ(t, q, []string{q})
	}
}
