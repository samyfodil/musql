// Tests INSERT row count behavior distinct from sqlite3_changes().
// sqlite3GenerateConstraintChecks never reaches it: OE_Update calls
// sqlite3UpsertDoUpdate and then falls through to OE_Ignore's
// "sqlite3VdbeGoto(v, ignoreDest)" (insert.c:2362 and 2589), whose ignoreDest is
// endOfLoop. sqlite3Update's own count is suppressed for an upsert too
// (update.c:1149's guard carries pUpsert==0). So a row that took DO UPDATE moves
// changes() and moves nothing else.
//
// Round 32 measured that one shape and left it open because nothing outside the
// engine could tell the branches apart. This battery is what the fix is measured
// on, and it deliberately spreads across the upsert shapes that reach the count
// by DIFFERENT routes: the plain OpUpsertStore case alongside a STRICT table, a
// triggered table, an INSERT ... SELECT source and a non-rowid UNIQUE index.
// They count through different code, which is exactly the split a battery built
// on one shape cannot see.
package compat

import "testing"

// r33qCountRows wraps differ for scripts whose whole point is the count ROW a
// DML answers under the flag: every statement's result is compared, so both the
// row's VALUE and its column NAME ("rows inserted"/"rows updated"/"rows
// deleted", sqlite3CodeChangeCount's zColName) are pinned.
func r33qCountRows(t *testing.T, name string, stmts ...string) {
	t.Helper()
	differ(t, name, stmts)
}

// TestR33QCountChangesUpsertExecutors runs the same upsert count semantics
// across shapes that reach it differently. Each block names what makes it
// different from the plain one, because that difference is the whole point of
// having more than one block.
func TestR33QCountChangesUpsertExecutors(t *testing.T) {
	// Compiled: a plain rowid table with no trigger and no exotic index emits
	// OpUpsertFind/OpUpsertStore/OpInsert.
	r33qCountRows(t, "r33q upsert compiled",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'x'),(2,'y')`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'p') ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
		`INSERT INTO t1 VALUES(3,'q') ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
		`INSERT INTO t1 VALUES(1,'r'),(4,'s'),(2,'t') ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
		`INSERT INTO t1 VALUES(1,'u') ON CONFLICT DO NOTHING`,
		`SELECT a,b FROM t1 ORDER BY a`,
	)
	// A STRICT table: the per-row declared-type check (OpTypeCheck,
	// engine/vdbe_write.go) runs inside the same statement as the upsert.
	r33qCountRows(t, "r33q upsert strict table",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT) STRICT`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`,
		`INSERT INTO t1 VALUES(2,'m') ON CONFLICT(a) DO UPDATE SET b='n'`,
		`SELECT a,b FROM t1 ORDER BY a`,
	)
	// An upsert combined with a TRIGGER: the AFTER fire is emitted
	// unconditionally, so a row the upsert SKIPS must not still fire it.
	r33qCountRows(t, "r33q upsert triggered table",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(v)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`,
		`INSERT INTO t1 VALUES(2,'m') ON CONFLICT(a) DO UPDATE SET b='n'`,
		`SELECT count(*) FROM log`,
		`SELECT a,b FROM t1 ORDER BY a`,
	)
	// An upsert whose source is an INSERT ... SELECT rather than VALUES.
	// The "WHERE true" is REQUIRED, and not by this engine -- SQLite's own
	// grammar cannot tell "ON CONFLICT" as an upsert clause from "ON" as a join
	// constraint after a bare SELECT, so it rejects the WHERE-less spelling
	// outright -- which, since round 34, this engine does too (the leading FROM
	// element's ON/USING slot, engine/sql_parser.go; the whole matrix is
	// compat-harness/upsert_r34x_grammar_test.go).
	r33qCountRows(t, "r33q upsert insert select source",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE src(a, b)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`INSERT INTO src VALUES(1,'p'),(2,'q'),(3,'r')`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 SELECT a,b FROM src WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
		`SELECT a,b FROM t1 ORDER BY a`,
	)
	// A UNIQUE index that is not the rowid: the upsert targets a secondary
	// constraint, which is the other arm of sqlite3GenerateConstraintChecks
	// (insert.c:2589's OE_Update, the per-index one).
	r33qCountRows(t, "r33q upsert unique index target",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, k TEXT UNIQUE, v)`,
		`INSERT INTO t1 VALUES(1,'kk','x')`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1(k,v) VALUES('kk','y') ON CONFLICT(k) DO UPDATE SET v='z'`,
		`INSERT INTO t1(k,v) VALUES('mm','w') ON CONFLICT(k) DO UPDATE SET v='q'`,
		`SELECT a,k,v FROM t1 ORDER BY a`,
	)
	// WITHOUT ROWID has no regRowid to complete an insertion into, and its
	// upsert takes the primary-key index arm.
	r33qCountRows(t, "r33q upsert without rowid",
		`CREATE TABLE t1(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO t1 VALUES('a',1)`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES('a',2) ON CONFLICT(k) DO UPDATE SET v=3`,
		`INSERT INTO t1 VALUES('b',4) ON CONFLICT(k) DO UPDATE SET v=5`,
		`SELECT k,v FROM t1 ORDER BY k`,
	)
}

// TestR33QCountChangesNonUpsert is the regression half: every INSERT shape whose
// count is NOT the upsert one still has to report exactly what it reported
// before the engine started keeping regRowCount separately. OR IGNORE and OR
// REPLACE are the interesting ones, because IGNORE also jumps to endOfLoop --
// it is the very branch OE_Update falls into -- while REPLACE deletes and then
// really inserts, so it DOES count.
func TestR33QCountChangesNonUpsert(t *testing.T) {
	r33qCountRows(t, "r33q or-clauses",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`PRAGMA count_changes=1`,
		`INSERT OR IGNORE INTO t1 VALUES(1,'y')`,
		`INSERT OR IGNORE INTO t1 VALUES(1,'y'),(2,'z'),(1,'w')`,
		`INSERT OR REPLACE INTO t1 VALUES(1,'v')`,
		`REPLACE INTO t1 VALUES(2,'u')`,
		`INSERT INTO t1 DEFAULT VALUES`,
		`SELECT a,b FROM t1 ORDER BY a`,
	)
	r33qCountRows(t, "r33q insert select and default",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE t2(a, b)`,
		`INSERT INTO t2 VALUES(1,'x'),(2,'y'),(3,'z')`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 SELECT * FROM t2`,
		`INSERT INTO t1(b) SELECT b FROM t2 WHERE 0`,
		`SELECT count(*) FROM t1`,
	)
	// A trigger body's own INSERT has no regRowCount of its own (the C codes one
	// only for pParse->pTriggerTab==0), so the firing statement must report only
	// its OWN rows -- including when the body statement is itself an upsert,
	// which is the one way a body could otherwise leak into the outer count.
	r33qCountRows(t, "r33q trigger body upserts",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE agg(k INTEGER PRIMARY KEY, n)`,
		`INSERT INTO agg VALUES(1,0)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN
		   INSERT INTO agg VALUES(1,1) ON CONFLICT(k) DO UPDATE SET n=n+1;
		 END`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`INSERT INTO t1 VALUES(2,'y'),(3,'z')`,
		`SELECT k,n FROM agg`,
		`SELECT count(*) FROM t1`,
	)
	// ...and the mirror: an upsert whose own table carries the trigger, so the
	// DO UPDATE branch runs the table's UPDATE trigger.
	r33qCountRows(t, "r33q upsert fires update trigger",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(v)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`CREATE TRIGGER tu AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES(new.b); END`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`,
		`SELECT v FROM log`,
		`SELECT a,b FROM t1 ORDER BY a`,
	)
	// The count survives a transaction and a rollback exactly as changes() does:
	// it is the STATEMENT's own count, published when the statement halts.
	r33qCountRows(t, "r33q upsert in a transaction",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`PRAGMA count_changes=1`,
		`BEGIN`,
		`INSERT INTO t1 VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`,
		`INSERT INTO t1 VALUES(2,'m') ON CONFLICT(a) DO UPDATE SET b='n'`,
		`ROLLBACK`,
		`INSERT INTO t1 VALUES(1,'q') ON CONFLICT(a) DO UPDATE SET b='r'`,
		`SELECT a,b FROM t1 ORDER BY a`,
	)
	// Turning the flag back off must stop the row, upsert or not -- the setter
	// is the same PragTyp_FLAG bit in both directions.
	r33qCountRows(t, "r33q upsert flag off again",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`,
		`PRAGMA count_changes=0`,
		`INSERT INTO t1 VALUES(1,'p') ON CONFLICT(a) DO UPDATE SET b='q'`,
		`PRAGMA count_changes`,
		`SELECT a,b FROM t1 ORDER BY a`,
	)
	// changes() itself must NOT move to the new counter: it still counts the DO
	// UPDATE. Read through SQL rather than through the driver's RowsAffected, so
	// the two are pinned independently.
	r33qCountRows(t, "r33q changes() still counts the DO UPDATE",
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`INSERT INTO t1 VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`,
		`SELECT changes(), total_changes()`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'p') ON CONFLICT(a) DO UPDATE SET b='q'`,
		`SELECT changes()`,
	)
}

// TestR33QUpsertSelectNeedsWhere lived here: the LIVE wrong answer this battery
// turned up on the way past, an upsert over a WHERE-less SELECT source that this
// engine accepted and really applied where SQLite rejects it at prepare time.
// It was an inverted tracker, and round 34 closed it, so it did what it was
// written to do and asked to be deleted.
//
// Its subject moved to compat-harness/upsert_r34x_grammar_test.go, which asserts
// the oracle's own verdict over both sides of the grammar line rather than the
// single WHERE-less shape.
