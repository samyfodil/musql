package compat

import "testing"

// TestWindowAggregateSameFrame: rows whose frame is the previous row's (every
// row of OVER (), one peer group of a RANGE or GROUPS frame) take that row's
// value; an EXCLUDE clause gives each row a frame of its own. Over one table
// and over a join, whose row order musql cannot prove is C's.
func TestWindowAggregateSameFrame(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a INTEGER, b REAL, c TEXT)`,
		`INSERT INTO t VALUES (1, 0.1, 'x'), (2, 0.2, 'y'), (2, NULL, 'x'), (3, 1e308, NULL), (3, 1e308, 'z'), (NULL, 0.5, 'y')`,
		`CREATE TABLE u(v INTEGER)`,
		`INSERT INTO u VALUES (1), (2), (3)`,
	}
	aggs := []string{"count(*)", "count(b)", "sum(a)", "total(a)", "avg(a)", "min(c)", "max(a)", "sum(b)",
		"group_concat(c)", "count(*) FILTER (WHERE a > 1)"}
	wins := []string{
		"()",
		"(PARTITION BY c)",
		"(ORDER BY a)",
		"(PARTITION BY c ORDER BY a)",
		"(ORDER BY a RANGE BETWEEN CURRENT ROW AND CURRENT ROW)",
		"(ORDER BY a GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW)",
		"(ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW)",
		"(ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE TIES)",
		"(ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE GROUP)",
	}
	for _, from := range []string{"t", "t, u"} {
		for _, a := range aggs {
			for _, w := range wins {
				q := "SELECT " + a + " OVER " + w + " AS r FROM " + from + " ORDER BY r"
				stmts := append(append([]string{}, setup...), q)
				t.Run(q, func(t *testing.T) {
					// Over the join, an aggregate whose value depends on the
					// order it steps in declines: the scan order is not
					// provably C's (#416).
					if from == "t, u" && (a == "group_concat(c)" || a == "sum(b)") {
						differAllowingDeclines(t, "sameframe/"+q, stmts)
						return
					}
					differ(t, "sameframe/"+q, stmts)
				})
			}
		}
	}
}
