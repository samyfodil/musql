package compat

import "testing"

// TestAggDrainReadsEnclosingRow pins an aggregate argument that names the
// ENCLOSING query's column inside a sorted GROUP BY: the drain codes it as the
// plain correlated read, since the enclosing query stays on its row while the
// subquery runs (a TK_COLUMN of an outer cursor is no AggInfo column,
// expr.c:7454-7469). indexexpr2.test 9.0 is the corpus shape.
func TestAggDrainReadsEnclosingRow(t *testing.T) {
	differ(t, "sorted GROUP BY drain over an outer column", []string{
		"CREATE TABLE t1(a INT, b INT)",
		"CREATE INDEX t1x ON t1(a, abs(b))",
		"CREATE TABLE t2(c INT, d INT)",
		"INSERT INTO t1(a,b) VALUES(4,4),(5,-5),(5,20),(6,6)",
		"INSERT INTO t2(c,d) VALUES(100,1),(200,1),(300,2)",
		"SELECT *, (SELECT max(c+abs(b)) FROM t2 GROUP BY d ORDER BY d LIMIT 1) AS subq FROM t1 WHERE a=5",
		"SELECT *, (SELECT max(c+abs(b)) FROM t2 GROUP BY d ORDER BY d LIMIT 1) AS subq FROM t1 ORDER BY a, b",
		"SELECT a, (SELECT group_concat(c*b, ',') FROM t2 GROUP BY d ORDER BY d DESC LIMIT 1) FROM t1 ORDER BY a, b",
		"SELECT a, (SELECT sum(c) FILTER (WHERE c>b*20) FROM t2 GROUP BY d ORDER BY d) FROM t1 ORDER BY a, b",
		"SELECT a, (SELECT count(DISTINCT c+b) FROM t2 GROUP BY d ORDER BY 1 LIMIT 1) FROM t1 ORDER BY a, b",
		"SELECT d, (SELECT max(c+abs(b)) FROM t1 WHERE a=5 GROUP BY a ORDER BY a) FROM t2 GROUP BY d ORDER BY d",
	})
}
