package compat

import "testing"

// TestDeferredArityErrors pins WHEN two arity errors are raised, which decides
// whether a CREATE TRIGGER carrying one succeeds: a vector assignment from a
// SELECT is counted only when coded (expr.c:2113), and a multi-row VALUES row
// is counted while parsing only on sqlite3MultiValues' co-routine path
// (insert.c:774-776), otherwise at resolve (resolve.c:2090). altertab.test
// 33.0 and altertab2.test 9.0 create such triggers.
func TestDeferredArityErrors(t *testing.T) {
	differ(t, "deferred arity errors", []string{
		"CREATE TABLE t1(a TEXT)",
		"CREATE TABLE t2(b TEXT, a)",
		"INSERT INTO t2 VALUES(1,2)",
		"UPDATE t2 SET (b,a)=(SELECT 1)",
		"UPDATE t2 SET (b,a)=(SELECT 1,2,3)",
		"UPDATE t2 SET (b,a)=(1)",
		"CREATE TRIGGER r3 AFTER INSERT ON t1 BEGIN UPDATE t2 SET (b,a)=(SELECT 1); END",
		"INSERT INTO t1 VALUES('x')",
		"SELECT count(*) FROM t1",
		"DROP TRIGGER r3",
		"SELECT * FROM (VALUES(1,2),(3))",
		"SELECT * FROM (VALUES(1,2),(3,4),(5))",
		"SELECT * FROM (VALUES(1,2) UNION ALL SELECT 3)",
		"WITH x AS (SELECT 1) SELECT * FROM (VALUES(1,2),(3))",
		"CREATE TRIGGER r4 AFTER INSERT ON t2 BEGIN SELECT * FROM (VALUES(new.a,2),(3)); END",
		"CREATE TRIGGER r5 AFTER INSERT ON t2 BEGIN SELECT * FROM (VALUES(1,2),(3)); END",
		"CREATE TRIGGER r6 AFTER INSERT ON t2 BEGIN SELECT * FROM (VALUES(1,2),(3,new.a),(4)); END",
		"CREATE TRIGGER r7 AFTER INSERT ON t2 BEGIN SELECT * FROM (VALUES(CAST(1 AS INT),2),(3)); END",
		"SELECT name FROM sqlite_schema WHERE type='trigger' ORDER BY name",
		"INSERT INTO t2 VALUES(3,4)",
	})
}
