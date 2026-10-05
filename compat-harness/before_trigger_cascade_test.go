// Differential battery for trigger bodies that INSERT, UPDATE, or DELETE
// into a BEFORE-triggered table. Compares full output including error text.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// beforeCascadeCase is a script run against both engines plus the queries whose
// full output must match. A statement that fails contributes its (normalized)
// error text to the transcript, so an engine that errors where the oracle
// answers diverges here rather than silently agreeing.
type beforeCascadeCase struct {
	name   string
	script []string
	probes []string
}

var beforeCascadeCases = []beforeCascadeCase{
	{"self-insert-before", []string{
		`CREATE TABLE tbl(a,b,c)`,
		`CREATE TRIGGER tbl_trig BEFORE INSERT ON tbl BEGIN INSERT INTO tbl VALUES(new.a,new.b,new.c); END`,
		`INSERT INTO tbl VALUES(1,2,3)`,
	}, []string{`SELECT rowid,a,b,c FROM tbl ORDER BY rowid`}},

	{"after-cascade-update", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE u(x)`,
		`INSERT INTO t VALUES(5,'five')`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO u VALUES(1); END`,
		`CREATE TRIGGER ua AFTER INSERT ON u BEGIN UPDATE t SET a=6 WHERE a=5; END`,
		`INSERT INTO t(b) VALUES('new')`,
	}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT x FROM u ORDER BY 1`}},

	{"after-cascade-delete", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE u(x)`,
		`INSERT INTO t VALUES(5,'five')`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO u VALUES(1); END`,
		`CREATE TRIGGER ua AFTER INSERT ON u BEGIN DELETE FROM t WHERE a=5; END`,
		`INSERT INTO t(b) VALUES('new')`,
	}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT x FROM u ORDER BY 1`}},

	{"trigger1-18.0-reload", []string{
		`CREATE TABLE t18(a PRIMARY KEY,b,c)`,
		`INSERT INTO t18(a,b,c) VALUES(1,2,3)`,
		`CREATE TRIGGER t18r1 BEFORE UPDATE ON t18 BEGIN UPDATE t18 SET b=1000 WHERE a=old.a; END`,
		`UPDATE t18 SET c=b WHERE a=1`,
	}, []string{`SELECT a,b,c FROM t18`}},

	{"trigger1-18.1-reload", []string{
		`CREATE TABLE t18(a PRIMARY KEY,b,c)`,
		`INSERT INTO t18(a,b,c) VALUES(1,2,3)`,
		`CREATE TRIGGER t18r1 BEFORE UPDATE ON t18 BEGIN UPDATE t18 SET b=1000 WHERE a=old.a; END`,
		`UPDATE t18 SET c=b, b=b+1 WHERE a=1`,
	}, []string{`SELECT a,b,c FROM t18`}},

	{"before-update-deletes-own-row", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN DELETE FROM t WHERE a=old.a; END`,
		`CREATE TRIGGER au AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
		`UPDATE t SET b='z'`,
	}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"before-delete-writes-table", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO t VALUES(old.a+10,'n'); END`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
		`DELETE FROM t WHERE a=1`,
	}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"before-insert-explicit-rowid", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY,b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl BEGIN INSERT INTO log VALUES(new.rowid); END`,
		`INSERT INTO tbl VALUES(7,'seven')`,
		`INSERT INTO tbl VALUES(NULL,'auto')`,
	}, []string{`SELECT a,b FROM tbl ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"before-insert-check-ignore", []string{
		`CREATE TABLE t(a, b, CHECK(a>0))`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(1); END`,
		`INSERT OR IGNORE INTO t VALUES(-1,'x')`,
	}, []string{`SELECT a,b FROM t`, `SELECT m FROM log`}},

	{"before-insert-raise-ignore", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON t WHEN new.b='skip' BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t VALUES(NULL,'keep')`,
		`INSERT INTO t VALUES(NULL,'skip')`,
		`INSERT INTO t VALUES(NULL,'keep2')`,
	}, []string{`SELECT a,b FROM t ORDER BY a`}},

	{"gencol-from-ipk", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, g AS (a*10) STORED)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.g); END`,
		`INSERT INTO t(b) VALUES('x')`,
		`INSERT INTO t(b) VALUES('y')`,
	}, []string{`SELECT a,b,g FROM t ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"before-insert-nested-two-deep", []string{
		`CREATE TABLE a1(x INTEGER PRIMARY KEY, y)`,
		`CREATE TABLE b1(x INTEGER PRIMARY KEY, y)`,
		`CREATE TRIGGER ba BEFORE INSERT ON a1 BEGIN INSERT INTO b1(y) VALUES(new.y||'-b'); END`,
		`CREATE TRIGGER bb BEFORE INSERT ON b1 BEGIN INSERT INTO a1(y) VALUES(new.y||'-a'); END`,
		`INSERT INTO a1(y) VALUES('r')`,
	}, []string{`SELECT x,y FROM a1 ORDER BY x`, `SELECT x,y FROM b1 ORDER BY x`}},

	{"before-update-changes-unassigned-plus-ipk", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b, c)`,
		`INSERT INTO t VALUES(1,2,3)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=99 WHERE k=old.k; END`,
		`UPDATE t SET c=b WHERE k=1`,
	}, []string{`SELECT k,b,c FROM t`}},

	{"before-update-moves-rowid", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET k=50 WHERE k=old.k; END`,
		`CREATE TRIGGER au AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.k||'->'||new.k); END`,
		`UPDATE t SET b='y' WHERE k=1`,
	}, []string{`SELECT k,b FROM t ORDER BY k`, `SELECT m FROM log ORDER BY 1`}},

	{"cached-repeat-insert", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY,b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl BEGIN INSERT INTO log VALUES(new.rowid); END`,
		`INSERT INTO tbl(b) VALUES('one')`,
		`INSERT INTO tbl(b) VALUES('two')`,
		`INSERT INTO tbl(b) VALUES('three')`,
	}, []string{`SELECT a,b FROM tbl ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"before-insert-strict", []string{
		`CREATE TABLE t(a INT, b TEXT) STRICT`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(1); END`,
		`INSERT INTO t VALUES(1,'x')`,
	}, []string{`SELECT a,b FROM t`, `SELECT m FROM log`}},

	{"multirow-values-before", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.rowid||':'||new.b); END`,
		`INSERT INTO t VALUES(NULL,'p'),(NULL,'q'),(20,'r'),(NULL,'s')`,
	}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"body-insert-explicit-rowid", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY,b)`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl BEGIN INSERT INTO tbl VALUES(new.a+100,'cascade'); END`,
		`INSERT INTO tbl VALUES(1,'one')`,
	}, []string{`SELECT a,b FROM tbl ORDER BY a`}},

	{"body-insert-autoinc", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY AUTOINCREMENT,b)`,
		`CREATE TABLE g(n)`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl WHEN new.b<>'c' BEGIN INSERT INTO tbl(b) VALUES('c'); END`,
		`INSERT INTO tbl(b) VALUES('x')`,
		`INSERT INTO tbl(b) VALUES('y')`,
	}, []string{`SELECT a,b FROM tbl ORDER BY a`, `SELECT seq FROM sqlite_sequence WHERE name='tbl'`}},

	{"body-insert-unique-conflict", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY, u UNIQUE, b)`,
		`INSERT INTO tbl VALUES(1,10,'x')`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl WHEN new.b<>'c' BEGIN INSERT INTO tbl(u,b) VALUES(new.u,'c'); END`,
		`INSERT INTO tbl(u,b) VALUES(20,'y')`,
	}, []string{`SELECT a,u,b FROM tbl ORDER BY a`}},

	{"body-insert-check-uses-rowid", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY, b, CHECK(a IS NOT NULL))`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO tbl(b) VALUES('x')`,
	}, []string{`SELECT a,b FROM tbl ORDER BY a`, `SELECT m FROM log`}},

	{"before-insert-check-sees-rowid", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY, b, CHECK(a>0))`,
		`INSERT INTO tbl(b) VALUES('x')`,
		`INSERT INTO tbl(b) VALUES('y')`,
	}, []string{`SELECT a,b FROM tbl ORDER BY a`}},

	{"recursive-triggers-on", []string{
		`PRAGMA recursive_triggers=ON`,
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY,b)`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl WHEN new.a<3 BEGIN INSERT INTO tbl(a,b) VALUES(new.a+1,'r'); END`,
		`INSERT INTO tbl VALUES(1,'one')`,
	}, []string{`SELECT a,b FROM tbl ORDER BY a`}},

	{"body-update-before-deletes-row", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY,b,c)`,
		`INSERT INTO t VALUES(1,'x','p'),(2,'y','q')`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN DELETE FROM t WHERE k=old.k+1; END`,
		`CREATE TRIGGER au AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.k); END`,
		`UPDATE t SET b='z'`,
	}, []string{`SELECT k,b,c FROM t ORDER BY k`, `SELECT m FROM log ORDER BY 1`}},

	{"body-update-reload-multi-col", []string{
		`CREATE TABLE t(a PRIMARY KEY,b,c,d)`,
		`INSERT INTO t VALUES(1,2,3,4)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=100,d=400 WHERE a=old.a; END`,
		`UPDATE t SET c=b+d WHERE a=1`,
	}, []string{`SELECT a,b,c,d FROM t`}},

	{"body-update-reload-gencol", []string{
		`CREATE TABLE t(a PRIMARY KEY,b,c, g AS (b*1000) STORED)`,
		`INSERT INTO t(a,b,c) VALUES(1,2,3)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=7 WHERE a=old.a; END`,
		`UPDATE t SET c=99 WHERE a=1`,
	}, []string{`SELECT a,b,c,g FROM t`}},

	{"body-update-reload-check", []string{
		`CREATE TABLE t(a PRIMARY KEY,b,c, CHECK(b<50))`,
		`INSERT INTO t VALUES(1,2,3)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=999 WHERE a=old.a; END`,
		`UPDATE t SET c=9 WHERE a=1`,
	}, []string{`SELECT a,b,c FROM t`}},

	{"body-delete-before-deletes-row", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN DELETE FROM t WHERE a=old.a; END`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
		`DELETE FROM t WHERE a=1`,
	}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"body-delete-before-moves-row", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN UPDATE t SET a=a+100 WHERE a=old.a; END`,
		`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
		`DELETE FROM t WHERE a=1`,
	}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"body-insert-into-before-triggered-other", []string{
		`CREATE TABLE a1(x INTEGER PRIMARY KEY, y)`,
		`CREATE TABLE b1(x INTEGER PRIMARY KEY, y)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER ta AFTER INSERT ON a1 BEGIN INSERT INTO b1(y) VALUES(new.y); END`,
		`CREATE TRIGGER tb BEFORE INSERT ON b1 BEGIN INSERT INTO log VALUES(new.rowid); END`,
		`INSERT INTO a1(y) VALUES('p')`,
		`INSERT INTO a1(y) VALUES('q')`,
	}, []string{`SELECT x,y FROM b1 ORDER BY x`, `SELECT m FROM log ORDER BY 1`}},

	{"body-insert-raise-ignore-cascade", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`CREATE TABLE u(x)`,
		`CREATE TRIGGER ai AFTER INSERT ON u BEGIN INSERT INTO t(b) VALUES('cascade'); END`,
		`CREATE TRIGGER bi BEFORE INSERT ON t WHEN new.b='cascade' BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO u VALUES(1)`,
		`INSERT INTO t(b) VALUES('direct')`,
	}, []string{`SELECT a,b FROM t ORDER BY a`, `SELECT x FROM u`}},

	{"body-update-of-before-triggered-nochange", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`CREATE TABLE u(n)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES('b'||old.k); END`,
		`CREATE TRIGGER ai AFTER INSERT ON u BEGIN UPDATE t SET b='cascaded' WHERE k=1; END`,
		`INSERT INTO u VALUES(1)`,
	}, []string{`SELECT k,b FROM t`, `SELECT m FROM log ORDER BY 1`}},

	{"body-delete-of-before-triggered", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE u(n)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('b'||old.k); END`,
		`CREATE TRIGGER ai AFTER INSERT ON u BEGIN DELETE FROM t WHERE k=1; END`,
		`INSERT INTO u VALUES(1)`,
	}, []string{`SELECT k,b FROM t ORDER BY k`, `SELECT m FROM log ORDER BY 1`}},

	{"before-insert-cached-explicit-then-auto", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY,b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO tbl VALUES(5,'five')`,
		`INSERT INTO tbl VALUES(NULL,'auto')`,
		`INSERT INTO tbl VALUES(9,'nine')`,
		`INSERT INTO tbl VALUES(NULL,'auto2')`,
	}, []string{`SELECT a,b FROM tbl ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"before-insert-mustbeint-precedes-fire", []string{
		`CREATE TABLE tbl(a INTEGER PRIMARY KEY,b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON tbl BEGIN INSERT INTO log VALUES(1); END`,
		`INSERT INTO tbl VALUES('abc','x')`,
	}, []string{`SELECT a,b FROM tbl`, `SELECT m FROM log`}},

	{"body-update-from-before", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE m1(k, nv)`,
		`INSERT INTO t VALUES(1,'a'),(2,'b')`,
		`INSERT INTO m1 VALUES(1,'A'),(2,'B')`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.k||':'||old.v||'->'||new.v); END`,
		`UPDATE t SET v=m1.nv FROM m1 WHERE m1.k=t.k`,
	}, []string{`SELECT k,v FROM t ORDER BY k`, `SELECT x FROM log ORDER BY 1`}},

	{"before-update-strict", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.b); END`,
		`UPDATE t SET b='y'`,
	}, []string{`SELECT k,b FROM t`, `SELECT m FROM log`}},

	{"body-update-ipk-set-with-before", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`CREATE TABLE u(n)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.k||'/'||new.k); END`,
		`CREATE TRIGGER ai AFTER INSERT ON u BEGIN UPDATE t SET k=77 WHERE k=1; END`,
		`INSERT INTO u VALUES(1)`,
	}, []string{`SELECT k,b FROM t`, `SELECT m FROM log`}},

	{"gencol-from-ipk-after", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, g AS (a*10) STORED)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER ai AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.g); END`,
		`INSERT INTO t(b) VALUES('x')`,
		`INSERT INTO t(b) VALUES('y')`,
	}, []string{`SELECT a,b,g FROM t ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"gencol-from-ipk-check-notrigger", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, g AS (a*10) STORED, CHECK(g<25))`,
		`INSERT INTO t(b) VALUES('r1')`,
		`INSERT INTO t(b) VALUES('r2')`,
		`INSERT INTO t(b) VALUES('r3')`,
	}, []string{`SELECT a,b,g FROM t ORDER BY a`}},

	{"gencol-from-ipk-virtual-before", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, g AS (a*10))`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.g); END`,
		`CREATE TRIGGER ai AFTER INSERT ON t BEGIN INSERT INTO log VALUES('a'||new.g); END`,
		`INSERT INTO t(b) VALUES('x')`,
		`INSERT INTO t VALUES(30,'e')`,
	}, []string{`SELECT a,b,g FROM t ORDER BY a`, `SELECT m FROM log ORDER BY 1`}},

	{"strict-ipk-numeric-text", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT) STRICT`,
		`INSERT INTO t VALUES('12','x')`,
		`INSERT INTO t VALUES(2.0,'y')`,
	}, []string{`SELECT a,b,typeof(a) FROM t ORDER BY a`}},

	{"strict-ipk-null-auto", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT) STRICT`,
		`INSERT INTO t VALUES(NULL,'x')`,
	}, []string{`SELECT a,b,typeof(a) FROM t ORDER BY a`}},

	{"strict-gencol-from-ipk", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, g TEXT AS (a) STORED) STRICT`,
		`INSERT INTO t(b) VALUES('x')`,
	}, []string{`SELECT a,b,g FROM t ORDER BY a`}},

	// Both of these arrived in this file as beforeCascadeKnownWrong -- the two
	// PRE-EXISTING divergences the trio found and did not close -- and both
	// were closed by ONE later edit, the move of the NOT NULL loop (and the NOT
	// NULL ON CONFLICT REPLACE substitution that feeds it) down to
	// sqlite3GenerateConstraintChecks' position, past the BEFORE fire and past
	// OpNewRowid (insert.c:1961-2060, reached from :1569). They are kept as
	// parity cases rather than deleted, because each is the only witness to one
	// half of that move.
	//
	//   - "before-insert-notnull-ignore" is the CONFLICT half. C SQLite
	//     fires the BEFORE INSERT program and only then enforces NOT NULL, so
	//     under OR IGNORE the program's writes to other tables SURVIVE the
	//     skipped row. This engine used to refuse the statement outright, on
	//     compileInsertStmt's conflict-x-BEFORE crossing.
	//     Lifting that crossing IS what this case measures.
	//
	//   - "gencol-from-ipk-notnull-notrigger" is the ROWID half, and it names
	//     no trigger at all: a NOT NULL over a generated column derived from
	//     the INTEGER PRIMARY KEY used to be judged while the rowid slot was
	//     still NULL, so row 1 was REJECTED where the oracle stores it. The
	//     STRICT OpTypeCheck deliberately did NOT move with the NOT NULL loop
	//     (the C runs it ahead of the fire too -- sqlite3TableAffinity on the
	//     NEW.* image, insert.c:1487-1492 with insert.c:179-201), so
	//     TestStrictTypeErrorRowidAlias's message prefix is untouched.
	{"before-insert-notnull-ignore", []string{
		`CREATE TABLE t(a NOT NULL, b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(1); END`,
		`INSERT OR IGNORE INTO t VALUES(NULL,'x')`,
	}, []string{`SELECT a,b FROM t`, `SELECT m FROM log`}},

	{"gencol-from-ipk-notnull-notrigger", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, g AS (nullif(a,2)) STORED NOT NULL)`,
		`INSERT INTO t(b) VALUES('r1')`,
		`INSERT INTO t(b) VALUES('r2')`,
		`INSERT INTO t(b) VALUES('r3')`,
	}, []string{`SELECT a,b,g FROM t ORDER BY a`}},

	// The same NOT NULL move, probed on the two shapes it could plausibly have
	// broken and did not: the ON CONFLICT REPLACE substitution now runs BELOW
	// the OpNewRowid rather than above it, and it feeds a generated column.
	// gencol1.test 7.20/7.21 is the rule -- b derives from the SUBSTITUTED c --
	// and it holds because the substitution loop still carries a
	// ComputeGenerated behind it, which is the C's own second pass
	// (insert.c:2049-2055).
	{"notnull-replace-default-gencol", []string{
		`CREATE TABLE t1(a NOT NULL DEFAULT 'aaa', b AS(c) NOT NULL, c NOT NULL DEFAULT 'ccc')`,
		`REPLACE INTO t1(a,c) VALUES(NULL,NULL)`,
	}, []string{`SELECT a,b,c FROM t1`}},

	// The After-BEFORE-trigger-reload-loop (update.c:1002-1019), and the wrong
	// answer that showed it was missing. Every other case in this file agreed
	// without it, because the reload only becomes observable once the
	// constraint checks run BELOW the BEFORE fire -- which is where
	// compileUpdateStmt puts them now (update.c:1030). A MULTI-COLUMN CHECK is
	// the shape that can tell the two apart: the trigger's own write passes its
	// own check, and only the COMBINATION with the outer statement's new value
	// violates it, so a stale register survives every single-column probe.
	//
	// musql stored (1,40,20) -- a row that violates its own CHECK -- where the
	// oracle raises "CHECK constraint failed" and leaves (1,2,3). The CHECK was
	// evaluated on b=2, the value the registers held from before the trigger
	// ran, and opUpdateRow's setCol merge then wrote b=40 into the row anyway.
	// The passing spelling beside it is the control: it must still SUCCEED, so
	// that a reload emitted with the wrong sense fails here too.
	{"before-update-reload-multi-col-check", []string{
		`CREATE TABLE t(a PRIMARY KEY, b, c, CHECK(b + c < 50))`,
		`INSERT INTO t VALUES(1,2,3)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=40 WHERE a=old.a; END`,
		`UPDATE t SET c=20 WHERE a=1`,
	}, []string{`SELECT a,b,c FROM t`}},

	{"before-update-reload-multi-col-check-passes", []string{
		`CREATE TABLE t(a PRIMARY KEY, b, c, CHECK(b + c < 50))`,
		`INSERT INTO t VALUES(1,2,3)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=40 WHERE a=old.a; END`,
		`UPDATE t SET c=5 WHERE a=1`,
	}, []string{`SELECT a,b,c FROM t`}},

	// The NOT NULL half of the same reload, which the C runs through the same
	// sqlite3GenerateConstraintChecks call.
	{"before-update-reload-notnull", []string{
		`CREATE TABLE t(a PRIMARY KEY, b NOT NULL, c)`,
		`INSERT INTO t VALUES(1,2,3)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=NULL WHERE a=old.a; END`,
		`UPDATE t SET c=9 WHERE a=1`,
	}, []string{`SELECT a,b,c FROM t`}},

	// The same reload on the UPDATE ... FROM loop, whose cursor is driven by an
	// ephemeral table (update.c:861-864) rather than by the target's own scan,
	// so its re-seek is the KEY-REGISTER form of OpNotExists and the one this
	// block adds is the content-refresh form on top of it. Both spellings
	// matter: one where the reload decides a CHECK, one where it decides only
	// the stored value.
	{"upfrom-before-reload-check", []string{
		`CREATE TABLE t(k PRIMARY KEY, b, c, CHECK(b + c < 50))`,
		`CREATE TABLE m(k, nc)`,
		`INSERT INTO t VALUES(1,2,3)`,
		`INSERT INTO m VALUES(1,20)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=40 WHERE k=old.k; END`,
		`UPDATE t SET c=m.nc FROM m WHERE m.k=t.k`,
	}, []string{`SELECT k,b,c FROM t ORDER BY k`}},

	{"upfrom-before-reload-value", []string{
		`CREATE TABLE t(k PRIMARY KEY, b, c)`,
		`CREATE TABLE m(k, nc)`,
		`INSERT INTO t VALUES(1,2,3)`,
		`INSERT INTO m VALUES(1,20)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN UPDATE t SET b=b||'!' WHERE k=old.k; END`,
		`UPDATE t SET c=m.nc FROM m WHERE m.k=t.k`,
	}, []string{`SELECT k,b,c FROM t ORDER BY k`}},

	// The re-seek's OTHER half, on the same instruction: a BEFORE program that
	// DELETES the row must skip the whole iteration rather than reload it. This
	// is what makes OpNotExists safe to use where OpSkipIfRowGone was -- it
	// still jumps to the row end when the row is gone.
	{"before-deletes-row-then-check", []string{
		`CREATE TABLE t(k PRIMARY KEY, b, c, CHECK(b + c < 50))`,
		`INSERT INTO t VALUES(1,2,3),(2,2,3)`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN DELETE FROM t WHERE k=old.k; END`,
		`UPDATE t SET c=20`,
	}, []string{`SELECT k,b,c FROM t ORDER BY k`}},

	{"notnull-replace-default-with-before", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b NOT NULL ON CONFLICT REPLACE DEFAULT 7, g AS (a*10) STORED)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.b); END`,
		`INSERT INTO t(b) VALUES(NULL)`,
		`INSERT INTO t(b) VALUES(3)`,
	}, []string{`SELECT a,b,g FROM t ORDER BY a`, `SELECT x FROM log ORDER BY rowid`}},
}

// beforeCascadeKnownWrong: PRE-EXISTING divergences this cluster found but did
// not close, pinned so they cannot be forgotten and so that fixing one FAILS
// this test rather than passing silently.
//
// EMPTY as of the NOT NULL move. It had two entries, both recorded above where
// they now sit as parity cases; the list is kept so the next divergence this
// battery finds has somewhere to go that is not "delete the case".
var beforeCascadeKnownWrong = []beforeCascadeCase{}

func bcRunMusql(t *testing.T, c beforeCascadeCase) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "before_cascade.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	var out []string
	for _, s := range c.script {
		if err := db.Exec(s); err != nil {
			out = append(out, "ERR("+s+"): "+bcNormErr(err))
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p, perr := engine.Open(path)
	if perr != nil {
		t.Fatalf("engine.Open: %v", perr)
	}
	defer p.Close()
	for _, q := range c.probes {
		_, rows, err := p.Query(q)
		if err != nil {
			out = append(out, "QERR: "+bcNormErr(err))
			continue
		}
		var sb strings.Builder
		for _, r := range rows {
			for i, v := range r {
				if i > 0 {
					sb.WriteString("|")
				}
				sb.WriteString(bcRenderEngineValue(v))
			}
			sb.WriteString(";")
		}
		out = append(out, sb.String())
	}
	return out
}

func bcRunOracle(t *testing.T, c beforeCascadeCase) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "before_cascade_oracle.sqlite")
	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	var out []string
	for _, s := range c.script {
		if _, err := db.Exec(s); err != nil {
			out = append(out, "ERR("+s+"): "+bcNormErr(err))
		}
	}
	for _, q := range c.probes {
		rows, err := db.Query(q)
		if err != nil {
			out = append(out, "QERR: "+bcNormErr(err))
			continue
		}
		cols, _ := rows.Columns()
		var sb strings.Builder
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			for i, v := range vals {
				if i > 0 {
					sb.WriteString("|")
				}
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				sb.WriteString(fmt.Sprintf("%v", v))
			}
			sb.WriteString(";")
		}
		rows.Close()
		out = append(out, sb.String())
	}
	return out
}

// bcNormErr strips the engine's own message furniture so the two engines'
// error TEXT is comparable: musql prefixes "engine: " and then names the
// statement ("INSERT into t: ..."), where the oracle reports the bare SQLite
// message. Everything after that prefix -- which constraint, which column --
// still has to match exactly, and that is the part this file is here to
// compare.
func bcNormErr(err error) string {
	m := strings.TrimPrefix(err.Error(), "engine: ")
	if i := strings.Index(m, ": "); i >= 0 &&
		(strings.HasPrefix(m, "INSERT into ") || strings.HasPrefix(m, "UPDATE ") || strings.HasPrefix(m, "DELETE from ")) {
		m = m[i+2:]
	}
	return m
}

func bcRenderEngineValue(v engine.Value) string {
	switch v.Typ {
	case engine.Null:
		return "<nil>"
	case engine.Int:
		return fmt.Sprintf("%d", v.I)
	case engine.Float:
		return fmt.Sprintf("%v", v.F)
	default:
		return string(v.S)
	}
}

func TestBeforeTriggerCascadeParity(t *testing.T) {
	for _, c := range beforeCascadeCases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(bcRunMusql(t, c), " ## ")
			want := strings.Join(bcRunOracle(t, c), " ## ")
			if got != want {
				t.Errorf("DIVERGES\n  musql: %s\n  oracle: %s", got, want)
			}
		})
	}
}

// TestBeforeTriggerCascadeKnownWrong asserts the tracked backlog is still
// exactly as tracked. A case that starts AGREEING fails here on purpose: move
// it up into beforeCascadeCases and say what fixed it.
func TestBeforeTriggerCascadeKnownWrong(t *testing.T) {
	for _, c := range beforeCascadeKnownWrong {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(bcRunMusql(t, c), " ## ")
			want := strings.Join(bcRunOracle(t, c), " ## ")
			if got == want {
				t.Errorf("%s now AGREES with the oracle (%s).\n"+
					"Move it into beforeCascadeCases and record what closed it.", c.name, got)
			}
		})
	}
}
