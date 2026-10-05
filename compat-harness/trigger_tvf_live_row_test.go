package compat

import "testing"

// Tests table-valued functions in trigger bodies with NEW/OLD row references.
// emitted at all. derivedSource.vtabTrig (vdbe_op.go) carries it from the
// compile that emitted the OpOpenDerived to the run-time fold, so the argument
// becomes an OpParam read of the firing row.
//
// That is C's own shape: sqlite3WhereTabFuncArgs (whereexpr.c:1902) rewrites a
// table-valued function's arguments into ordinary "hidden-column = <arg>" WHERE
// terms, whose right-hand sides are coded by sqlite3ExprCodeTarget
// (expr.c:4950) into the registers xFilter reads -- inside the trigger
// sub-program, where lookupName's pParse->pTriggerTab arm (resolve.c:525-543)
// binds NEW./OLD.
//
// # What each case is here to catch
//
// The engine-side gate (engine/live_read_codegen_test.go's liveReadShapes)
// proves these COMPILE. Only a differential can prove they compile to the right
// answer, and the cases below are chosen for the ways a wrong one would look:
//
//   - a MULTI-ROW firing, where a cached fold would repeat the first row's
//     argument for every later one;
//   - OLD. on an UPDATE and on a DELETE trigger, so the other pseudo-row is
//     covered and not just NEW;
//   - a NESTED trigger, where the inner body must read the INNER firing row --
//     the machine the fold is threaded from has to be the one running the inner
//     source, not the outer;
//   - a NEW. operand with a DECLARED COLLATE NOCASE, which is the property a
//     literal fold through compiler.rowOuter would have lost (it is why the
//     argument is compiled to an OpParam read rather than folded);
//   - a CONSTANT argument, which needed no row context at all and was refused
//     anyway, since the refusal never decided constness;
//   - and a body SELECT reading a TVF joined to a table the SAME body wrote,
//     which is the live half: a frozen image answers zero rows.
func TestTriggerBodyTableValuedFunctionRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"attach.test 5.10 verbatim", []string{
			`CREATE TABLE t1(x)`,
			`CREATE TABLE t2(a,b)`,
			`CREATE TRIGGER x1 AFTER INSERT ON t1 BEGIN INSERT INTO t2(a,b) SELECT key, value FROM json_each(NEW.x); END`,
			`INSERT INTO t1(x) VALUES('{"a":1}')`,
			`SELECT * FROM t2`,
		}},
		{"one INSERT firing three times, each with its own argument", []string{
			`CREATE TABLE t1(x)`,
			`CREATE TABLE t2(a,b)`,
			`CREATE TRIGGER x1 AFTER INSERT ON t1 BEGIN INSERT INTO t2(a,b) SELECT key, value FROM json_each(NEW.x); END`,
			`INSERT INTO t1(x) VALUES('{"a":1}'),('{"b":2,"c":3}'),('{"d":4}')`,
			`SELECT * FROM t2 ORDER BY rowid`,
		}},
		{"a constant argument", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT value FROM json_each('[1,2,3]'); END`,
			`INSERT INTO t VALUES(1)`,
			`SELECT * FROM log ORDER BY rowid`,
		}},
		{"nested inside a body subquery's own derived table", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM (SELECT value FROM json_each('[1,2]')))); END`,
			`INSERT INTO t VALUES(1)`,
			`SELECT * FROM log`,
		}},
		{"a WHEN guard over a TVF naming NEW", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t WHEN (SELECT count(*) FROM json_each(new.a))>1 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES('[1,2]')`,
			`INSERT INTO t VALUES('[9]')`,
			`SELECT * FROM log`,
		}},
		{"a VIEW over a TVF, materialized inside a body", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE log(n)`,
			`CREATE VIEW v AS SELECT value AS n FROM json_each('[5,6]')`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT n FROM v; END`,
			`INSERT INTO t VALUES(1)`,
			`SELECT * FROM log ORDER BY rowid`,
		}},
		{"a bare body SELECT over a TVF (rows discarded, and it must not error)", []string{
			`CREATE TABLE t(a,b)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT value FROM json_each('[1,2,3]'); END`,
			`INSERT INTO t VALUES(1,2)`,
			`SELECT * FROM t`,
		}},
		{"a bare body SELECT over a TVF naming NEW", []string{
			`CREATE TABLE t(a,b)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT value FROM json_each(new.b); END`,
			`INSERT INTO t VALUES(1,'[4,5]')`,
			`SELECT * FROM t`,
		}},
		{"OLD. on an UPDATE trigger, beside NEW.", []string{
			`CREATE TABLE t1(x)`,
			`CREATE TABLE t2(a,b)`,
			`INSERT INTO t1 VALUES('[1,2]')`,
			`CREATE TRIGGER x2 AFTER UPDATE ON t1 BEGIN INSERT INTO t2(a,b) SELECT OLD.x, value FROM json_each(NEW.x); END`,
			`UPDATE t1 SET x='[7,8,9]'`,
			`SELECT * FROM t2 ORDER BY rowid`,
		}},
		{"OLD. on a DELETE trigger", []string{
			`CREATE TABLE t1(x)`,
			`CREATE TABLE t2(a)`,
			`INSERT INTO t1 VALUES('[4,5,6]')`,
			`CREATE TRIGGER x3 AFTER DELETE ON t1 BEGIN INSERT INTO t2(a) SELECT value FROM json_each(OLD.x); END`,
			`DELETE FROM t1`,
			`SELECT * FROM t2 ORDER BY rowid`,
		}},
		{"a BEFORE trigger", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE log(n)`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log SELECT value FROM json_each(NEW.a); END`,
			`INSERT INTO t VALUES('[21,22]')`,
			`SELECT * FROM log ORDER BY rowid`,
		}},
		{"a NESTED trigger, whose inner body must read the INNER firing row", []string{
			`CREATE TABLE outerT(x)`,
			`CREATE TABLE mid(y)`,
			`CREATE TABLE log(n)`,
			`CREATE TRIGGER tm AFTER INSERT ON mid BEGIN INSERT INTO log SELECT value FROM json_each(NEW.y); END`,
			`CREATE TRIGGER tt AFTER INSERT ON outerT BEGIN INSERT INTO mid VALUES(NEW.x); END`,
			`INSERT INTO outerT VALUES('[11,12]')`,
			`INSERT INTO outerT VALUES('[13]')`,
			`SELECT * FROM log ORDER BY rowid`,
		}},
		{"a NEW. operand keeps its column's declared COLLATE NOCASE", []string{
			`CREATE TABLE t(a TEXT COLLATE NOCASE)`,
			`CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT count(*) FROM json_each('[1,2]') WHERE NEW.a='ABC'; END`,
			`INSERT INTO t VALUES('abc')`,
			`SELECT * FROM log`,
		}},
		{"a body UPDATE's SET, and a body DELETE's WHERE", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE s(k,n)`,
			`CREATE TABLE d(x)`,
			`INSERT INTO s VALUES(1,0)`,
			`INSERT INTO d VALUES(1),(2),(3)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN
			   UPDATE s SET n=(SELECT count(*) FROM json_each(NEW.a));
			   DELETE FROM d WHERE x IN (SELECT value FROM json_each(NEW.a));
			 END`,
			`INSERT INTO t VALUES('[1,3]')`,
			`SELECT * FROM s`,
			`SELECT * FROM d ORDER BY x`,
		}},
		{"the read is LIVE: the TVF is joined to a table the same body wrote", []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE seen(j)`,
			`CREATE TABLE log(n)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN
			   INSERT INTO seen VALUES(NEW.a);
			   INSERT INTO log SELECT count(*) FROM seen, json_each(NEW.a);
			 END`,
			`INSERT INTO t VALUES('[1,2]'),('[3]')`,
			`SELECT * FROM log ORDER BY rowid`,
		}},
		{"recursive triggers, so the fold is re-run per recursion level", []string{
			`PRAGMA recursive_triggers=ON`,
			`CREATE TABLE t(a)`,
			`CREATE TRIGGER tb AFTER INSERT ON t WHEN (SELECT count(*) FROM t)<3 BEGIN INSERT INTO t SELECT value FROM json_each(NEW.a); END`,
			`INSERT INTO t VALUES('[1]')`,
			`SELECT count(*) FROM t`,
		}},
	} {
		differ(t, "trigger-tvf/"+tc.name, tc.stmts)
	}
}
