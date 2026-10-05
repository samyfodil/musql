// This file tests min()/max() aggregates in compound arms of FROM-less
// scalar subqueries in select lists, where the enclosing query becomes
// aggregate only because of the hoist.
package compat

import "testing"

// aggCompoundMinMaxHoistDDL is a simple single-table fixture.
var aggCompoundMinMaxHoistDDL = []string{
	"CREATE TABLE t1(a)",
	"INSERT INTO t1 VALUES (1),(2),(3)",
}

// TestAggCompoundMinMaxHoistOrdinaryTable tests min/max alongside control
// aggregates (sum/count/avg).
func TestAggCompoundMinMaxHoistOrdinaryTable(t *testing.T) {
	for _, agg := range []string{"sum", "count", "avg", "min", "max"} {
		t.Run(agg, func(t *testing.T) {
			differ(t, agg, append(append([]string{}, aggCompoundMinMaxHoistDDL...),
				"SELECT (SELECT 999 UNION SELECT "+agg+"(a)) FROM t1"))
		})
	}
}

// TestAggCompoundMinMaxHoistVariants tests DISTINCT, ORDER BY, multi-arm,
// and EXISTS variants.
func TestAggCompoundMinMaxHoistVariants(t *testing.T) {
	cases := []struct {
		name string
		q    string
	}{
		{"distinct-order-by", "SELECT (SELECT 999 UNION SELECT DISTINCT max(a) ORDER BY 1) FROM t1"},
		{"three-arm-compound", "SELECT (SELECT 999 UNION SELECT max(a) UNION SELECT -1) FROM t1"},
		{"exists", "SELECT EXISTS(SELECT 999 UNION SELECT max(a)) FROM t1"},
		{"min-instead-of-max", "SELECT (SELECT 999 UNION SELECT min(a)) FROM t1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, append(append([]string{}, aggCompoundMinMaxHoistDDL...), c.q))
		})
	}
}

// TestAggCompoundMinMaxHoistNeverAppliesLocalArgument verifies that
// aggregates with local arguments do not hoist.
func TestAggCompoundMinMaxHoistNeverAppliesLocalArgument(t *testing.T) {
	differ(t, "local-arm-from-not-hoisted", append(append([]string{}, aggCompoundMinMaxHoistDDL...),
		"CREATE TABLE t2(a)",
		"INSERT INTO t2 VALUES (100),(200)",
		"SELECT (SELECT 999 UNION SELECT max(a) FROM t2) FROM t1",
	))
}
