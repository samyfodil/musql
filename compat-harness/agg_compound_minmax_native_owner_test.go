// Tests a fix for a wrong answer when a native aggregate (GROUP BY or
// select-list aggregate) owns a min/max in a subquery. Previously read the
// anchor row's value instead of aggregating the whole group.
package compat

import "testing"

// TestAggCompoundMinMaxNativeOwnerDeclinesNotWrong checks that previously
// wrong answers now decline cleanly.
func TestAggCompoundMinMaxNativeOwnerDeclinesNotWrong(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"group-by-owner-compound-max", []string{
			"CREATE TABLE t1(g, v)",
			"INSERT INTO t1 VALUES (1,10),(1,20),(2,30),(2,5)",
			"SELECT g, (SELECT 999 UNION SELECT max(v)) FROM t1 GROUP BY g ORDER BY g",
		}},
		{"select-list-aggregate-owner-compound-max", []string{
			"CREATE TABLE t1(a)",
			"INSERT INTO t1 VALUES (1),(2),(3)",
			"SELECT sum(a), (SELECT 999 UNION SELECT max(a)) FROM t1",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differAllowingDeclines(t, c.name, c.stmts)
		})
	}
}

// TestAggCompoundMinMaxNativeOwnerAdjacentShapes tests min() and HAVING clauses.
func TestAggCompoundMinMaxNativeOwnerAdjacentShapes(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"group-by-owner-compound-min", []string{
			"CREATE TABLE t1(g, v)",
			"INSERT INTO t1 VALUES (1,10),(1,20),(2,30),(2,5)",
			// The placeholder must be LARGER than every real v (999, not
			// -999): UNION (not UNION ALL) sorts its combined, DISTINCT rows
			// ascending, and a scalar subquery context reads only the FIRST
			// one -- so a placeholder smaller than min(v) would sort first
			// and mask the whole shape (min(v)'s own row never gets read).
			// Same placeholder direction agg_compound_minmax_hoist_test.go's
			// own "min-instead-of-max" case already uses, for the identical
			// reason.
			"SELECT g, (SELECT 999 UNION SELECT min(v)) FROM t1 GROUP BY g ORDER BY g",
		}},
		{"group-by-owner-no-other-aggregate", []string{
			"CREATE TABLE t1(g, v)",
			"INSERT INTO t1 VALUES (1,10),(1,20),(2,30),(2,5)",
			"SELECT g FROM t1 GROUP BY g HAVING (SELECT 999 UNION SELECT max(v)) > 15 ORDER BY g",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differAllowingDeclines(t, c.name, c.stmts)
		})
	}
}

// TestAggCompoundMinMaxRetryOwnerStillServed is the negative control: queries
// that become aggregate via retry should still be served.
func TestAggCompoundMinMaxRetryOwnerStillServed(t *testing.T) {
	differ(t, "no-group-by-owner-compound-max", []string{
		"CREATE TABLE t1(a)",
		"INSERT INTO t1 VALUES (1),(2),(3)",
		"SELECT (SELECT 999 UNION SELECT max(a)) FROM t1",
	})
}
