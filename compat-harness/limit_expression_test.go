// Tests LIMIT/OFFSET as general expressions, not just integer literals.
package compat

import "testing"

func TestLimitOffsetExpression(t *testing.T) {
	differ(t, "LIMIT/OFFSET as an expression", []string{
		`CREATE TABLE t4(a)`,
		`INSERT INTO t4 VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE t5(a)`,
		`INSERT INTO t5 VALUES(2)`,
		// subquery2.test's own statement.
		`SELECT * FROM (SELECT * FROM t4 ORDER BY a LIMIT -1 OFFSET 1) LIMIT (SELECT a FROM t5)`,
		`SELECT a FROM t4 LIMIT 1+1`,
		`SELECT a FROM t4 LIMIT (SELECT a FROM t5)`,
		`SELECT a FROM t4 LIMIT abs(-2)`,
		`SELECT a FROM t4 LIMIT 2 OFFSET 1+1`,
		`SELECT a FROM t4 LIMIT (SELECT a FROM t5) OFFSET (SELECT a-1 FROM t5)`,
		`SELECT a FROM t4 LIMIT 6-4, 2`,
		// A CORRELATED-looking subquery in LIMIT is still evaluated once,
		// against no outer row.
		`SELECT a FROM t4 LIMIT (SELECT count(*) FROM t5)`,
		// The plain-literal fast path must be untouched.
		`SELECT a FROM t4 LIMIT 2`,
		`SELECT a FROM t4 LIMIT -1`,
		`SELECT a FROM t4 LIMIT 2 OFFSET 1`,
		`SELECT a FROM t4 LIMIT 1, 2`,
		`SELECT a FROM t4 ORDER BY a DESC LIMIT 2`,
	})
	// The coercion boundary, every case measured against 3.53.3.
	differ(t, "LIMIT value coercion", []string{
		`CREATE TABLE t4(a)`,
		`INSERT INTO t4 VALUES(1),(2),(3),(4),(5)`,
		`SELECT a FROM t4 LIMIT 2.0`,
		`SELECT a FROM t4 LIMIT 2.5`,
		`SELECT a FROM t4 LIMIT '2'`,
		`SELECT a FROM t4 LIMIT 'x'`,
		`SELECT a FROM t4 LIMIT NULL`,
		`SELECT a FROM t4 LIMIT x'32'`,
		`SELECT a FROM t4 LIMIT TRUE`,
		`SELECT a FROM t4 LIMIT 1e2`,
		`SELECT a FROM t4 LIMIT (SELECT NULL)`,
		`SELECT a FROM t4 LIMIT 3 OFFSET NULL`,
		`SELECT a FROM t4 LIMIT 3 OFFSET 1.5`,
	})
	// A compound's own trailing LIMIT, and one inside a CTE body: both reach
	// execSelect's one-time resolution, by different routes.
	differ(t, "LIMIT expression in nested positions", []string{
		`CREATE TABLE t4(a)`,
		`INSERT INTO t4 VALUES(1),(2),(3),(4),(5)`,
		`SELECT a FROM t4 UNION ALL SELECT a FROM t4 LIMIT 2+1`,
		`WITH c AS (SELECT a FROM t4 LIMIT 3-1) SELECT * FROM c`,
		// A literal LIMIT inside a derived table is unaffected.
		`SELECT * FROM (SELECT a FROM t4 LIMIT 2)`,
	})
	// A LIMIT/OFFSET expression inside a DERIVED TABLE never reaches
	// execSelect's resolution step: the body is compiled straight to a
	// sub-Program by compileSubProgram (vdbe_codegen.go), which resolves the
	// clause itself for the length of that compile
	// (resolveSubProgramLimitOffset). The first three below used to be
	// asserted as one-sided DECLINES here -- that assertion went stale when
	// that function landed, and the engine has agreed with the oracle on them
	// since, so they are gated as ordinary differ() cases now.
	differ(t, "LIMIT expression in a derived table", []string{
		`CREATE TABLE t4(a)`,
		`INSERT INTO t4 VALUES(1),(2),(3),(4),(5)`,
		`SELECT * FROM (SELECT a FROM t4 LIMIT 1+1)`,
		`SELECT * FROM (SELECT a FROM t4 LIMIT (SELECT 2))`,
		`SELECT * FROM (SELECT a FROM t4 LIMIT 3 OFFSET 1+1)`,
		`SELECT * FROM (SELECT a FROM t4 LIMIT abs(-2))`,
		`SELECT * FROM (SELECT a FROM t4 LIMIT CASE WHEN 1 THEN 2 ELSE 4 END)`,
	})
}

// TestTriggerBodyLimitExpressionIsNotFoldedAtCompileTime is the wrong answer
// that made resolveSubProgramLimitOffset (engine/vdbe_codegen.go) UNDO its own
// resolution instead of leaving it in the AST.
//
// compileTriggerFirePlan (engine/vdbe_trigger.go) compiles a trigger's
// BODY statements while compiling the FIRING statement -- before that
// statement has written a single row. Resolving "LIMIT (SELECT count(*) FROM
// ctr)" there evaluates the subquery against the PRE-insert table, which is
// fine for the Program that compile produces (it is declined anyway, by
// programReadsFrozenSnapshot, for reading exactly that stale snapshot) -- but
// the resolution was written back into the trigger's own body AST, which is
// held in db.triggers and reused. So every LATER firing re-read a bound that
// had already been folded to the pre-insert count, and nothing downstream
// re-resolved the clause at the moment it should have been evaluated. The
// oracle answers 1,2,3 (the count AFTER the row lands); musql answered 1,2.
//
// C SQLite cannot have this bug: computeLimitRegisters (select.c:2547-2562)
// CODES the expression into the LIMIT register and evaluates it when the
// SELECT runs, folding only what sqlite3ExprIsInteger accepts (expr.c:2899).
func TestTriggerBodyLimitExpressionIsNotFoldedAtCompileTime(t *testing.T) {
	differ(t, "trigger body LIMIT expression", []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE ctr(a)`,
		`CREATE TABLE dst(a)`,
		`INSERT INTO ctr VALUES(1),(2)`,
		`CREATE TRIGGER tg AFTER INSERT ON ctr BEGIN INSERT INTO dst SELECT a FROM src LIMIT (SELECT count(*) FROM ctr); END`,
		`INSERT INTO ctr VALUES(3)`,
		`SELECT a FROM dst ORDER BY a`,
	})
	differ(t, "trigger body OFFSET expression", []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE ctr(a)`,
		`CREATE TABLE dst(a)`,
		`INSERT INTO ctr VALUES(1),(2)`,
		`CREATE TRIGGER tg AFTER INSERT ON ctr BEGIN INSERT INTO dst SELECT a FROM src LIMIT 2 OFFSET (SELECT count(*) FROM ctr); END`,
		`INSERT INTO ctr VALUES(3)`,
		`SELECT a FROM dst ORDER BY a`,
	})
	// The same trigger firing TWICE: the second firing must see the second
	// row's count, not a bound folded (and cached) during the first.
	differ(t, "trigger body LIMIT expression, fired twice", []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE ctr(a)`,
		`CREATE TABLE dst(a)`,
		`CREATE TRIGGER tg AFTER INSERT ON ctr BEGIN INSERT INTO dst SELECT a FROM src LIMIT (SELECT count(*) FROM ctr); END`,
		`INSERT INTO ctr VALUES(1)`,
		`INSERT INTO ctr VALUES(2)`,
		`SELECT a FROM dst ORDER BY rowid`,
	})
	// A constant LIMIT in the same position is unaffected -- it resolves to
	// the same value whenever it is evaluated.
	differ(t, "trigger body constant LIMIT expression", []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE ctr(a)`,
		`CREATE TABLE dst(a)`,
		`CREATE TRIGGER tg AFTER INSERT ON ctr BEGIN INSERT INTO dst SELECT a FROM src LIMIT 1+1; END`,
		`INSERT INTO ctr VALUES(1)`,
		`SELECT a FROM dst ORDER BY a`,
	})
	// The same AST-poisoning reached a DELETE body's WHERE subquery too: its
	// LIMIT was resolved once, during the firing statement's compile, and the
	// value stuck for every later firing.
	differ(t, "trigger body DELETE whose WHERE subquery carries a LIMIT expression", []string{
		`CREATE TABLE ctr(a)`,
		`CREATE TABLE dst(a)`,
		`INSERT INTO dst VALUES(1),(2),(3),(4),(5)`,
		`CREATE TRIGGER tg AFTER INSERT ON ctr BEGIN DELETE FROM dst WHERE a IN (SELECT a FROM dst ORDER BY a LIMIT (SELECT count(*) FROM ctr)); END`,
		`INSERT INTO ctr VALUES(1)`,
		`SELECT a FROM dst ORDER BY a`,
		`INSERT INTO ctr VALUES(2)`,
		`SELECT a FROM dst ORDER BY a`,
	})
	// And the plain (trigger-free) repeat: the same INSERT ... SELECT text run
	// twice must resolve its LIMIT against each execution's own table, not
	// against the first compile's. This one passed even with the bug -- an
	// INSERT ... SELECT program carries Program.WritePager, so
	// cachedWriteProgram never reuses it, and driver.Stmt reparses the SQL
	// text fresh every run so there is no shared AST to poison. It is here to
	// pin those two properties, which are what confine the resolution above to
	// a single execution.
	differ(t, "repeated INSERT ... SELECT with a subquery LIMIT", []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(2),(3),(4),(5)`,
		`CREATE TABLE ctr(a)`,
		`CREATE TABLE dst(a)`,
		`INSERT INTO ctr VALUES(1)`,
		`INSERT INTO dst SELECT a FROM src LIMIT (SELECT count(*) FROM ctr)`,
		`INSERT INTO ctr VALUES(2),(3)`,
		`INSERT INTO dst SELECT a FROM src LIMIT (SELECT count(*) FROM ctr)`,
		`SELECT a FROM dst ORDER BY rowid`,
	})
}
