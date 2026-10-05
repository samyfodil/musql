package compat

import "testing"

// TestR44DerivedExprOnlySelfRefAnchorOrder tests self-referential ON clauses in derived tables
// when all columns are computed expressions.
func TestR44DerivedExprOnlySelfRefAnchorOrder(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(c PRIMARY KEY, a TEXT(10000), b TEXT(10000))`,
	}
	cases := []struct {
		name  string
		stmts []string
	}{
		{
			"join-14.5-empty-t1",
			append(append([]string{}, setup...),
				`SELECT * FROM (SELECT 111 AS x UNION ALL SELECT 222)
				   LEFT JOIN (SELECT c+333 AS y FROM t1) ON x=y
				  GROUP BY 1`),
		},
		{
			"join-14.6-duplicate-left-row",
			append(append([]string{}, setup...),
				`SELECT * FROM (SELECT 111 AS x UNION ALL SELECT 111)
				   LEFT JOIN (SELECT c+333 AS y FROM t1) ON x=y
				  GROUP BY 1`),
		},
		{
			"join-14.7-three-arms",
			append(append([]string{}, setup...),
				`SELECT * FROM (SELECT 111 AS x UNION ALL SELECT 111 UNION ALL SELECT 222)
				   LEFT JOIN (SELECT c+333 AS y FROM t1) ON x=y
				  GROUP BY 1`),
		},
		{
			"join-14.8-real-match",
			append(append(append([]string{}, setup...),
				`INSERT INTO t1(c) VALUES(-111)`),
				`SELECT * FROM (SELECT 111 AS x UNION ALL SELECT 111 UNION ALL SELECT 222)
				   LEFT JOIN (SELECT c+333 AS y FROM t1) ON x=y
				  GROUP BY 1`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
