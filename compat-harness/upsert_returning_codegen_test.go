// Tests the compiled INSERT ... ON CONFLICT ... RETURNING route. The
// RETURNING block now emits correctly for each arm of the upsert, where the
// old row-store path declined this clause with a silent wrong answer.
package compat

import "testing"

func TestUpsertReturningCompiledShapes(t *testing.T) {
	// A RETURNING list that is not just columns, over both arms in one
	// statement.
	differ(t, "upsert RETURNING expressions over both arms", []string{
		`CREATE TABLE t(a UNIQUE, b, c)`,
		`INSERT INTO t VALUES(1,22,33)`,
		`INSERT INTO t(a,b,c) VALUES(1,'x','y'),(2,'p','q') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING *`,
		`INSERT INTO t(a,b) VALUES(1,'z') ON CONFLICT(a) DO UPDATE SET b='w' RETURNING a+1, upper(b), b||'!', typeof(c)`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT changes()`,
	})
	// A generated column is re-derived for the RETURNING image rather than read
	// out of the stored record; a WITHOUT ROWID target has no rowid register.
	differ(t, "upsert RETURNING with a generated column and a WITHOUT ROWID target", []string{
		`CREATE TABLE g(a UNIQUE, b, x AS (b*2))`,
		`INSERT INTO g(a,b) VALUES(1,10)`,
		`INSERT INTO g(a,b) VALUES(1,50) ON CONFLICT(a) DO UPDATE SET b=99 RETURNING *`,
		`SELECT a,b,x FROM g`,
		`CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO w VALUES('a',1)`,
		`INSERT INTO w VALUES('a',2) ON CONFLICT(k) DO UPDATE SET v=v+10 RETURNING k,v`,
		`INSERT INTO w VALUES('b',3) ON CONFLICT(k) DO UPDATE SET v=v+10 RETURNING *`,
		`SELECT k,v FROM w ORDER BY k`,
	})
	// A CHECK that fails on the DO UPDATE arm must still fail, with the row
	// unchanged -- the RETURNING block sits below the checks, not around them.
	differ(t, "upsert RETURNING with a CHECK that fails", []string{
		`CREATE TABLE t(a UNIQUE, b CHECK(b<100))`,
		`INSERT INTO t VALUES(1,5)`,
		`INSERT INTO t VALUES(1,7) ON CONFLICT(a) DO UPDATE SET b=b+1 RETURNING a,b`,
		`INSERT INTO t VALUES(1,7) ON CONFLICT(a) DO UPDATE SET b=500 RETURNING a,b`,
		`SELECT a,b FROM t`,
	})
	// "excluded" and the target's alias are both invisible to RETURNING.
	// Both must be prepare-time errors.
	differ(t, "upsert RETURNING sees neither excluded nor the target alias", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b=excluded.b RETURNING excluded.b`,
		`INSERT INTO t AS base VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b=excluded.b||'!' RETURNING base.a`,
		`INSERT INTO t AS base VALUES(1,'dup2') ON CONFLICT(a) DO UPDATE SET b=excluded.b||'?' RETURNING t.a, a, b`,
		`SELECT a,b FROM t`,
	})
}

// TestUpsertReturningOverASelectSource tests the compiled route for UPSERT
// over an INSERT ... SELECT source, which the old path declined.
func TestUpsertReturningOverASelectSource(t *testing.T) {
	differ(t, "upsert RETURNING over a SELECT source", []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`CREATE TABLE s(a, b)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO s VALUES(1,'sx'),(2,'sy')`,
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY a`,
		`SELECT changes()`,
	})
	// The source rows conflict with EACH OTHER, so the second meets a row the
	// first just inserted: the probe is against the live store, and each row's
	// RETURNING image is the one that row left behind.
	differ(t, "upsert RETURNING over a self-conflicting SELECT source", []string{
		`CREATE TABLE t(a UNIQUE, b, c)`,
		`CREATE TABLE s(a, b)`,
		`INSERT INTO s VALUES(1,'first'),(1,'second'),(2,'other')`,
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=coalesce(c,0)+1 RETURNING a,b,c`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT changes()`,
	})
}

// TestUpsertReturningWithTriggers tests UPSERT with RETURNING against
// triggered tables, which the old path declined with errors.
func TestUpsertReturningWithTriggers(t *testing.T) {
	// INSERT triggers: BEFORE fires for every candidate, AFTER only for the one
	// that really inserted (the DO UPDATE arm reaches ignoreDest instead).
	differ(t, "upsert RETURNING on a table with INSERT triggers", []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tbi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('bi'||new.a); END`,
		`CREATE TRIGGER tai AFTER INSERT ON t BEGIN INSERT INTO log VALUES('ai'||new.a); END`,
		`INSERT INTO t VALUES(1,'dup'),(2,'two') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// UPDATE triggers around the DO UPDATE arm, the AFTER one logging the NEW
	// image beside the OLD.
	differ(t, "upsert RETURNING on a table with UPDATE triggers", []string{
		`CREATE TABLE t(a UNIQUE, b, c DEFAULT 0)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t(a,b) VALUES(1,'one')`,
		`CREATE TRIGGER tbu BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES('bu:'||old.c); END`,
		`CREATE TRIGGER tau AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||old.c||'->'||new.c); END`,
		`INSERT INTO t(a,b) VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET c=c+1 RETURNING a,b,c`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b,c FROM t`,
	})
	// A user AFTER program that RAISEs IGNORE runs BELOW the captured row, on
	// each arm in turn.
	differ(t, "upsert RETURNING survives an AFTER UPDATE RAISE(IGNORE)", []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
		`SELECT a,b FROM t`,
	})
	differ(t, "upsert RETURNING survives an AFTER INSERT RAISE(IGNORE)", []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ti AFTER INSERT ON t BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t VALUES(2,'two') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// A BEFORE UPDATE program that edits the row. The RETURNING list sees
	// the edited row, not the original.
	differ(t, "upsert RETURNING sees a BEFORE UPDATE program's edit", []string{
		`CREATE TABLE t(a UNIQUE, b, c)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'b1','c1')`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN UPDATE t SET c='trig' WHERE a=old.a; END`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('new.c='||new.c); END`,
		`INSERT INTO t(a,b) VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b,c`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b,c FROM t`,
	})
	// With a generated column, which is re-derived after a BEFORE UPDATE edit
	// rather than reloaded from storage.
	differ(t, "upsert RETURNING re-derives a generated column after a BEFORE UPDATE edit", []string{
		`CREATE TABLE t(a UNIQUE, b, c, g AS (b||'/'||c))`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t(a,b,c) VALUES(1,'b1','c1')`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN UPDATE t SET c='trig' WHERE a=old.a; END`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('new.g='||new.g); END`,
		`INSERT INTO t(a,b) VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b,c,g`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b,c,g FROM t`,
	})
	// A BEFORE UPDATE program that deletes the row: RETURNING is skipped too.
	differ(t, "upsert RETURNING when a BEFORE UPDATE program deletes the row", []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`INSERT INTO t VALUES(1,'one'),(2,'two')`,
		`CREATE TRIGGER tbd BEFORE UPDATE ON t BEGIN DELETE FROM t WHERE a=old.a; END`,
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}

// TestUpsertReturningSubqueryStillAnswers verifies subqueries in RETURNING
// work correctly over both upsert arms.
func TestUpsertReturningSubqueryStillAnswers(t *testing.T) {
	differ(t, "upsert RETURNING carrying a subquery", []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2)`,
		`INSERT INTO t VALUES(1,'one')`,
		// Over another table: subquery is evaluated once.
		`INSERT INTO t VALUES(1,'dup'),(3,'three') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,(SELECT count(*) FROM s)`,
		// Over the modified table: subquery is re-evaluated per row.
		`INSERT INTO t VALUES(1,'dup2'),(4,'four') ON CONFLICT(a) DO UPDATE SET b='u2' RETURNING a,(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Each arm has its own subquery lifetime across alternating VALUES.
	for _, ret := range []string{
		`RETURNING a,b,(SELECT count(*) FROM s),(SELECT group_concat(x) FROM s)`,
		`RETURNING a,b,(SELECT count(*) FROM t),(SELECT max(a) FROM t)`,
	} {
		differ(t, "upsert RETURNING subqueries across alternating arms: "+ret, []string{
			`CREATE TABLE t(a UNIQUE, b)`,
			`CREATE TABLE s(x)`,
			`INSERT INTO s VALUES(1),(2),(3)`,
			`INSERT INTO t VALUES(1,'one'),(2,'two')`,
			`INSERT INTO t VALUES(1,'a'),(5,'b'),(2,'c'),(6,'d'),(7,'e') ON CONFLICT(a) DO UPDATE SET b=excluded.b ` + ret,
			`INSERT INTO s VALUES(4)`,
			`INSERT INTO t VALUES(8,'f'),(1,'g'),(9,'h'),(5,'i') ON CONFLICT(a) DO UPDATE SET b=excluded.b ` + ret,
			`SELECT a,b FROM t ORDER BY a`,
		})
		// INSERT ... SELECT: one block per arm inside the source scan's loop.
		differ(t, "upsert RETURNING subqueries from INSERT ... SELECT: "+ret, []string{
			`CREATE TABLE t(a UNIQUE, b)`,
			`CREATE TABLE s(x)`,
			`INSERT INTO s VALUES(1),(2),(3),(4)`,
			`INSERT INTO t VALUES(2,'two'),(4,'four')`,
			`INSERT INTO t SELECT x, 'sel' FROM s WHERE true ON CONFLICT(a) DO UPDATE SET b='upd' ` + ret,
			`SELECT a,b FROM t ORDER BY a`,
		})
	}
	// Mixing both lifetimes may decline but never gives wrong answers.
	differAllowingDeclines(t, "upsert RETURNING mixing the two lifetimes", []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(3)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO t VALUES(1,'a'),(5,'b') ON CONFLICT(a) DO UPDATE SET b=excluded.b ` +
			`RETURNING a,(SELECT count(*) FROM s),(SELECT count(*) FROM t)`,
	})
	// Correlated subqueries may decline but never answer wrong.
	differAllowingDeclines(t, "upsert RETURNING subquery correlated to the row", []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(3)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO t VALUES(1,'a'),(3,'b') ON CONFLICT(a) DO UPDATE SET b=excluded.b ` +
			`RETURNING a,(SELECT count(*) FROM s WHERE s.x<=a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}
