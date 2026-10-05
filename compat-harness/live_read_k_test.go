// Tests subqueries inside trigger WHEN guards and BODY statements. These
// subqueries must be evaluated once per firing, reflecting any changes made
// earlier in the same firing.
package compat

import "testing"

func TestLiveReadTriggerBodyReadsTheFiringInstant(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// --- the two the frozen image gets wrong -------------------------------
		{"body VALUES subquery sees an earlier body statement's write", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES((SELECT count(*) FROM s)); END`,
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body VALUES subquery sees an earlier row of the firing statement", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t)); END`,
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log ORDER BY rowid`}},

		// --- the restriction: what must still decline --------------------------
		{"body VALUES subquery naming NEW inside the subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1),(2)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM s WHERE x=new.a)); END`,
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log ORDER BY rowid`}},
		{"WHEN guard subquery naming NEW", []string{
			`CREATE TABLE king(a,b,PRIMARY KEY(a))`, `CREATE TABLE prince(c,d)`,
			`CREATE TRIGGER kt AFTER INSERT ON prince WHEN NOT EXISTS (SELECT a FROM king WHERE a=new.c) BEGIN INSERT INTO king VALUES(new.c,NULL); END`,
			`INSERT INTO prince VALUES(1,2)`, `INSERT INTO prince VALUES(1,9)`, `INSERT INTO prince VALUES(2,3)`,
			`SELECT a FROM king ORDER BY a`}},
		{"body VALUES subquery over an unknown table", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM nosuch)); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"WHEN guard subquery over an unknown table", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM nosuch)>0 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		// A statement-level WITH inside a trigger body is a PARSE error on both
		// sides ("expected SELECT, got INSERT" here; the oracle rejects it at
		// CREATE TRIGGER and then fails the INSERT). Pinned because
		// compileInsertStmt's live branch is gated on there being no such
		// clause, and this is the evidence for why that gate never has to serve
		// a CTE scope at fire time -- select.c:6000's sqlite3WithPush ahead of
		// select.c:6028/6036's tie-break-free CTE-shadows-table lookup is what
		// re-lowering without the scope would get wrong.
		{"statement-level WITH on a body INSERT", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(4)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN WITH c AS (SELECT x FROM s) INSERT INTO log SELECT x FROM c; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"body subquery over a TEMP table", []string{
			`CREATE TABLE t(a)`, `CREATE TEMP TABLE s(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES((SELECT count(*) FROM s)); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},

		// --- the WHEN guard, per firing row ------------------------------------
		{"WHEN guard counts the firing table", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM t)>1 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`, `SELECT x FROM log ORDER BY rowid`}},
		{"WHEN guard sees a write the body made on an earlier row", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM s)<2 BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2),(3),(4)`, `SELECT n FROM log ORDER BY rowid`, `SELECT x FROM s ORDER BY rowid`}},
		{"WHEN guard false for every row", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM s)>0 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},
		{"WHEN guard on max() of the firing table", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT max(a) FROM t)=new.a BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(3),(1),(2)`, `SELECT x FROM log ORDER BY rowid`}},

		// --- the body, across every read opcode and every firing event ---------
		{"body INSERT ... SELECT counts the firing table", []string{
			`CREATE TABLE t1(a)`, `CREATE TABLE t2(n)`,
			`CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN INSERT INTO t2 SELECT count(*) FROM t1; END`,
			`INSERT INTO t1 VALUES(1)`, `INSERT INTO t1 VALUES(2)`, `SELECT n FROM t2 ORDER BY rowid`}},
		{"body INSERT ... SELECT sees an earlier body statement's write", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(y)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log SELECT x FROM s; END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT y FROM log ORDER BY rowid`}},
		{"body EXISTS subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES(EXISTS(SELECT 1 FROM s)); INSERT INTO s VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body row-value IN subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x,y)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES(((1,2) IN (SELECT x,y FROM s))); INSERT INTO s VALUES(1,2); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body DELETE with an IN subquery in WHERE", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`,
			`INSERT INTO s VALUES(1),(2),(3)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN DELETE FROM s WHERE x IN (SELECT a FROM t); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT x FROM s ORDER BY rowid`}},
		{"body UPDATE with a SET subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x,n)`, `INSERT INTO s VALUES(1,0),(2,0)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN UPDATE s SET n=(SELECT count(*) FROM t); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT x,n FROM s ORDER BY rowid`}},
		{"AFTER UPDATE body subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO t VALUES(1),(2)`,
			`CREATE TRIGGER tb AFTER UPDATE ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES((SELECT count(*) FROM s)); END`,
			`UPDATE t SET a=a+10`, `SELECT n FROM log ORDER BY rowid`}},
		{"AFTER DELETE body subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`INSERT INTO t VALUES(1),(2),(3)`,
			`CREATE TRIGGER tb AFTER DELETE ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t)); END`,
			`DELETE FROM t`, `SELECT n FROM log ORDER BY rowid`}},
		{"BEFORE trigger body subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t)); END`,
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log ORDER BY rowid`}},
		{"INSTEAD OF trigger on a view, body subquery", []string{
			`CREATE TABLE t(a)`, `CREATE VIEW v AS SELECT a FROM t`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tv INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a); INSERT INTO log VALUES((SELECT count(*) FROM t)); END`,
			`INSERT INTO v VALUES(1)`, `INSERT INTO v VALUES(2)`, `SELECT n FROM log ORDER BY rowid`}},
		{"nested trigger: the inner body's subquery sees the outer body's write", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tu AFTER INSERT ON u BEGIN INSERT INTO log VALUES((SELECT count(*) FROM u)); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body subquery over the table the body is writing", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM log)); END`,
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body subquery over a VIEW", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE VIEW v AS SELECT count(*) AS c FROM s`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES((SELECT c FROM v)); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body INSERT with a SELECT-level WITH", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(7)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log WITH c AS (SELECT x FROM s) SELECT x FROM c; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"body subquery whose probed column declares a collation", []string{
			`CREATE TABLE t(a TEXT)`, `CREATE TABLE s(x TEXT COLLATE NOCASE)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES('AbC')`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM s WHERE x='abc')); END`,
			`INSERT INTO t VALUES('q')`, `SELECT n FROM log`}},

		// --- OP_Once: once per FIRING, not once per row of the body's own scan --
		//
		// The body's outer SELECT visits three rows of s; its inner scalar
		// subquery is uncorrelated, so the oracle evaluates it ONCE and reuses
		// the value for all three (expr.c:3889's OP_Once). The engine's
		// runSubOnce cache is what reproduces that, and the live re-lowering
		// deliberately sits BELOW that cache so it is paid once per firing too.
		{"body subquery runs once per firing, not once per body row", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1),(2),(3)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT (SELECT count(*) FROM s) FROM s; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log ORDER BY rowid`}},
		// Two tuples of one multi-row body VALUES: each holds its own copy of
		// the subquery, and both answer from the same firing instant.
		{"body multi-row VALUES, one subquery per tuple", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM s)),((SELECT count(*) FROM s)); INSERT INTO s VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},

		// --- the surrounding machinery the promotion must not disturb ----------
		//
		// The firing statement's program is CACHED by text for as long as the
		// schema and transaction generation hold (cachedWriteProgram), so three
		// runs of one INSERT reuse one program -- and each must still re-lower
		// its body's read. 1,2,3 rather than 1,1,1 is the assertion.
		{"the same firing statement run three times reuses one cached program", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES((SELECT count(*) FROM s)); END`,
			`INSERT INTO t VALUES(1)`, `INSERT INTO t VALUES(1)`, `INSERT INTO t VALUES(1)`,
			`SELECT n FROM log ORDER BY rowid`}},
		{"a rolled-back transaction leaves the next firing reading the right state", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES((SELECT count(*) FROM s)); END`,
			`BEGIN`, `INSERT INTO t VALUES(1),(2)`, `ROLLBACK`,
			`INSERT INTO t VALUES(9)`, `SELECT n FROM log ORDER BY rowid`}},
	}
	for _, tc := range cases {
		differ(t, tc.name, tc.stmts)
	}
}

// TestLiveReadTriggerBodyBreadth is the breadth sweep the promotion was audited
// with, kept as a gate. Each case is a body or guard read of a KIND the live
// re-lowering has to reproduce -- a compound, a window function, a recursive
// CTE, a schema catalog, a virtual table, a WITHOUT ROWID b-tree, a generated
// column, an ORDER BY with an explicit collation, a connection-state function.
//
// It exists because reasoning about which of these are "self-contained" is
// exactly what this batch got wrong once: the virtual-table cases below were
// swept into the promotion by an argument that sounded complete and was not
// (see programNeedsRowContext, engine/vdbe_live_read.go). They are DECLINED
// again now, and they stay here because a decline that answers is a pinned
// answer -- differ() compares the whole transcript either way, so this file
// does not need to know, or care, which route each case took.
func TestLiveReadTriggerBodyBreadth(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"body read of a UNION ALL compound", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE u(y)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1)`, `INSERT INTO u VALUES(2)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT x FROM s UNION ALL SELECT y FROM u; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body read through a window function", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1),(2),(3)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT sum(x) OVER (ORDER BY x) FROM s; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body read through a recursive CTE", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<3) SELECT i FROM c; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body read of the schema catalog", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM sqlite_master)); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"body read of sqlite_sequence", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE q(i INTEGER PRIMARY KEY AUTOINCREMENT, v)`, `CREATE TABLE log(n)`,
			`INSERT INTO q(v) VALUES('a')`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM sqlite_sequence)); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"body read of a WITHOUT ROWID table", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, `CREATE TABLE log(n)`,
			`INSERT INTO w VALUES('a',1),('b',2)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM w)); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"body read of a generated column", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE g(x, y AS (x*2))`, `CREATE TABLE log(n)`,
			`INSERT INTO g(x) VALUES(3)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT y FROM g)); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"body read with ORDER BY ... COLLATE", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x TEXT)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES('B'),('a')`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT x FROM s ORDER BY x COLLATE NOCASE; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body read with LIMIT and OFFSET", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1),(2),(3)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT x FROM s LIMIT 2 OFFSET 1; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log ORDER BY rowid`}},

		// The connection-state functions: their answer comes from the live
		// SESSION, and a live sub-program runs on a fresh VM whose only link to
		// it is the snapshot pager's own connection state. Pinned because that
		// link is easy to break and impossible to notice from a compile.
		{"changes() inside a body subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1),(2)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT changes() FROM s LIMIT 1)); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},
		{"total_changes() inside a body subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1),(2)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT total_changes() FROM s LIMIT 1)); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},
		{"last_insert_rowid() inside a body subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1),(2)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT last_insert_rowid() FROM s LIMIT 1)); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},
		{"changes() inside a WHEN guard's subquery", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
			`INSERT INTO s VALUES(1)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT changes() FROM s)>=0 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT n FROM log ORDER BY rowid`}},

		// The virtual-table family, all DECLINED (programNeedsRowContext) and
		// all still answering the oracle's answer.
		{"body read of an rtree virtual table", []string{
			`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE log(n)`,
			`INSERT INTO r VALUES(1,1.0,2.0)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM r)); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"body read of an fts4 table through MATCH", []string{
			`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(c)`, `CREATE TABLE log(n)`,
			`INSERT INTO f VALUES('hello world')`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM f WHERE f MATCH 'hello')); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
		{"body read of a table-valued function with a constant argument", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT value FROM json_each('[1,2,3]'); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log ORDER BY rowid`}},
		{"body read of a VIEW whose own body reads a table-valued function", []string{
			`CREATE TABLE t(a)`, `CREATE VIEW v AS SELECT value AS q FROM json_each('[7,8]')`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT q FROM v; END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log ORDER BY rowid`}},
		{"WHEN guard read of a virtual table", []string{
			`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE log(n)`,
			`INSERT INTO r VALUES(1,1.0,2.0)`,
			`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM r)>0 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(1)`, `SELECT n FROM log`}},
	}
	for _, tc := range cases {
		differ(t, tc.name, tc.stmts)
	}
}
