package compat

// Derived-table index-order decisions: isolated single-table verdict must hold
// for the flattened query unless outer predicates might fold into it.

import "testing"

func derivedIdxFixture() []string {
	return []string{
		"CREATE TABLE t1(c PRIMARY KEY, a TEXT(10000), b TEXT(10000))",
		"INSERT INTO t1(c) VALUES(5)",
		"INSERT INTO t1(c) VALUES(1)",
		"INSERT INTO t1(c) VALUES(9)",
		"INSERT INTO t1(c) VALUES(3)",
	}
}

// Shapes safe for isolated analysis: nothing outside can fold new predicates.
func TestDerivedIdxOrderSafe(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"left-join-groupby", "SELECT * FROM (SELECT 111) LEFT JOIN (SELECT c+222 FROM t1) GROUP BY 1"},
		{"left-join-no-groupby", "SELECT * FROM (SELECT 111) LEFT JOIN (SELECT c+222 FROM t1)"},
		{"derived-left-of-join", "SELECT * FROM (SELECT c+222 FROM t1) LEFT JOIN (SELECT 111)"},
	}
	for _, c := range cases {
		stmts := append(append([]string(nil), derivedIdxFixture()...), c.sql)
		differ(t, c.name, stmts)
	}
}

// Shapes where outer ON/WHERE references derived columns; flattening substitutes them.
func TestDerivedIdxOrderOuterUnsafe(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"on-clause-references-derived", "SELECT * FROM (SELECT 111 AS x UNION ALL SELECT 222) LEFT JOIN (SELECT c+333 AS y FROM t1) ON x=y GROUP BY 1"},
		{"where-references-derived", "SELECT * FROM (SELECT 111 AS x) LEFT JOIN (SELECT c+222 AS y FROM t1) WHERE y > 200 GROUP BY 1"},
		{"where-references-derived-subset", "SELECT * FROM (SELECT 111 AS x) LEFT JOIN (SELECT c+222 AS y FROM t1) WHERE y > 224 GROUP BY 1"},
		{"on-inequality-several-rows-per-group", "SELECT * FROM (SELECT 111 AS x UNION ALL SELECT 222) LEFT JOIN (SELECT c+106 AS y FROM t1) ON x<y GROUP BY 1"},
	}
	for _, c := range cases {
		differ(t, c.name, append(append([]string(nil), derivedIdxFixture()...), c.sql))
	}
}
