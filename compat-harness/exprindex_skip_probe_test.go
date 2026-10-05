// Tests that expression indexes and partial indexes are skipped when they
// cannot affect query planning.
package compat

import (
	"testing"
)

func TestExprIndexSkipProbe(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"multiple-unreferenced-expr-indexes", []string{
			`CREATE TABLE t1(a,b,c)`,
			`INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,8,9)`,
			`CREATE INDEX t1a1 ON t1(substr(a,1,12))`,
			`CREATE INDEX t1ba ON t1(b,substr(a,2,3),c)`,
			`CREATE INDEX t1abx ON t1(substr(a,b,3))`,
			`CREATE INDEX t1alen ON t1(length(a))`,
			`SELECT a, SUM(1) AS t1c, SUM(c) AS t3 FROM t1`,
		}},
		{"minmax4-shape", []string{
			`CREATE TABLE t0 (c0, c1)`,
			`CREATE INDEX i0 ON t0(c1, c1 + 1 DESC)`,
			`INSERT INTO t0(c0) VALUES (1)`,
			`SELECT MIN(t0.c1), t0.c0 FROM t0 WHERE t0.c1 ISNULL`,
		}},
		{"partial-index-no-where", []string{
			`CREATE TABLE t2(x,y)`,
			`INSERT INTO t2 VALUES(1,10),(2,20),(3,30)`,
			`CREATE INDEX t2px ON t2(x) WHERE y > 15`,
			`SELECT x, count(*) FROM t2`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			differ(t, tc.name, tc.stmts)
		})
	}
}

// TestExprIndexWhereReferenceServed: a WHERE clause that references the
// indexed expression (abs(b) = 2) is served too, and correctly -- NOT
// because this port reproduces a real index-assisted SEEK
// (whereScanInitIndexExpr, where.c:461, still not ported: it would jump
// straight to matching entries instead of visiting all of them), but
// because it does not need to. This engine's read path always visits every
// row of whichever b-tree the chosen loop names and applies WHERE as a
// per-row POST-filter, computed fresh from that row's own column values --
// never from the index's stored key bytes. So a covering-scan candidate
// that happens to also be named in a WHERE clause is filtered exactly like
// a plain table scan would filter it; only the SCAN ORDER differs (which is
// what the covering-scan representation exists to get right in the first
// place), and an unordered WHERE-filtered result is order-insensitive by
// definition. Verified directly against the oracle, with and without an
// explicit ORDER BY overriding the internal scan order.
func TestExprIndexWhereReferenceServed(t *testing.T) {
	differ(t, "expridx-where-reference", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,-2),(3,4),(5,-6)`,
		`CREATE INDEX i1 ON t1(abs(b))`,
		`SELECT a FROM t1 WHERE abs(b) = 2 ORDER BY a`,
	})
	differ(t, "expridx-where-reference-no-orderby", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,-2),(3,4),(5,-6)`,
		`CREATE INDEX i1 ON t1(abs(b))`,
		`SELECT a, count(*) FROM t1 WHERE abs(b) = 2`,
	})
}

// select5-shape (mined from select5.test#1, the actual corpus fixture): its
// GROUP BY genuinely references the index's own expression (abs(b)), so
// indexProvablyIrrelevant correctly refuses to clear it -- but with no WHERE
// clause seeking on that expression, where_plan_exprindex_cover.go's
// covering-scan representation now serves it directly, matching real
// SQLite's own choice of a covering scan over i1 (its key order is what
// determines the row a bare GROUP BY column reports).
func TestExprIndexSkipProbeGroupByReferenceServed(t *testing.T) {
	differ(t, "select5-shape", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,-2),(3,4),(5,-6)`,
		`CREATE INDEX i1 ON t1(abs(b))`,
		`SELECT quote(a), quote(b), '|' FROM t1 GROUP BY a, abs(b)`,
	})
}
