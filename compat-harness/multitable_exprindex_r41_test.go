// Expression indexes in multi-table statements: no longer blanket-declined.
package compat

import "testing"

// Fixture with expression index; used in multi-table join.
func r41ExprIndexSetup() []string {
	return []string{
		`CREATE TABLE t1(a INT, b TEXT, c INT, d INT)`,
		`INSERT INTO t1(a,b,c,d) VALUES
			(1, '{"x":1}', 12,  3),
			(1, '{"x":2}',  4,  5),
			(1, '{"x":1}',  6, 11),
			(2, '{"x":1}', 22,  3),
			(2, '{"x":2}',  4,  5),
			(3, '{"x":1}',  6,  7)`,
		`CREATE INDEX t1x ON t1(d, a, b->>'x', c)`,
		`CREATE TABLE t2(y)`,
		`INSERT INTO t2(y) VALUES(9)`,
	}
}

func TestR41MultiTableExprIndex(t *testing.T) {
	cases := []struct {
		name, sql string
	}{
		// The exact mined statement: t2 CROSS JOIN t1.
		{"tkt-99378-111-t2-cross-t1", `
			SELECT if(a,a,y),
			       SUM(1)                              AS t1,
			       SUM(CASE WHEN b->>'x'=1 THEN 1 END) AS t2,
			       SUM(c)                              AS t3,
			       SUM(CASE WHEN b->>'x'=1 THEN c END) AS t4
			  FROM t2 CROSS JOIN t1
			 WHERE d BETWEEN 0 and 10
			 GROUP BY a`},
		// The OTHER FROM order -- t1 (the indexed table) now drives the join
		// instead of trailing it. wherePlanMultiTableOrder's own solver, not
		// literal FROM order, decides the nesting either way; both orders
		// must reach the SAME answer C SQLite gives.
		{"tkt-99378-111-t1-cross-t2", `
			SELECT if(a,a,y),
			       SUM(1)                              AS t1,
			       SUM(CASE WHEN b->>'x'=1 THEN 1 END) AS t2,
			       SUM(c)                              AS t3,
			       SUM(CASE WHEN b->>'x'=1 THEN c END) AS t4
			  FROM t1 CROSS JOIN t2
			 WHERE d BETWEEN 0 and 10
			 GROUP BY a`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			differ(t, tc.name, append(append([]string{}, r41ExprIndexSetup()...), tc.sql))
		})
	}
}

// TestR41MultiTableExprIndexOrderDisagrees rules out coincidental agreement:
// t1x's own key order (ascending d) and t1's rowid (insertion) order
// DISAGREE on which row of the GROUP BY a=1 group comes first, and c -- a
// bare column outside both the GROUP BY key and any aggregate -- is read
// straight from whichever row the chosen scan visits first. A wrong access
// path (or the pre-round-41 fallback to plain rowid order) reports a
// DIFFERENT "c" than the oracle's; verified directly against it that the
// correct value is 111 (the small-d row), not 999 (the small-rowid row).
func TestR41MultiTableExprIndexOrderDisagrees(t *testing.T) {
	differ(t, "exprindex-order-disagrees", []string{
		`CREATE TABLE t1(a INT, b TEXT, c INT, d INT)`,
		`INSERT INTO t1(a,b,c,d) VALUES
			(1, '{"x":9}', 999, 50),
			(1, '{"x":1}', 111, 3)`,
		`CREATE INDEX t1x ON t1(d, a, b->>'x', c)`,
		`CREATE TABLE t2(y)`,
		`INSERT INTO t2 VALUES(9)`,
		`SELECT if(a,a,y), c, SUM(1) FROM t2 CROSS JOIN t1 WHERE d BETWEEN 0 and 100 GROUP BY a`,
	})
}

// TestR41MultiTablePartialIndexOnClause is the class round 41's fix left
// declined: a PARTIAL index on a table joined with >=1 other source.
// whereUsablePartialIndex (where.c:3700) tests sqlite3WhereBegin's ONE shared
// WhereClause, which for two or more FROM items also holds every INNER join's
// own ON conjunct (sqlite3ProcessJoin, select.c:657). Here t1_partial's
// condition (b>5) is EXACTLY t1's ON conjunct, so C picks "SEARCH t1 USING
// INDEX t1_partial (b>?)" -- ascending b, the b=8 row before the b=20 one,
// opposite of rowid order -- and the bare column c reports it.
//
// Round 41 had to decline it, because indexProvablyIrrelevant never sees an ON
// clause and serving rowid order was measured to be a WRONG answer
// (c="HIGH_B_LOW_ROWID" where the oracle says "LOW_B_HIGH_ROWID"). The
// implication proof is now ported over the planner's own WhereClause, ON terms
// included (engine/where_plan_partial.go), so the shape is answered -- and must
// answer the oracle's row.
func TestR41MultiTablePartialIndexOnClause(t *testing.T) {
	differ(t, "r41 partial index implied by an ON clause", []string{
		`CREATE TABLE t1(a INT, b INT, c TEXT)`,
		`INSERT INTO t1 VALUES (1,20,'HIGH_B_LOW_ROWID'),(1,8,'LOW_B_HIGH_ROWID')`,
		`CREATE INDEX t1_partial ON t1(b) WHERE b > 5`,
		`CREATE TABLE t2(y)`,
		`INSERT INTO t2 VALUES(9)`,
		`SELECT a, c, SUM(b) FROM t2 JOIN t1 ON b > 5 GROUP BY a`,
	})
}
