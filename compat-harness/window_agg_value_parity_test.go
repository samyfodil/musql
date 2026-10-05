// Oracle parity for value families that were the last things to be lowered below OpAggStep/OpWindowFinal.
// Each site either serves a lowered value or the statement declines at compile time.
package compat

import "testing"

var windowAggParityFixture = []string{
	`CREATE TABLE w(a, b TEXT, c TEXT COLLATE NOCASE, d)`,
	`INSERT INTO w VALUES(1,'A','apple',10),(2,'B','APPLE',20),(3,'C','pear',30),` +
		`(3,'D','PEAR',30),(5,'E','fig',50),(7,'F','Fig',70),(NULL,'G','date',NULL)`,
	`CREATE TABLE u(k, v)`,
	`INSERT INTO u VALUES(1,100),(2,200),(3,300),(9,900)`,
	// g is the GROUP BY workhorse. Its key is plain BINARY text on purpose --
	// see the package comment above on what an earlier NOCASE key measured.
	`CREATE TABLE g(k TEXT, n, s TEXT)`,
	`INSERT INTO g VALUES('a',1,'x'),('a',2,'yy'),('b',3,'zzz'),('b',-4,'w'),` +
		`('c',NULL,NULL),('c',5,'5x'),(NULL,6,'12'),('d',9223372036854775807,'q')`,
}

func windowAggParity(t *testing.T, name string, probes []string) {
	t.Helper()
	driverParity(t, name, append(append([]string(nil), windowAggParityFixture...), probes...))
}

// TestWindowProjectionParity drives projectWindowRow over the scopes that were
// the last to lower: a JOIN or a coalescing scope, which windowProjScopeLowerable
// (engine/vdbe_window_codegen.go) still refuses, so each of these reaches
// compileWindowProjection's own emitter rather than the common path.
func TestWindowProjectionParity(t *testing.T) {
	windowAggParity(t, "window-projection", []string{
		`SELECT w.a, u.v, sum(w.d) OVER () FROM w, u WHERE w.a=u.k ORDER BY w.a`,
		// A CAST beside a second read of its own operand. This shape was once
		// answered WRONG here (G2: OP_Cast rewrote the operand's register, so a
		// shared row block was corrupted for every later reader). The fix is
		// what this case exists to hold.
		`SELECT CAST(w.b AS INTEGER) || '-' || w.b, count(*) OVER () FROM w, u WHERE w.a=u.k ORDER BY w.a`,
		`SELECT w.c = 'APPLE', w.c COLLATE BINARY = 'APPLE', row_number() OVER (ORDER BY w.a) FROM w, u WHERE w.a=u.k ORDER BY w.a`,
		`SELECT w.a+u.v, -u.v, u.v%3, w.d*1.0/u.v, sum(u.v) OVER () FROM w JOIN u ON w.a=u.k ORDER BY w.a`,
		`SELECT abs(w.a), length(w.b), upper(w.c), sum(w.d) OVER (PARTITION BY w.c) FROM w LEFT JOIN u ON w.a=u.k ORDER BY w.b`,
		`SELECT w.a IS NULL, u.k IS NULL, coalesce(u.v,-1), nullif(w.a,3), count(*) OVER () FROM w LEFT JOIN u ON w.a=u.k ORDER BY w.b`,
		`SELECT (SELECT max(k) FROM u), w.a IN (SELECT k FROM u), sum(w.d) OVER () FROM w, u WHERE w.a=u.k ORDER BY w.a`,
		`SELECT CASE WHEN w.a>2 THEN w.b ELSE w.c END, sum(u.v) OVER () FROM w, u WHERE w.a=u.k ORDER BY w.a`,
		`SELECT w.*, count(*) OVER () FROM w, u WHERE w.a=u.k ORDER BY w.a`,
		// An expression that FAILS. The arm must raise, not answer.
		`SELECT abs(-9223372036854775808), count(*) OVER () FROM w, u WHERE w.a=u.k`,
		// RIGHT/FULL JOIN: a coalescing scope, which resolveRowReg has no
		// equivalent of at all -- the value is decided by the ROW, not by a
		// static register index.
		`SELECT w.a, u.k, sum(u.v) OVER () FROM w RIGHT JOIN u ON w.a=u.k ORDER BY u.k`,
		`SELECT a, k, count(*) OVER () FROM w FULL JOIN u ON w.a=u.k ORDER BY k, b`,
	})
}

// TestWindowProjectionRejectionParity pins the errors a JOIN-scope window
// projection RAISES, which is what any future widening of the lowering has to
// keep raising.
//
// For a JOIN, name resolution is the ONLY thing that raises them:
// windowProjScopeLowerable refuses a join scope, and expandSelectList resolves
// only names and "*" expansion -- so "SELECT x, count(*) OVER () FROM p, q"
// reaches windowFinal and is rejected there, "ambiguous column name: x", by
// resolveColumnEx (engine/column_scope.go). That is exactly why the join
// guard is kept: compiler.resolveRowReg is LAST-scope-wins with no ambiguity
// check, so a lowering that went through it would ANSWER from one arbitrary
// table where 3.53.3 errors -- the wrong-answer half of the disagreement, which
// invariant 2 weighs above the extra shape lowered.
func TestWindowProjectionRejectionParity(t *testing.T) {
	driverParity(t, "window-projection-rejections", []string{
		`CREATE TABLE p(x,y)`, `CREATE TABLE q(x,z)`,
		`INSERT INTO p VALUES(1,10),(2,20)`,
		`INSERT INTO q VALUES(1,100),(3,300)`,
		`SELECT x, count(*) OVER () FROM p, q ORDER BY 1`,
		`SELECT rowid, count(*) OVER () FROM p, q ORDER BY 1`,
		`SELECT nosuch, count(*) OVER () FROM p, q`,
		`SELECT p.x, q.x, count(*) OVER () FROM p, q ORDER BY 1,2`,
		`SELECT p.rowid, q.rowid, count(*) OVER () FROM p, q ORDER BY 1,2`,
		`SELECT x, count(*) OVER () FROM p JOIN q USING(x) ORDER BY 1`,
		`SELECT x, count(*) OVER () FROM p NATURAL JOIN q ORDER BY 1`,
		`SELECT p.y+q.z, count(*) OVER () FROM p, q ORDER BY 1`,
		`SELECT p.*, count(*) OVER () FROM p, q ORDER BY 1`,
	})
}

// TestWindowOperandParity drives the operand shapes windowOperandLowerable is
// most likely to get wrong: a PARTITION BY / ORDER BY key or a positional window
// argument holding a subquery, a correlated reference, an aggregate or a nested
// window call, plus the join scope. Each either lowers into the batch's operand
// block (windowOperand then reads it as a plain column) or declines outright,
// and this pins WHICH against the oracle.
func TestWindowOperandParity(t *testing.T) {
	windowAggParity(t, "window-operand", []string{
		`SELECT a, sum(d) OVER (PARTITION BY (SELECT max(k) FROM u)) FROM w ORDER BY b`,
		`SELECT a, sum(d) OVER (ORDER BY (SELECT count(*) FROM u WHERE k=w.a)) FROM w ORDER BY b`,
		`SELECT a, nth_value(b, (SELECT 2)) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT a, group_concat(b, (SELECT '/')) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT a, sum(d) FILTER (WHERE (SELECT 1)) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT w.a, sum(w.d) OVER (PARTITION BY u.k) FROM w, u WHERE w.a=u.k ORDER BY w.a`,
		`SELECT w.a, sum(w.d) OVER (ORDER BY u.v) FROM w, u WHERE w.a=u.k ORDER BY w.a`,
		// The collated operand cases, which the batch column has to preserve:
		// a PARTITION BY / ORDER BY key over a NOCASE column, and the same key
		// with an explicit BINARY override.
		`SELECT a, sum(d) OVER (PARTITION BY c) FROM w ORDER BY b`,
		`SELECT a, sum(d) OVER (PARTITION BY c COLLATE BINARY) FROM w ORDER BY b`,
		`SELECT a, sum(d) OVER (ORDER BY c) FROM w ORDER BY b`,
		// json_group_array/json_group_object are SQLITE_SUBTYPE aggregates,
		// which C itself re-codes at step time rather than buffering
		// (pWin->bExprArgs, window.c:1041-1044) -- so they stay off the
		// allow-list and take the separate step-time route.
		`SELECT a, json_group_array(b) OVER (ORDER BY a) FROM w ORDER BY b`,
	})
}

// TestAggResultParity drives aggResult -- an aggregate item's REWRITTEN tree
// (rewriteGroupExpr, engine/sql_group.go), run once per group as its own
// program with the finalized accumulator values and the group key seeded into
// its register block.
func TestAggResultParity(t *testing.T) {
	windowAggParity(t, "agg-result", []string{
		`SELECT k, count(*), CAST(k AS INTEGER) || '-' || k FROM g GROUP BY k ORDER BY k`,
		`SELECT k, sum(n)+max(n)*2, abs(sum(n)), -sum(n), sum(n)%7 FROM g GROUP BY k ORDER BY k`,
		`SELECT k, CASE WHEN count(*)>1 THEN 'many' ELSE 'one' END FROM g GROUP BY k ORDER BY k`,
		`SELECT k, k || '=' || count(*) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, CAST(sum(n) AS TEXT) || '/' || CAST(sum(n) AS TEXT) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, sum(n) FROM g GROUP BY k ORDER BY sum(n) DESC, k`,
		// A correlated subquery in the item body reads the group's ANCHOR row.
		`SELECT k, max(n), (SELECT v FROM u WHERE u.k=g.n) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT count(*) FROM u WHERE u.k=g.n) FROM g GROUP BY k ORDER BY k`,
		// HAVING is its own item set, evaluated through the same arm.
		`SELECT k, count(*) FROM g GROUP BY k HAVING (SELECT max(v) FROM u) > 100 ORDER BY k`,
		`SELECT k, count(*) FROM g GROUP BY k HAVING count(*) > (SELECT count(*) FROM u WHERE u.k=1) ORDER BY k`,
		`SELECT k, sum(n) FROM g GROUP BY k HAVING sum(n) IN (SELECT v FROM u) ORDER BY k`,
		`SELECT k, max(n)*2, min(n) FROM g GROUP BY k HAVING max(n)>2 ORDER BY k`,
		// A whole-table aggregate (groupKey nil) takes the same route.
		`SELECT sum(n)+1, count(*), abs(min(n)), count(*), abs(-9223372036854775808) FROM g`,
		`SELECT CAST(max(s) AS INTEGER) || '-' || max(s) FROM g`,
		// A BARE column beside an aggregate: the anchor row the min/max census
		// picks decides its value, and it must be the same row the correlated
		// subquery above reads.
		`SELECT k, n, count(*) FROM g GROUP BY k ORDER BY k`,
	})
}

// TestAggRowValueParity drives aggItem.rowValue: an aggregate ARGUMENT,
// separator or FILTER, each of which must have a register behind it.
//
// The sorted GROUP BY drain used to be the bulk of what did NOT -- planAggArgRegs
// never stamped that plan, so every accumulator on it walked its argument -- and
// it no longer is: the drain reads its leaves out of the sorter record now
// (aggDrainRow, engine/vdbe_agg_codegen.go). The last shape to hold out was the
// "nothing AFTER a raising step body" ordering rule (aggStepBodyMayRaise), which
// is why the json_group_object case below measured 16 reaches and the rest of
// this block measured none. See this file's header for the re-measurement and
// for why the now-lowered statements are kept.
func TestAggRowValueParity(t *testing.T) {
	windowAggParity(t, "row-value", []string{
		`SELECT k, sum(n*2), max(n+1), count(s||'x') FROM g GROUP BY k ORDER BY k`,
		`SELECT k, sum(CAST(s AS INTEGER)), count(*) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, sum(n+1), total(n), avg(n) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, max(s||''), min(s||'') FROM g GROUP BY k ORDER BY k`,
		`SELECT k, count(nullif(n,3)), sum(coalesce(n,0)) FROM g GROUP BY k ORDER BY k`,
		// The separator slot, constant and computed.
		`SELECT k, group_concat(s, '|'), count(*) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, group_concat(s, CAST(n AS TEXT)) FROM g GROUP BY k ORDER BY k`,
		// The FILTER slot. A filtered-out row must not evaluate its argument at
		// all, which is the ordering the lowering is required to preserve.
		`SELECT k, sum(n) FILTER (WHERE n>2), count(*) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, sum(n) FILTER (WHERE s IS NOT NULL), group_concat(s) FROM g GROUP BY k ORDER BY k`,
		// DISTINCT dedup and the JSON aggregates, both stepped through this slot.
		`SELECT k, sum(DISTINCT n), count(DISTINCT s) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, json_group_array(s), json_group_object(s, n) FROM g GROUP BY k ORDER BY k`,
		// Arguments that FAIL, on a row some group does and does not contain.
		`SELECT k, sum(CASE WHEN n=6 THEN abs(-9223372036854775808) ELSE n END) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, sum(n/0), count(*) FROM g GROUP BY k ORDER BY k`,
		`SELECT k, sum(n+9223372036854775807) FROM g GROUP BY k ORDER BY k`,
		// Over a join, and drained in a non-key order.
		`SELECT g.k, sum(u.v) FROM g LEFT JOIN u ON u.k=g.n GROUP BY g.k ORDER BY g.k`,
		`SELECT k, count(*) FROM g GROUP BY k ORDER BY k DESC`,
		`SELECT k, count(*), sum(n) FROM g GROUP BY k ORDER BY sum(n)`,
	})
}

// TestSchemaTimeExprParity is the fifth site's CONTROL: selfRowExpr.eval's
// decline (engine/vdbe_run.go), covering a schema-time expression -- a CHECK
// constraint, a generated column's body, a partial index's WHERE, an expression
// index's key.
//
// It is a CONTROL, not a probe: instrumenting the decline and running the whole
// engine suite showed that NOTHING but its own guard test
// (TestSelfRowExprEvalGuards) reaches it -- every schema expression the corpus
// writes compiles. These shapes therefore measure the compiled half, including
// the two cases G2 found wrong there (a CAST persisting the wrong value into a
// stored generated column, and a CHECK rejecting a row 3.53.3 accepts).
func TestSchemaTimeExprParity(t *testing.T) {
	driverParity(t, "schema-time", []string{
		`CREATE TABLE s1(a TEXT, b AS (CAST(a AS INTEGER) || '-' || a))`,
		`INSERT INTO s1(a) VALUES('5x'),('12'),(NULL)`,
		`SELECT a, b FROM s1 ORDER BY a`,
		`CREATE TABLE s2(a TEXT, b AS (CAST(a AS INTEGER) || '-' || a) STORED)`,
		`INSERT INTO s2(a) VALUES('5x'),('12'),(NULL)`,
		`SELECT a, b FROM s2 ORDER BY a`,
		`CREATE TABLE s3(a TEXT, CHECK(CAST(a AS INTEGER)=5 AND a='5x'))`,
		`INSERT INTO s3(a) VALUES('5x')`,
		`SELECT * FROM s3`,
		`CREATE TABLE s4(a, b)`,
		`CREATE INDEX s4i ON s4(abs(a)) WHERE b > 1`,
		`INSERT INTO s4 VALUES(-5,2),(3,0),(-1,9)`,
		`SELECT a FROM s4 WHERE abs(a)=5 AND b>1`,
		`SELECT a FROM s4 ORDER BY abs(a)`,
		`CREATE TABLE s5(a TEXT COLLATE NOCASE, b AS (a='ABC'))`,
		`INSERT INTO s5(a) VALUES('abc'),('ABC'),('x')`,
		`SELECT a, b FROM s5 ORDER BY rowid`,
		`CREATE TABLE s6(a, b AS (a*2) STORED, c AS (b+1))`,
		`INSERT INTO s6(a) VALUES(3),(NULL),('x')`,
		`SELECT a, b, c FROM s6 ORDER BY rowid`,
	})
}
