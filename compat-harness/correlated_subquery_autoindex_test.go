package compat

import (
	"fmt"
	"testing"
)

// C reads a FROM-clause subquery of a correlated subquery through an
// automatic index when an equality on the enclosing query's columns drives
// one: built once, keyed by every column of the subquery, so the rows that
// match arrive ordered by the rest. An order-reading aggregate shows it
// (issue #172).
func TestCorrelatedSubqueryAutoIndexShapes(t *testing.T) {
	pre := []string{
		"CREATE TABLE src(k INT, j INT, v REAL)", "CREATE TABLE t(k INT, w TEXT)",
		"INSERT INTO src VALUES(1,9,1e16),(1,3,1.0),(2,1,0.5),(1,1,-1e16),(1,5,1.0),(2,0,0.25),(3,7,0.1),(3,2,0.2)",
		"INSERT INTO t VALUES(1,'a'),(2,'b'),(1,'c'),(4,'d'),(3,'e')",
		"CREATE TABLE n(s TEXT COLLATE NOCASE, j INT)", "INSERT INTO n VALUES('A',3),('a',1),('B',2),('b',0)",
	}
	for i, q := range []string{
		"SELECT (SELECT group_concat(j) FROM (SELECT k, j, v FROM src LIMIT 100) d WHERE d.k = t.k) FROM t",
		"SELECT (SELECT sum(v) FROM (SELECT k, j, v FROM src LIMIT 100) d WHERE d.k = t.k) FROM t",
		"SELECT w, (SELECT group_concat(j || ':' || v) FROM (SELECT v, j, k FROM src ORDER BY j DESC LIMIT 100) d WHERE d.k = t.k) FROM t",
		"WITH d AS (SELECT k, j, sum(v) v FROM src GROUP BY j, k) SELECT (SELECT group_concat(j) FROM d WHERE d.k = t.k) FROM t",
		"WITH d AS (SELECT k, j, sum(v) v FROM src GROUP BY j, k) " +
			"SELECT (SELECT group_concat(j) FROM d WHERE d.k = t.k), (SELECT group_concat(j) FROM d d2 WHERE d2.k = t.k) FROM t",
		"WITH d AS (SELECT k, j, v FROM src) SELECT t.k, (SELECT group_concat(j) FROM d WHERE d.k = t.k) FROM t, d d0 WHERE d0.k = t.k AND d0.j > 2",
		"WITH d AS MATERIALIZED (SELECT k, j, v FROM src) SELECT (SELECT group_concat(j) FROM d WHERE d.k = t.k) FROM t",
		"SELECT (SELECT group_concat(j) FROM (SELECT k, j FROM src UNION ALL SELECT k + 1, j * 10 FROM src) d WHERE d.k = t.k) FROM t",
		"SELECT (SELECT group_concat(j) FROM (SELECT DISTINCT k, j FROM src) d WHERE d.k = t.k AND d.j > 1) FROM t",
		"SELECT * FROM t WHERE (SELECT group_concat(j) FROM (SELECT k, j FROM src LIMIT 100) d WHERE d.k = t.k) LIKE '%3%'",
		"SELECT (SELECT group_concat(j) FROM (SELECT s, j FROM n LIMIT 10) d WHERE d.s = t.w) FROM t",
		"SELECT (SELECT group_concat(j) FROM (SELECT k, j FROM src LIMIT 100) d WHERE d.k = t.k + 0) FROM t",
		"SELECT (SELECT group_concat(j) FROM (SELECT k, j FROM src LIMIT 100) d WHERE d.k = 1) FROM t",
		"SELECT (SELECT group_concat(j) FROM (SELECT k, j FROM src LIMIT 100) d WHERE d.k >= t.k) FROM t",
		"SELECT (SELECT max(j) FROM (SELECT k, j FROM src LIMIT 100) d WHERE d.k = t.k) FROM t ORDER BY 1",
		"SELECT t.k, count(*) FROM t WHERE EXISTS (SELECT 1 FROM (SELECT k FROM src LIMIT 100) d WHERE d.k = t.k) GROUP BY t.k",
		"SELECT (SELECT j FROM (SELECT k, j FROM src LIMIT 100) d WHERE d.k = t.k) FROM t",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
