package compat

import "testing"

// TestVtabInsertReturningCorrelatedSubquery verifies that RETURNING output
// columns in virtual-table writes can reference the row being written through
// correlated subqueries.
func TestVtabInsertReturningCorrelatedSubquery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"returning1.test 13.1 verbatim", []string{
			`CREATE VIRTUAL TABLE t1 USING rtree(a, b, c)`,
			`CREATE TABLE t2(x)`,
			`INSERT INTO t1(a,b,c) VALUES(1,2,3) RETURNING (SELECT b FROM t2)`,
			`SELECT * FROM t1`,
		}},
		{"the correlated operand decides the value", []string{
			`CREATE VIRTUAL TABLE t1 USING rtree(a, b, c)`,
			`CREATE TABLE t2(x)`,
			`INSERT INTO t2 VALUES(10),(20)`,
			`INSERT INTO t1(a,b,c) VALUES(4,5,6) RETURNING (SELECT b FROM t2)`,
			`INSERT INTO t1(a,b,c) VALUES(7,8,9) RETURNING (SELECT b FROM t2 WHERE x=20)`,
			`INSERT INTO t1(a,b,c) VALUES(10,11,12) RETURNING (SELECT sum(b+x) FROM t2)`,
			`SELECT * FROM t1 ORDER BY a`,
		}},
		{"EXISTS, IN and a count over the affected row", []string{
			`CREATE VIRTUAL TABLE t1 USING rtree(a, b, c)`,
			`CREATE TABLE t2(x)`,
			`INSERT INTO t2 VALUES(2),(20)`,
			`INSERT INTO t1(a,b,c) VALUES(1,2,3) RETURNING EXISTS(SELECT 1 FROM t2 WHERE x=b)`,
			`INSERT INTO t1(a,b,c) VALUES(4,5,6) RETURNING b IN (SELECT x FROM t2)`,
			`INSERT INTO t1(a,b,c) VALUES(7,8,9) RETURNING (SELECT count(*) FROM t2 WHERE x<b)`,
		}},
		{"one INSERT writing several rows, each with its own correlated value", []string{
			`CREATE VIRTUAL TABLE t1 USING rtree(a, b, c)`,
			`CREATE TABLE t2(x)`,
			`INSERT INTO t2 VALUES(100)`,
			`INSERT INTO t1(a,b,c) VALUES(1,2,3),(4,5,6),(7,8,9) RETURNING a, (SELECT x+b FROM t2)`,
			`SELECT * FROM t1 ORDER BY a`,
		}},
		{"an UNCORRELATED subquery in the same position keeps working", []string{
			`CREATE VIRTUAL TABLE t1 USING rtree(a, b, c)`,
			`CREATE TABLE t2(x)`,
			`INSERT INTO t2 VALUES(42)`,
			`INSERT INTO t1(a,b,c) VALUES(1,2,3) RETURNING (SELECT x FROM t2)`,
		}},
		{"the affected row named by the table, and a plain column beside it", []string{
			`CREATE VIRTUAL TABLE t1 USING rtree(a, b, c)`,
			`CREATE TABLE t2(x)`,
			`INSERT INTO t2 VALUES(7)`,
			`INSERT INTO t1(a,b,c) VALUES(1,2,3) RETURNING b, (SELECT x FROM t2 WHERE x=t1.b+5)`,
		}},
	} {
		differ(t, "vtab-returning-correlated/"+tc.name, tc.stmts)
	}
}
