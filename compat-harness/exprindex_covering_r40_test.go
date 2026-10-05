// Expression indexes as covering scans.
package compat

import "testing"

func TestExprIndexCoveringScan(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// Composite index with plain and expression columns as covering scan.
		{"indexexpr1-covering-composite", []string{
			`CREATE TABLE t1(a INT, b TEXT, c INT, d INT)`,
			`INSERT INTO t1(a,b,c,d) VALUES
				(1, '{"x":1}', 12,  3),
				(1, '{"x":2}',  4,  5),
				(1, '{"x":1}',  6, 11),
				(2, '{"x":1}', 22,  3),
				(2, '{"x":2}',  4,  5),
				(3, '{"x":1}',  6,  7)`,
			`CREATE INDEX t1x ON t1(d, a, b->>'x', c)`,
			`SELECT a,
			   SUM(1)                              AS t1,
			   SUM(CASE WHEN b->>'x'=1 THEN 1 END) AS t2,
			   SUM(c)                              AS t3,
			   SUM(CASE WHEN b->>'x'=1 THEN c END) AS t4
			FROM t1`,
		}},
		// Same shape, but with the index's key order and rowid (insertion)
		// order DISAGREEING on which physical row sorts first -- rules out
		// coincidental agreement, since a wrong access-path choice here
		// reports a different "a" than the oracle's.
		{"indexexpr1-covering-order-disagrees", []string{
			`CREATE TABLE t1(a INT, b TEXT, c INT, d INT)`,
			`INSERT INTO t1(a,b,c,d) VALUES
				(99, '{"x":9}', 1,  50),
				(1, '{"x":1}', 12,  3),
				(2, '{"x":2}',  4,  5)`,
			`CREATE INDEX t1x ON t1(d, a, b->>'x', c)`,
			`SELECT a,
			   SUM(1)                              AS t1,
			   SUM(CASE WHEN b->>'x'=1 THEN 1 END) AS t2,
			   SUM(c)                              AS t3,
			   SUM(CASE WHEN b->>'x'=1 THEN c END) AS t4
			FROM t1`,
		}},
		// minmax2.test's own shape: "Extend the min/max optimization to
		// indexes on expressions" -- a single-expression index, no WHERE.
		{"minmax2-bare-column-plus-max", []string{
			`CREATE TABLE t11(a,b,c)`,
			`INSERT INTO t11(a,b,c) VALUES(1,10,5),(2,8,11),(3,1,4),(4,20,1),(5,16,4)`,
			`CREATE INDEX t11bc ON t11(b+c)`,
			`SELECT a, max(b+c) FROM t11`,
		}},
		{"minmax2-bare-column-plus-min", []string{
			`CREATE TABLE t11(a,b,c)`,
			`INSERT INTO t11(a,b,c) VALUES(1,10,5),(2,8,11),(3,1,4),(4,20,1),(5,16,4)`,
			`CREATE INDEX t11bc ON t11(b+c)`,
			`SELECT a, min(b+c) FROM t11`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			differ(t, tc.name, tc.stmts)
		})
	}
}
