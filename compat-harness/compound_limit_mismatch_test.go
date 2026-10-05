// Tests LIMIT/OFFSET timing in compound subqueries: non-integer limits must be
// errors only when the subquery is actually executed, not at compile time.
package compat

import "testing"

// TestCompoundSubqueryLimitMismatchTiming verifies the timing rule for LIMIT
// errors in stepped vs. never-stepped subqueries.
func TestCompoundSubqueryLimitMismatchTiming(t *testing.T) {
	setup := []string{
		`CREATE TABLE abc(a,b,c)`,
		`CREATE TABLE full1(x)`,
		`INSERT INTO full1 VALUES(1)`,
	}
	q := func(sql ...string) []string { return append(append([]string{}, setup...), sql...) }

	// NEVER STEPPED: the enclosing scan is over an empty table, so 3.53.3
	// answers zero rows and this engine must too. Asserted as a non-error
	// separately from differ(), which cannot tell two agreeing errors from two
	// agreeing answers.
	answering := []string{
		`SELECT EXISTS(SELECT 1 FROM (SELECT 1 AS x UNION ALL SELECT 2 LIMIT 56.1)) FROM abc`,
		`SELECT EXISTS(SELECT 1 FROM (SELECT 1 AS x UNION ALL SELECT 2 LIMIT 1 OFFSET 56.1)) FROM abc`,
		`SELECT (SELECT x FROM (SELECT 1 AS x UNION ALL SELECT 2 ORDER BY 1 LIMIT 'zz')) FROM abc`,
		`SELECT EXISTS(SELECT 1 FROM (SELECT 1 AS x UNION ALL SELECT 2 LIMIT x'41')) FROM abc`,
		`SELECT EXISTS(SELECT 1 FROM (SELECT 1 AS x UNION ALL SELECT 2 LIMIT NULL)) FROM abc`,
		// misc1-26.0's own two derived compounds, in isolation.
		`SELECT EXISTS(SELECT 1 FROM (SELECT DISTINCT 2147483648, 'hardware' UNION ALL SELECT -2147483648, 'experiments' ORDER BY 2147483648 LIMIT 1 OFFSET 123456789.1234567899)) FROM abc`,
		`SELECT EXISTS(SELECT 1 FROM (SELECT 1,2,3 UNION SELECT 'The','The',2147483649 ORDER BY 1 LIMIT 123456789.1234567899 OFFSET -2147483647)) FROM abc`,
	}
	for _, sql := range answering {
		differ(t, sql, q(sql))
		res := run(t, "musql", q(sql))
		if last := res[len(res)-1]; last["kind"] == "error" {
			t.Errorf("never-stepped compound LIMIT still declines: %s -> %v", sql, last)
		}
	}

	// STEPPED: the same bodies, reached. Both engines must raise "datatype
	// mismatch"; the worker reports only that a statement errored, so the
	// message itself is not asserted here -- what is asserted is that the
	// answer is an ERROR and not a row, which is the direction a mistimed
	// deferral would break.
	for _, sql := range []string{
		`SELECT EXISTS(SELECT 1 FROM (SELECT 1 AS x UNION ALL SELECT 2 LIMIT 56.1)) FROM full1`,
		`SELECT EXISTS(SELECT 1 FROM (SELECT 1 AS x UNION ALL SELECT 2 LIMIT 1 OFFSET 56.1)) FROM full1`,
		`SELECT * FROM (SELECT 1 AS x UNION ALL SELECT 2 LIMIT 56.1)`,
	} {
		differ(t, sql, q(sql))
		res := run(t, "musql", q(sql))
		if last := res[len(res)-1]; last["kind"] != "error" {
			t.Errorf("stepped compound LIMIT must raise datatype mismatch: %s -> %v", sql, last)
		}
	}
}

// TestCompoundSubqueryLimitStillFolds guards the other direction: a compound
// subquery whose clause DOES fold to an integer must keep folding, with the
// bound applied. These all answered before this slice and must keep answering
// with the same rows.
func TestCompoundSubqueryLimitStillFolds(t *testing.T) {
	differ(t, "compound subquery LIMIT that folds", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`SELECT * FROM (SELECT a FROM t UNION ALL SELECT a+10 FROM t ORDER BY 1 LIMIT 1+1)`,
		`SELECT * FROM (SELECT a FROM t UNION ALL SELECT a+10 FROM t ORDER BY 1 LIMIT 2 OFFSET 1+1)`,
		`SELECT * FROM (SELECT a FROM t UNION ALL SELECT a+10 FROM t ORDER BY 1 LIMIT (SELECT 2))`,
		`SELECT * FROM (SELECT a FROM t UNION ALL SELECT a+10 FROM t ORDER BY 1 LIMIT '2')`,
		`SELECT * FROM (SELECT a FROM t UNION ALL SELECT a+10 FROM t ORDER BY 1 LIMIT 2.0)`,
		`SELECT * FROM (SELECT a FROM t UNION ALL SELECT a+10 FROM t ORDER BY 1 LIMIT 0)`,
	})
}

// misc1.test's misc1-26.0 is CLOSED. It used to be pinned here as still
// declining, with the two remaining layers spelled out; both are gone.
//
// Layer 1 was its outer derived compound's "ORDER BY 'hardware'", which C
// resolves against the THIRD arm's own select-list expression
// (resolveCompoundOrderBy walks every arm until each term is done,
// resolve.c:1633-1697) and this engine gave up on at the FIRST arm. Layer 2 was
// why: resolveArmOutputs resolved that arm's FROM by EXECUTING it
// (resolveFrom -> resolveDerivedRows -> execSelect), so the inner derived
// compound's own bad OFFSET was evaluated during NAME RESOLUTION -- where C
// runs nothing at all (sqlite3ExpandSubquery, select.c:5885, reads the leftmost
// arm's expression list). resolveArmOutputsOuter now retries without the
// execution; see compound_arm_schema_only_test.go, which replays misc1-26.0
// verbatim through differ() and asserts it ANSWERS.
