package compat

import "testing"

// TestReturningAggregateSubqueryReadsRow pins a RETURNING subquery that is an
// aggregate query of its own and also names the affected row outside its
// aggregate (returning1.test 20.3): the row is resolve.c's pseudo-row
// (resolve.c:528-532), read after every FROM missed, and the aggregate over
// the table sees each deletion as it happens.
func TestReturningAggregateSubqueryReadsRow(t *testing.T) {
	differ(t, "RETURNING aggregate subquery", []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT)",
		"INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40),(6,60),(8,80)",
		"BEGIN",
		"DELETE FROM t1 RETURNING a, (SELECT min(t2.a)+t1.a*100 FROM t1 AS t2), (SELECT max(t2.a)+t1.a*100 FROM t1 AS t2), (SELECT round(avg(t2.a),2)+t1.a*100 FROM t1 AS t2)",
		"ROLLBACK",
		"UPDATE t1 SET b=b+1 WHERE a<4 RETURNING a, (SELECT count(*)+b FROM t1 AS t2 WHERE t2.a<t1.a)",
		"INSERT INTO t1 VALUES(9,90) RETURNING (SELECT sum(t2.b)-b FROM t1 AS t2)",
	})
}
