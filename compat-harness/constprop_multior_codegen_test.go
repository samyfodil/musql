package compat

import "testing"

// TestConstPropMultiOrDisjunctCompiles: a WHERE_MULTI_OR disjunct is compiled
// from the planner's copy of the WHERE, where propagateConstants has fixed a
// column (whereFixedCol). The compiler refused that node, and gencol1.test's
// "x.b='2' AND (y.a=2 OR (x.b LIKE '2*' AND y.a=x.b))" declined; it compiles as
// the column it stands for.
func TestConstPropMultiOrDisjunctCompiles(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t0 AS x JOIN t0 AS y WHERE x.b='2' AND (y.a=2 OR (x.b LIKE '2*' AND y.a=x.b))",
		"SELECT * FROM t0 AS x JOIN t0 AS y WHERE x.b=2 AND (y.a=2 OR (x.b > 1 AND y.a=x.b))",
		"SELECT * FROM t0 AS x JOIN t0 AS y WHERE x.a=2 AND (y.a=x.a OR y.b=x.a)",
	} {
		differ(t, q, []string{"CREATE TABLE t0(a INTEGER PRIMARY KEY, b TEXT AS (a) UNIQUE)", "INSERT INTO t0(a) VALUES(1),(2),(3)",
			"CREATE INDEX t0b ON t0(b)", q})
	}
}
