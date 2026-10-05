package compat

import "testing"

// TestMinMaxHoistIntoNativeAggregate pins a min()/max() written in a FROM-less
// subquery, whose argument names only the enclosing GROUP BY query's columns:
// it is that query's aggregate (resolve.c:1346-1373), computed per group.
// window1.test 76.5 is the corpus shape.
func TestMinMaxHoistIntoNativeAggregate(t *testing.T) {
	differ(t, "min/max hoisted into a GROUP BY", []string{
		"CREATE TABLE t3(x)",
		"CREATE TABLE t4(y)",
		"INSERT INTO t3 VALUES(100), (200), (400), (400)",
		"INSERT INTO t4 VALUES(100), (300), (400)",
		"SELECT (SELECT max(y)+sum(0) OVER ()) FROM t3 LEFT JOIN t4 ON x=y GROUP BY x",
		"SELECT (SELECT max(y)) FROM t3 LEFT JOIN t4 ON x=y GROUP BY x",
		"SELECT (SELECT max(y)+1) FROM t3 LEFT JOIN t4 ON x=y GROUP BY x",
		"SELECT (SELECT max(x)) FROM t3 GROUP BY x",
		"SELECT x, (SELECT min(y)), count(*) FROM t3, t4 GROUP BY x ORDER BY x",
		"SELECT (SELECT max(y)) FROM t3, t4 WHERE x=y GROUP BY x",
		"SELECT max(y), (SELECT max(y)) FROM t3 LEFT JOIN t4 ON x=y GROUP BY x",
	})
}
