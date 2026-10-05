package compat

import "testing"

// TestR39CompoundDerivedAnchorOrder tests anchor-order checks for nested
// GROUP BY in views and for FROM-less compound derived tables.
func TestR39CompoundDerivedAnchorOrder(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{
			"view-nested-groupby-indexed-table",
			[]string{
				`CREATE TABLE t4(a TEXT, b INT)`,
				`INSERT INTO t4(a,b) VALUES('abc',0),('ABC',1),('def',2)`,
				`CREATE INDEX t4x ON t4(a, +a COLLATE NOCASE)`,
				`CREATE VIEW t5 AS
				   SELECT 1 AS b
				    WHERE (SELECT count(0=NOT+a COLLATE NOCASE IN (SELECT 0))
				             FROM t4
				            GROUP BY a)`,
				`SELECT * FROM t5`,
			},
		},
		{
			"compound-derived-noindex-vs-real-table-index",
			[]string{
				`CREATE TABLE t2(c PRIMARY KEY, v)`,
				`INSERT INTO t2 VALUES(1,10),(2,20)`,
				`SELECT * FROM (SELECT 111 AS x UNION ALL SELECT 222)
				   LEFT JOIN (SELECT v AS y FROM t2) ON 1=1
				  GROUP BY 1`,
			},
		},
		{
			"compound-derived-three-arms",
			[]string{
				`CREATE TABLE t3(c PRIMARY KEY, v)`,
				`INSERT INTO t3 VALUES(1,7),(2,8),(3,9)`,
				`SELECT * FROM (SELECT 1 AS x UNION SELECT 2 UNION SELECT 3)
				   LEFT JOIN (SELECT v AS y FROM t3) ON 1=1
				  GROUP BY 1`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
