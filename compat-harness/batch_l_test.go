// Tests differential behavior for upsert ROWID reassignment and INSERT with
// recursive triggers on conflict resolution.
package compat

import "testing"

// TestUpsertRowidReassignDiff tests DO UPDATE with ROWID reassignment.
func TestUpsertRowidReassignDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"moves-the-row", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'x'),(5,'z')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=9`,
			`SELECT a,b,rowid FROM t ORDER BY a`,
			`SELECT changes(), last_insert_rowid()`,
		}},
		{"collides-with-an-existing-rowid", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'x'),(5,'z')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=5`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT changes()`,
		}},
		{"excluded-and-a-second-column", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(9,'x')`,
			`INSERT INTO t VALUES(9,'w') ON CONFLICT(a) DO UPDATE SET a=a+100, b=excluded.b`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		// The one-operand OP_MustBeInt (update.c:894) has no jump target and
		// no onError, so each of these is a hard "datatype mismatch" -- never a
		// skipped row, never an auto-assigned rowid.
		{"non-integer-key", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a='abc'`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"null-key", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=NULL`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"non-integral-real-key", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=7.5`,
			`SELECT a,b,typeof(a) FROM t ORDER BY a`,
		}},
		{"integral-real-and-numeric-text-keys", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=7.0`,
			`SELECT a,b,typeof(a) FROM t ORDER BY a`,
			`INSERT INTO t VALUES(7,'y') ON CONFLICT(a) DO UPDATE SET a='8'`,
			`SELECT a,b,typeof(a) FROM t ORDER BY a`,
		}},
		{"check-over-the-new-key", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b, CHECK(a<>4))`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=4`,
			`SELECT a,b FROM t ORDER BY a`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=3`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"generated-over-the-new-key", []string{
			`CREATE TABLE v(a INTEGER PRIMARY KEY, b, g AS (a*2))`,
			`CREATE TABLE s(a INTEGER PRIMARY KEY, b, g TEXT AS (a*2) STORED)`,
			`INSERT INTO v(a,b) VALUES(1,'x')`,
			`INSERT INTO s(a,b) VALUES(1,'x')`,
			`INSERT INTO v(a,b) VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=4`,
			`INSERT INTO s(a,b) VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=6`,
			`SELECT a,b,g,typeof(g) FROM v`,
			`SELECT a,b,g,typeof(g) FROM s`,
		}},
		{"strict-table", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT) STRICT`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=3, b=4`,
			`SELECT a,b,typeof(b) FROM t`,
			`INSERT INTO t VALUES(3,'y') ON CONFLICT(a) DO UPDATE SET a=5, b=x'01'`,
			`SELECT a,b,typeof(b) FROM t`,
		}},
		{"second-unique-constraint-violated-by-the-move", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
			`INSERT INTO t VALUES(1,'x'),(5,'z')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=9, b='w'`,
			`SELECT a,b FROM t ORDER BY a`,
			`INSERT INTO t VALUES(9,'y') ON CONFLICT(a) DO UPDATE SET a=11, b='z'`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"do-update-where-false", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=99 WHERE b='no'`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT changes()`,
		}},
		{"autoincrement-sequence-untouched", []string{
			`CREATE TABLE ai(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
			`INSERT INTO ai VALUES(1,'x')`,
			`INSERT INTO ai VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=100`,
			`SELECT a,b FROM ai ORDER BY a`,
			`SELECT name,seq FROM sqlite_sequence`,
			`INSERT INTO ai(b) VALUES('z')`,
			`SELECT a,b FROM ai ORDER BY a`,
		}},
		{"foreign-key-on-the-moved-key", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(id INTEGER PRIMARY KEY, v)`,
			`CREATE TABLE ch(id INTEGER PRIMARY KEY, pid REFERENCES p(id))`,
			`INSERT INTO p VALUES(1,'a'),(2,'b')`,
			`INSERT INTO ch VALUES(10,1)`,
			`INSERT INTO p VALUES(1,'c') ON CONFLICT(id) DO UPDATE SET id=77`,
			`SELECT id,v FROM p ORDER BY id`,
			`SELECT id,pid FROM ch ORDER BY id`,
		}},
		// The neighbouring shape the promotion deliberately did not touch: a
		// WITHOUT ROWID table has no INTEGER PRIMARY KEY, so "DO UPDATE SET
		// <pk> = ..." rewrites stored values under an unchanged row-store key.
		{"without-rowid-primary-key-reassignment", []string{
			`CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
			`INSERT INTO w VALUES('a','x'),('c','z')`,
			`INSERT INTO w VALUES('a','y') ON CONFLICT(k) DO UPDATE SET k='b'`,
			`SELECT k,v FROM w ORDER BY k`,
			`SELECT changes()`,
			`INSERT INTO w VALUES('b','q') ON CONFLICT(k) DO UPDATE SET k='c'`,
			`SELECT k,v FROM w ORDER BY k`,
			`SELECT changes()`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "upsert-rowid-"+tc.name, tc.stmts) })
	}
}

// TestReplaceVictimDeleteTriggersDiff: the REPLACE victim's DELETE triggers
// under recursive_triggers, and insert.c's re-check after them.
func TestReplaceVictimDeleteTriggersDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"after-on-a-rowid-conflict", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('a-'||old.a||'-'||old.b); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
			`SELECT changes()`,
		}},
		{"before-on-a-unique-index-conflict", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a, b UNIQUE)`,
			`CREATE TRIGGER td BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('b-'||old.a); END`,
			`INSERT INTO t VALUES('x',1)`,
			`INSERT OR REPLACE INTO t VALUES('y',1)`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		{"two-victims-two-constraints", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a UNIQUE, b UNIQUE, c)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('c-'||old.c); END`,
			`INSERT INTO t VALUES(1,10,'p')`,
			`INSERT INTO t VALUES(2,20,'q')`,
			`INSERT OR REPLACE INTO t VALUES(1,20,'r')`,
			`SELECT a,b,c FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		{"before-raise-ignore-keeps-the-victim", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td BEFORE DELETE ON t BEGIN SELECT RAISE(IGNORE); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT changes()`,
		}},
		{"before-raise-abort", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td BEFORE DELETE ON t BEGIN SELECT RAISE(ABORT,'no delete'); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"body-deletes-the-victim-itself", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td BEFORE DELETE ON t BEGIN DELETE FROM t WHERE a=old.a; END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"the-post-cascade-recheck", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO t VALUES(old.a,'resurrected'); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		// trigger.c:1137: the victim-delete program's own OE_Replace overrides
		// each body statement's clause, so the body's OR IGNORE loses.
		{"body-runs-under-replace", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE other(x UNIQUE, y)`,
			`INSERT INTO other VALUES(1,'orig')`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO other VALUES(1,'from-trigger'); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x,y FROM other ORDER BY x`,
		}},
		{"body-written-or-ignore-still-replaces", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE other(x UNIQUE, y)`,
			`INSERT INTO other VALUES(1,'orig')`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT OR IGNORE INTO other VALUES(1,'from-trigger'); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT x,y FROM other ORDER BY x`,
		}},
		{"declared-on-conflict-replace-default", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a UNIQUE ON CONFLICT REPLACE, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('t-'||old.b); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		{"multi-row-values", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('t-'||old.a); END`,
			`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`,
			`INSERT OR REPLACE INTO t VALUES(1,'X'),(4,'Y'),(2,'Z')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
			`SELECT changes()`,
		}},
		{"insert-select-source", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE src(a,b)`,
			`INSERT INTO src VALUES(1,'s1'),(5,'s5')`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('t-'||old.a); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t SELECT a,b FROM src`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		{"without-rowid-target", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('t-'||old.k||'-'||old.v); END`,
			`INSERT INTO t VALUES('a','one')`,
			`INSERT OR REPLACE INTO t VALUES('a','two')`,
			`SELECT k,v FROM t ORDER BY k`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		// The pragma OFF is the baseline: insert.c:2214-2218 leaves pTrigger
		// zero, so nothing fires and the victim is simply dropped.
		{"pragma-off-fires-nothing", []string{
			`PRAGMA recursive_triggers=0`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('t-'||old.a); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		// The pragma is read at COMPILE time here, and the write-plan cache is
		// keyed on statement TEXT -- so the SAME text either side of a flip
		// must not replay the plan compiled under the other flag.
		{"pragma-flip-between-identical-statements", []string{
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('d-'||old.a); END`,
			`PRAGMA recursive_triggers=0`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`PRAGMA recursive_triggers=1`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		{"pragma-flip-the-other-way", []string{
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('d-'||old.a); END`,
			`PRAGMA recursive_triggers=1`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`PRAGMA recursive_triggers=0`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		{"statement-unwind-undoes-the-victim-delete", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b NOT NULL)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('t-'||old.a); END`,
			`INSERT INTO t VALUES(1,'one'),(2,'two')`,
			`INSERT OR REPLACE INTO t VALUES(1,'X'),(2,NULL)`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		{"when-guard-on-the-delete-trigger", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t WHEN old.a > 5 BEGIN INSERT INTO log VALUES('t-'||old.a); END`,
			`INSERT INTO t VALUES(1,'one'),(9,'nine')`,
			`INSERT OR REPLACE INTO t VALUES(1,'X')`,
			`INSERT OR REPLACE INTO t VALUES(9,'Y')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
			`DELETE FROM t WHERE a=9`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		// RETURNING alongside the victim delete: this combination was refused
		// outright before the promotion, so the engine reported an error where
		// SQLite answers. It compiles now.
		{"returning", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('d-'||old.a||'-'||old.b); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'three') RETURNING *`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
			`INSERT OR REPLACE INTO t VALUES(1,'four'),(2,'five') RETURNING a,b`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		// An UPDATE OR REPLACE on the same table was NOT part of this
		// promotion. It is here to confirm it still agrees, since the shared
		// victim-delete machinery moved underneath it.
		{"update-or-replace-victim-delete", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			`CREATE TABLE t(a INTEGER UNIQUE, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('u-'||old.a); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT INTO t VALUES(2,'two')`,
			`UPDATE OR REPLACE t SET a=a+1 WHERE a>=1`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "replace-victim-"+tc.name, tc.stmts) })
	}
}

// TestReplaceVictimBodyCompiledOnlyWhenCoded is the round-2 half: WHICH
// statements code the victim's DELETE trigger body at all. The body here is
// broken, so the answer is visible as an error rather than only as a fired or
// unfired trigger -- SQLite refuses to PREPARE a statement whose body it coded,
// with no conflicting row present for a REPLACE ever to reach, and prepares the
// rest happily.
//
// Round 1 compiled the body on EVERY insert into such a table, which turned all
// of these into errors. The gate is replaceResolutionCoded (engine/vdbe_write.go):
// sqlite3GenerateRowDelete is reachable only from a "case OE_Replace:" arm of
// sqlite3GenerateConstraintChecks' onError switch (insert.c:2314/2339-2340 and
// insert.c:2601/2613-2615).
func TestReplaceVictimBodyCompiledOnlyWhenCoded(t *testing.T) {
	// One table, one broken DELETE trigger on it, one statement.
	with := func(schema string, tail ...string) []string {
		out := []string{
			`PRAGMA recursive_triggers=1`,
			schema,
			`CREATE TABLE u(x, y)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO u VALUES(1,2,3); END`,
		}
		return append(out, tail...)
	}
	// The INSERT ... SELECT compiler takes the same gate, and needs a source.
	src := func(stmt string) []string {
		return []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE src(p,q)`,
			`INSERT INTO src VALUES(1,'x')`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TABLE u(x, y)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO u VALUES(1,2,3); END`,
			stmt,
			`SELECT a,b FROM t`,
		}
	}
	const ipk = `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`
	const sel = `SELECT a,b FROM t`
	cases := []struct {
		name  string
		stmts []string
	}{
		{"plain-insert-codes-nothing", with(ipk, `INSERT INTO t VALUES(1,'x')`, sel)},
		{"or-ignore-codes-nothing", with(ipk, `INSERT OR IGNORE INTO t VALUES(1,'x')`, sel)},
		{"or-abort-codes-nothing", with(ipk, `INSERT OR ABORT INTO t VALUES(1,'x')`, sel)},
		{"or-replace-codes-it", with(ipk, `INSERT OR REPLACE INTO t VALUES(1,'x')`, sel)},
		// pkChng==0 (insert.c:2241/1570): the rowid arm is not emitted, and
		// there is no other constraint to emit one.
		{"or-replace-without-the-rowid-codes-nothing", with(ipk, `INSERT OR REPLACE INTO t(b) VALUES('x')`, sel)},
		{"or-replace-without-the-rowid-but-a-unique-column-codes-it",
			with(`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`, `INSERT OR REPLACE INTO t(b) VALUES('x')`, sel)},
		{"or-replace-default-values-codes-nothing", with(ipk, `INSERT OR REPLACE INTO t DEFAULT VALUES`, sel)},
		{"or-replace-with-no-constraint-anywhere-codes-nothing",
			with(`CREATE TABLE t(a, b)`, `INSERT OR REPLACE INTO t VALUES(1,'x')`, sel)},
		// The constraint's own declared clause, and the OR-clause overriding it.
		{"declared-replace-on-the-ipk-codes-it",
			with(`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b)`, `INSERT INTO t VALUES(1,'x')`, sel)},
		{"declared-replace-on-a-unique-column-codes-it",
			with(`CREATE TABLE t(a, b UNIQUE ON CONFLICT REPLACE)`, `INSERT INTO t VALUES('p',1)`, sel)},
		{"declared-replace-overridden-codes-nothing",
			with(`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b)`, `INSERT OR ABORT INTO t VALUES(1,'x')`, sel)},
		// An ON CONFLICT clause makes the covered constraint OE_Ignore/
		// OE_Update, applied AFTER overrideError (insert.c:2253-2260/:2477-2484).
		{"upsert-do-nothing-codes-nothing",
			with(`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b)`, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO NOTHING`, sel)},
		{"or-replace-plus-upsert-codes-nothing",
			with(ipk, `INSERT OR REPLACE INTO t VALUES(1,'x') ON CONFLICT(a) DO NOTHING`, sel)},

		{"select-source-plain-codes-nothing", src(`INSERT INTO t SELECT p,q FROM src`)},
		{"select-source-or-replace-codes-it", src(`INSERT OR REPLACE INTO t SELECT p,q FROM src`)},
		{"select-source-or-replace-without-the-rowid-codes-nothing", src(`INSERT OR REPLACE INTO t(b) SELECT q FROM src`)},

		// The nested case: the outer OR REPLACE codes t's body, and
		// trigger.c:1137 imposes OE_Replace on that body's own INSERT INTO v,
		// which codes v's body in turn.
		{"nested-body-under-imposed-replace", []string{
			`PRAGMA recursive_triggers=1`,
			ipk,
			`CREATE TABLE v(k INTEGER PRIMARY KEY, m)`,
			`CREATE TABLE u(x, y)`,
			`CREATE TRIGGER tv AFTER DELETE ON v BEGIN INSERT INTO u VALUES(1,2,3); END`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO v VALUES(1,'m'); END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			sel,
		}},

		// The gate reads the SCHEMA, so it moves with it. A UNIQUE index added
		// between two byte-identical statements flips "no arm coded" to
		// "coded" (insert.c:2466) -- the write-plan cache is keyed on text, and
		// schemaGen is what has to drop it.
		{"unique-index-added-between-identical-statements", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			ipk,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('d-'||old.a); END`,
			`INSERT INTO t(b) VALUES('p')`,
			`INSERT OR REPLACE INTO t(b) VALUES('p')`,
			`CREATE UNIQUE INDEX ux ON t(b)`,
			`INSERT OR REPLACE INTO t(b) VALUES('p')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		// The imposed OE_Replace reaching a SECOND-level victim, and the same
		// body reached without it firing nothing. The pair is what separates
		// "gate on the statement" from "gate on the statement's own clause".
		{"imposed-replace-reaches-a-second-level-victim", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			ipk,
			`CREATE TABLE v(k INTEGER PRIMARY KEY, m)`,
			`CREATE TRIGGER vd AFTER DELETE ON v BEGIN INSERT INTO log VALUES('v-'||old.m); END`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO v VALUES(1,'body'); END`,
			`INSERT INTO v VALUES(1,'orig')`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			sel,
			`SELECT k,m FROM v ORDER BY k`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
		{"no-imposed-replace-reaches-no-second-level-victim", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE log(x)`,
			ipk,
			`CREATE TABLE v(k INTEGER PRIMARY KEY, m)`,
			`CREATE TRIGGER vd AFTER DELETE ON v BEGIN INSERT INTO log VALUES('v-'||old.m); END`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO v VALUES(1,'body'); END`,
			`INSERT INTO v VALUES(1,'orig')`,
			`INSERT INTO t VALUES(1,'one')`,
			`DELETE FROM t`,
			`SELECT k,m FROM v ORDER BY k`,
			`SELECT x FROM log ORDER BY rowid`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "victim-body-coded-"+tc.name, tc.stmts) })
	}
}

// TestUpsertRowidCheckOverTheRowidDiff is round 2's OTHER half: a CHECK naming
// the ROWID under a DO UPDATE that MOVES the row. The scope's rowid register
// has to be the NEW one -- sqlite3UpsertDoUpdate codes the branch as a plain
// sqlite3Update (upsert.c:325-326), whose regNewData IS regNewRowid
// (update.c:1031-1032), and the CHECK block resolves a bare rowid to exactly
// that register (insert.c:2066 meets expr.c:5063-5064). Round 1 passed the OLD
// one, which silently STORED a row the oracle rejects in one direction and
// rejected one it stores in the other.
func TestUpsertRowidCheckOverTheRowidDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"moving-out-of-range", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b, CHECK(rowid < 100))`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=500`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"moving-onto-the-forbidden-value", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b, CHECK(rowid<>7))`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=7`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"the-column-equals-the-rowid", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b, CHECK(a=rowid))`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=500`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"a-rowid-alias", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b, CHECK(_rowid_ < 100))`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=500`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"not-moving-at-all", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b, CHECK(rowid < 100))`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
		{"the-where-clause-still-reads-the-old-rowid", []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'x')`,
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=500 WHERE rowid=1`,
			`SELECT a,b FROM t ORDER BY a`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "upsert-rowid-check-"+tc.name, tc.stmts) })
	}
}
