package compat

// This file tests conflict clauses with triggers through the VDBE compiler.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// musqlDriverName is driver's registered database/sql name.
var musqlDriverName = driver.DriverName

// conflictTriggerCases are INSERT/UPDATE statements with conflict clauses and triggers.
var conflictTriggerCases = []struct {
	name   string
	nSetup int
	stmts  []string
}{
	{"declared-ignore-after-only", 3, []string{
		`CREATE TABLE t(a INTEGER UNIQUE ON CONFLICT IGNORE, b TEXT)`,
		`CREATE TABLE log(tag,x,y)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`,
		`INSERT INTO t VALUES(1,'dup'),(2,'two')`,
		`SELECT tag,x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"declared-ignore-before-and-after", 3, []string{
		`CREATE TABLE t(a INTEGER UNIQUE ON CONFLICT IGNORE, b TEXT)`,
		`CREATE TABLE log(tag,x,y)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a,new.b); END`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`,
		`INSERT INTO t VALUES(1,'dup'),(2,'two')`,
		`SELECT tag,x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// A BEFORE program that the conflict clause does NOT cross: a CHECK
	// violation. Both write paths run the CHECK block after the BEFORE program
	// on the INSERT side (checkTableChecks, insert_write.go), which is where
	// insert.c runs it, so this one is ordinary parity and rides in this list.
	{"before-trigger-vs-check-fail", 4, []string{
		`CREATE TABLE t(a, b, CHECK(a<10))`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR FAIL INTO t VALUES(2,'a'),(20,'b'),(3,'c')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- INSERT, explicit OR-clause, every action, AFTER trigger ----
	{"ins-or-ignore-unique", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two'),(1,'dup2'),(3,'three')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-or-replace-unique", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one'),(2,'two')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR REPLACE INTO t VALUES(1,'dup'),(3,'three')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"replace-into-spelling", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one'),(2,'two')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`REPLACE INTO t VALUES(1,'dup'),(3,'three')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-fail-partial", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(9,'nine')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR FAIL INTO t VALUES(1,'a'),(9,'dup'),(3,'c')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-or-abort-partial", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(9,'nine')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR ABORT INTO t VALUES(1,'a'),(9,'dup'),(3,'c')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-rollback-in-txn", 4, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(9,'nine')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`BEGIN`,
		`INSERT INTO t VALUES(5,'five')`,
		`INSERT OR ROLLBACK INTO t VALUES(9,'dup')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- pre-store constraint skips (emitInsertRowBody's own IGNORE jumps) ----
	{"ins-or-ignore-notnull", 3, []string{
		`CREATE TABLE t(a,b NOT NULL)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(0,'z')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR IGNORE INTO t VALUES(1,'a'),(2,NULL),(3,'c')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"declared-notnull-ignore", 3, []string{
		`CREATE TABLE t(a, b NOT NULL ON CONFLICT IGNORE)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(0,'z')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO t VALUES(1,'a'),(2,NULL),(3,'c')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"declared-notnull-replace-default", 3, []string{
		`CREATE TABLE t(a, b NOT NULL ON CONFLICT REPLACE DEFAULT 'dd')`,
		`CREATE TABLE log(x,y)`,
		`INSERT INTO t VALUES(0,'z')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a,new.b); END`,
		`INSERT INTO t VALUES(1,'a'),(2,NULL)`,
		`SELECT x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-ignore-check", 3, []string{
		`CREATE TABLE t(a,b, CHECK(a<10))`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(0,'z')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR IGNORE INTO t VALUES(1,'a'),(20,'b'),(3,'c')`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- REPLACE's implicit victim delete meeting DELETE triggers ----
	{"ins-or-replace-victim-delete-triggers-off", 4, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I',new.a); END`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('D',old.a); END`,
		`INSERT OR REPLACE INTO t VALUES(1,'dup')`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-replace-victim-delete-triggers-on", 5, []string{
		`PRAGMA recursive_triggers=ON`,
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I',new.a); END`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('D',old.a); END`,
		`INSERT OR REPLACE INTO t VALUES(1,'dup')`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- FOREIGN KEYS crossed with the conflict clause and the trigger ----
	// sqlite3FkCheck runs from inside the same insertion loop the trigger calls
	// bracket (insert.c:1572-1573), and a REPLACE victim's ON DELETE CASCADE
	// runs from sqlite3GenerateRowDelete inside the constraint checks
	// (insert.c:2339 / :2613) -- so an FK failure and an IGNORE-skipped row
	// interleave with the AFTER fire exactly the way a UNIQUE conflict does.
	{"fk-child-or-ignore-with-trigger", 5, []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(a UNIQUE, k REFERENCES p(k))`,
		`CREATE TABLE log(x)`,
		`INSERT INTO p VALUES(1)`,
		`CREATE TRIGGER ta AFTER INSERT ON c BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR IGNORE INTO c VALUES(1,1),(2,99),(3,1)`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,k FROM c ORDER BY rowid`,
	}},
	{"fk-replace-cascade-with-trigger", 6, []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v UNIQUE)`,
		`CREATE TABLE c(id, k REFERENCES p(k) ON DELETE CASCADE)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO p VALUES(1,'one')`,
		`INSERT INTO c VALUES(10,1)`,
		`CREATE TRIGGER ta AFTER INSERT ON p BEGIN INSERT INTO log VALUES('I',new.k); END`,
		`INSERT OR REPLACE INTO p VALUES(2,'one')`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT k,v FROM p ORDER BY rowid`,
		`SELECT id,k FROM c ORDER BY rowid`,
	}},

	// ---- trigger.c:1137: the FIRING statement's clause governs the body ----
	// The body says REPLACE. Under a bare INSERT (OE_Default) it keeps it;
	// under INSERT OR IGNORE the body runs as IGNORE instead, so u keeps its
	// ORIGINAL row.
	{"body-orconf-default-keeps-its-own", 4, []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE u(a UNIQUE,tag)`,
		`INSERT INTO u VALUES(1,'orig')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT OR REPLACE INTO u VALUES(new.a,'body'); END`,
		`INSERT INTO t VALUES(1)`,
		`SELECT a,tag FROM u ORDER BY rowid`,
	}},
	{"body-orconf-overridden-by-firing-ignore", 4, []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE u(a UNIQUE,tag)`,
		`INSERT INTO u VALUES(1,'orig')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT OR REPLACE INTO u VALUES(new.a,'body'); END`,
		`INSERT OR IGNORE INTO t VALUES(1)`,
		`SELECT a,tag FROM u ORDER BY rowid`,
	}},
	// The mirror: the body has no clause of its own, and the firing statement
	// hands it REPLACE.
	{"body-inherits-firing-replace", 4, []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE u(a UNIQUE,tag)`,
		`INSERT INTO u VALUES(1,'orig')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,'body'); END`,
		`INSERT OR REPLACE INTO t VALUES(1)`,
		`SELECT a,tag FROM u ORDER BY rowid`,
	}},
	{"body-inherits-firing-ignore", 4, []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE u(a UNIQUE,tag)`,
		`INSERT INTO u VALUES(1,'orig')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,'body'); END`,
		`INSERT OR IGNORE INTO t VALUES(1)`,
		`SELECT a,tag FROM u ORDER BY rowid`,
	}},
	// The same rule for a body UPDATE, reached from an UPDATE.
	{"body-update-inherits-firing-ignore", 5, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE u(a UNIQUE,tag)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO u VALUES(1,'one'),(2,'two')`,
		`CREATE TRIGGER tr AFTER UPDATE ON t BEGIN UPDATE u SET a=2 WHERE a=1; END`,
		`UPDATE OR IGNORE t SET b='y'`,
		`SELECT a,tag FROM u ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- DEFAULT VALUES into a triggered table ----
	{"default-values-after-trigger", 2, []string{
		`CREATE TABLE t(a DEFAULT 7, b DEFAULT 'dd', c)`,
		`CREATE TABLE log(x,y,z,r)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a,new.b,new.c,new.rowid); END`,
		`INSERT INTO t DEFAULT VALUES`,
		`SELECT x,y,z,r FROM log ORDER BY rowid`,
		`SELECT a,b,c FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"default-values-before-and-after", 2, []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b DEFAULT 'dd')`,
		`CREATE TABLE log(tag,x,y)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a,new.b); END`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`,
		`INSERT INTO t DEFAULT VALUES`,
		`SELECT tag,x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"default-values-notnull-abort", 2, []string{
		`CREATE TABLE t(a NOT NULL, b DEFAULT 1)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO t DEFAULT VALUES`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT count(*) FROM t`,
	}},

	// ---- UPDATE OF <col-list> gating ----
	{"update-of-fires-on-overlap", 3, []string{
		`CREATE TABLE t(a,b,c)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,2,3)`,
		`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES('a',new.a); END`,
		`UPDATE t SET a=10`,
		`SELECT tag,x FROM log ORDER BY rowid`,
	}},
	{"update-of-silent-without-overlap", 3, []string{
		`CREATE TABLE t(a,b,c)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,2,3)`,
		`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES('a',new.a); END`,
		`UPDATE t SET b=20`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT a,b,c FROM t ORDER BY rowid`,
	}},
	{"update-of-multi-column-list", 3, []string{
		`CREATE TABLE t(a,b,c)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,2,3)`,
		`CREATE TRIGGER tu AFTER UPDATE OF a,c ON t BEGIN INSERT INTO log VALUES('ac',new.c); END`,
		`UPDATE t SET b=20`,
		`UPDATE t SET c=30`,
		`UPDATE t SET b=21,a=11`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT a,b,c FROM t ORDER BY rowid`,
	}},
	{"update-of-case-insensitive", 3, []string{
		`CREATE TABLE t(Abc,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,2)`,
		`CREATE TRIGGER tu AFTER UPDATE OF aBC ON t BEGIN INSERT INTO log VALUES(new.Abc); END`,
		`UPDATE t SET ABc=9`,
		`SELECT x FROM log ORDER BY rowid`,
	}},
	{"update-of-before-timing", 3, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,2)`,
		`CREATE TRIGGER tu BEFORE UPDATE OF a ON t BEGIN INSERT INTO log VALUES('B',old.a); END`,
		`UPDATE t SET b=20`,
		`UPDATE t SET a=10`,
		`SELECT tag,x FROM log ORDER BY rowid`,
	}},
	{"update-of-mixed-with-plain-trigger", 3, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,2)`,
		`CREATE TRIGGER tp AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('plain',new.b); END`,
		`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES('ofa',new.a); END`,
		`UPDATE t SET b=20`,
		`UPDATE t SET a=10`,
		`SELECT tag,x FROM log ORDER BY rowid`,
	}},
	{"update-of-raise-ignore", 3, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,2),(3,4)`,
		`CREATE TRIGGER tu BEFORE UPDATE OF a ON t BEGIN SELECT RAISE(IGNORE); END`,
		`UPDATE t SET a=a+100`,
		`UPDATE t SET b=b+100`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT x FROM log ORDER BY rowid`,
	}},

	// ---- UPDATE, conflict clause, AFTER trigger ----
	{"upd-or-ignore-after-trigger", 3, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`CREATE TABLE log(x,y)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.b,new.b); END`,
		`UPDATE OR IGNORE t SET b=b+10`,
		`SELECT x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-replace-after-trigger", 3, []string{
		`CREATE TABLE t(a,b UNIQUE)`,
		`CREATE TABLE log(x,y)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.b,new.b); END`,
		`UPDATE OR REPLACE t SET b=b+10`,
		`SELECT x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-fail-after-trigger", 3, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`CREATE TABLE log(x,y)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.b,new.b); END`,
		`UPDATE OR FAIL t SET b=b+10`,
		`SELECT x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-declared-ignore-after-trigger", 3, []string{
		`CREATE TABLE t(a, b INTEGER UNIQUE ON CONFLICT IGNORE)`,
		`CREATE TABLE log(x,y)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.b,new.b); END`,
		`UPDATE t SET b=b+10`,
		`SELECT x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-ignore-notnull-after-trigger", 3, []string{
		`CREATE TABLE t(a,b NOT NULL)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`UPDATE OR IGNORE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-ignore-check-after-trigger", 3, []string{
		`CREATE TABLE t(a,b, CHECK(b<10))`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,1),(2,9),(3,3)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`,
		`UPDATE OR IGNORE t SET b=b+1`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-ignore-with-returning", 3, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`,
		`UPDATE OR IGNORE t SET b=b+10 RETURNING a,b`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-ignore-with-returning", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two') RETURNING a,b`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- UPSERT beside this table's own INSERT triggers ----
	// The rule being pinned is that neither upsert branch fires an AFTER
	// INSERT trigger: DO NOTHING leaves through ignoreDest and DO UPDATE
	// through sqlite3UpsertDoUpdate's own sqlite3Update (upsert.c:325-326),
	// and insert.c's AFTER call at :1604-1607 sits below both. The BEFORE
	// program fires for the candidate either way (insert.c:1494-1496).
	{"upsert-do-nothing-conflicting", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`,
		`INSERT INTO t VALUES(1,'dup'),(2,'two') ON CONFLICT(a) DO NOTHING`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upsert-do-nothing-before-and-after", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(tag,x,y)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a,new.b); END`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`,
		`INSERT INTO t VALUES(1,'dup'),(2,'two') ON CONFLICT(a) DO NOTHING`,
		`SELECT tag,x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upsert-do-update-conflicting", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(tag,x,y)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a,new.b); END`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`,
		`INSERT INTO t VALUES(1,'dup'),(2,'two') ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
		`SELECT tag,x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upsert-do-update-where-false", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`,
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE 0`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upsert-do-update-moves-ipk", 3, []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(tag,x,y)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`,
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET a=7`,
		`SELECT tag,x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upsert-bare-target-with-trigger", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`,
		`INSERT INTO t VALUES(1,'dup'),(3,'three') ON CONFLICT DO NOTHING`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upsert-over-select-source", 4, []string{
		`CREATE TABLE t(a UNIQUE,b,c)`,
		`CREATE TABLE s(a,b)`,
		`CREATE TABLE log(tag,x)`,
		`INSERT INTO t(a,b) VALUES(1,'one')`,
		`INSERT INTO s VALUES(1,'first'),(1,'second'),(2,'other')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`,
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=coalesce(c,0)+1`,
		`SELECT tag,x FROM log ORDER BY rowid`,
		`SELECT a,b,c FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upsert-body-raise-ignore", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t VALUES(1,'dup'),(2,'two') ON CONFLICT(a) DO UPDATE SET b='upd'`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},

	// ---- an AFTER program's RAISE(IGNORE) under a conflict clause ----
	{"ins-or-ignore-after-raise-ignore", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two')`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},

	// ---- INSERT ... SELECT crossed with a conflict clause ----
	{"ins-select-or-ignore", 4, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE s(a,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO s VALUES(1,'dup'),(2,'two'),(3,'three')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT OR IGNORE INTO t SELECT a,b FROM s`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-select-declared-ignore", 4, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`,
		`CREATE TABLE s(a,b)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO s VALUES(1,'dup'),(2,'two')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO t SELECT a,b FROM s`,
		`SELECT x FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- INTEGER PRIMARY KEY rowid collisions under a conflict clause ----
	{"ins-or-replace-ipk", 3, []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(tag,x,y)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I',new.a,new.b); END`,
		`INSERT OR REPLACE INTO t VALUES(1,'dup'),(2,'two')`,
		`SELECT tag,x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-ignore-ipk", 3, []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(tag,x,y)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I',new.a,new.b); END`,
		`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two')`,
		`SELECT tag,x,y FROM log ORDER BY rowid`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- WITHOUT ROWID under a conflict clause and a trigger ----
	{"ins-or-replace-without-rowid", 3, []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`CREATE TABLE log(x,y)`,
		`INSERT INTO t VALUES('a',1)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.k,new.v); END`,
		`INSERT OR REPLACE INTO t VALUES('a',9),('b',2)`,
		`SELECT x,y FROM log ORDER BY rowid`,
		`SELECT k,v FROM t ORDER BY k`,
	}},
}

func TestConflictTriggerCodegenParity(t *testing.T) {
	for _, tc := range conflictTriggerCases {
		t.Run(tc.name, func(t *testing.T) {
			differExecOnce(t, tc.name, tc.nSetup, tc.stmts)
		})
	}
}

// beforeOrderProbe is one conflict-aware statement whose target also carries a
// BEFORE program, and whose PRE-store constraint (NOT NULL, or CHECK on the
// UPDATE side) resolves to IGNORE. The probe reads the trigger's own log, which
// is the whole observable: C SQLite fires the BEFORE program FIRST and
// verifies constraints after -- insert.c:1494-1496 vs :1569-1571, and update.c
// is explicit about it ("Fire any BEFORE UPDATE triggers. This happens before
// constraints are verified. One could argue that this is wrong.",
// update.c:978-980) -- so the writes of a program that ran for a row the IGNORE
// then skipped SURVIVE on the oracle.
type beforeOrderProbe struct {
	name  string
	setup []string
	stmt  string
	probe string
}

// beforeOrderPromoted: every case that used to DECLINE here, and now compiles.
// All of them declined at the conflict-x-BEFORE crossings in compileInsertStmt
// and compileUpdateStmt -- and all of them now answer, because both emitters put their
// constraint checks at sqlite3GenerateConstraintChecks' position rather than
// above the fire. The compile-level half is
// engine/before_trigger_order_codegen_test.go; this is the ANSWER half, and it
// is the only one that can say the surviving log rows are the RIGHT ones.
//
// This table was TestBeforeTriggerConstraintOrderDeclines, which asserted the
// opposite. It is worth saying that in the file rather than in a commit
// message: the decline it pinned was correct at the time and named the exact
// mechanism, and the mechanism is what got built.
var beforeOrderPromoted = []beforeOrderProbe{
	{"insert-notnull-ignore",
		[]string{`CREATE TABLE t(a UNIQUE, b NOT NULL)`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'one')`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t VALUES(2,NULL),(3,'c')`,
		`SELECT x FROM log ORDER BY rowid`},
	{"insert-declared-notnull-ignore",
		[]string{`CREATE TABLE t(a, b NOT NULL ON CONFLICT IGNORE)`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'one')`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(2,NULL),(3,'c')`,
		`SELECT x FROM log ORDER BY rowid`},

	// The same two through a SELECT row source, which is a different emitter
	// above the row body (compileInsertSelectWrite) and which fired its BEFORE
	// program from the tail -- BELOW the checks -- until the row body took it
	// over. insert.c puts both fires inside its ONE insertion loop whichever
	// template filled the register block (:1495, :1604-1608, endOfLoop :1613).
	{"insert-select-notnull-ignore",
		[]string{`CREATE TABLE t(a UNIQUE, b NOT NULL)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO s VALUES(2,NULL),(3,'c')`,
			`INSERT INTO t VALUES(1,'one')`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t SELECT a,b FROM s`,
		`SELECT x FROM log ORDER BY rowid`},
	{"insert-select-declared-notnull-ignore",
		[]string{`CREATE TABLE t(a, b NOT NULL ON CONFLICT IGNORE)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
			`INSERT INTO s VALUES(2,NULL),(3,'c')`,
			`INSERT INTO t VALUES(1,'one')`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a,b FROM s`,
		`SELECT x FROM log ORDER BY rowid`},

	// The UPDATE half, whose emitter is compileUpdateStmt's own row body.
	{"update-notnull-ignore",
		[]string{`CREATE TABLE t(a,b NOT NULL)`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
			`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE OR IGNORE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`,
		`SELECT x FROM log ORDER BY rowid`},
	{"update-check-ignore",
		[]string{`CREATE TABLE t(a,b, CHECK(b<10))`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,1),(2,9),(3,3)`,
			`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE OR IGNORE t SET b=b+1`,
		`SELECT x FROM log ORDER BY rowid`},
	{"update-declared-notnull-ignore",
		[]string{`CREATE TABLE t(a, b NOT NULL ON CONFLICT IGNORE)`, `CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q')`,
			`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`,
		`SELECT x FROM log ORDER BY rowid`},

	// The STRICT straddle, whose ERROR TEXT is the observable rather than a log
	// row: with a BEFORE program the datatype check runs ahead of NOT NULL
	// (update.c:981's sqlite3TableAffinity inside the BEFORE arm), so the
	// statement reports the datatype violation. Both engines must fail here and
	// the probe must see the same untouched table.
	{"update-strict-before-typecheck-first",
		[]string{`CREATE TABLE u(i INT, t TEXT, n INT NOT NULL) STRICT`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1,'x',1)`,
			`CREATE TRIGGER tb BEFORE UPDATE ON u BEGIN INSERT INTO log VALUES(old.i); END`},
		`UPDATE u SET n=NULL, i='abc'`,
		`SELECT i,t,n FROM u`},
	{"update-strict-after-notnull-first",
		[]string{`CREATE TABLE u(i INT, t TEXT, n INT NOT NULL) STRICT`, `CREATE TABLE log(x)`,
			`INSERT INTO u VALUES(1,'x',1)`,
			`CREATE TRIGGER ta AFTER UPDATE ON u BEGIN INSERT INTO log VALUES(old.i); END`},
		`UPDATE u SET n=NULL, i='abc'`,
		`SELECT i,t,n FROM u`},
}

// TestBeforeConstraintOrderPromoted is a PARITY gate, not a permissive one.
// Each case must reach the SAME outcome as the oracle -- the same error text
// where both fail, the same probe rows either way. A decline where the oracle
// answers fails it, which is the point: it is what would notice either
// crossing's guard coming back, or a constraint block drifting above its fire.
func TestBeforeConstraintOrderPromoted(t *testing.T) {
	for _, tc := range beforeOrderPromoted {
		t.Run(tc.name, func(t *testing.T) {
			oracleLog, oracleErr := runDeclineProbe(t, "sqlite3", tc.setup, tc.stmt, tc.probe)
			mushLog, mushErr := runDeclineProbe(t, musqlDriverName, tc.setup, tc.stmt, tc.probe)
			if normProbeErr(oracleErr) != normProbeErr(mushErr) {
				t.Fatalf("[%s] %q:\n  musql: %s\n  oracle: %s\n"+
					"The BEFORE program fires ahead of the constraint checks (insert.c:1494-1496 vs\n"+
					":1569-1571; update.c:978-980 vs :1030). A decline where the oracle answers is\n"+
					"that promotion coming undone; a different error is the order drifting.",
					tc.name, tc.stmt, mushErr, oracleErr)
			}
			if mushLog != oracleLog {
				t.Errorf("[%s] %q probe %s, oracle %s.\n"+
					"A BEFORE program's writes survive an IGNORE on the oracle; answering without\n"+
					"them is a WRONG ANSWER, which AGENTS.md invariant 1 puts BELOW a clean decline.",
					tc.name, tc.stmt, mushLog, oracleLog)
			}
		})
	}
}

// normProbeErr strips this engine's statement prefix so the two engines' error
// TEXT can be compared directly. musql says "engine: UPDATE u: cannot store
// TEXT value in INT column u.i" where the oracle says the tail alone, and the
// tail is the part that carries which constraint fired -- which is the whole
// observable for the STRICT straddle cases above.
func normProbeErr(s string) string {
	if i := strings.LastIndex(s, ": "); i >= 0 {
		for _, tail := range []string{"NOT NULL constraint failed", "cannot store", "CHECK constraint failed",
			"UNIQUE constraint failed", "datatype mismatch"} {
			if j := strings.Index(s, tail); j >= 0 {
				return s[j:]
			}
		}
		_ = i
	}
	return s
}

// runDeclineProbe runs setup then stmt on one engine and returns the probe
// query's rendered rows plus the error stmt raised (empty when it succeeded).
// A setup failure is fatal: a case whose setup both engines rejected agrees
// vacuously, which is differExecOnce's own recorded trap.
func runDeclineProbe(t *testing.T, driver string, setup []string, stmt, probe string) (string, string) {
	t.Helper()
	db, err := sql.Open(driver, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("open %s: %v", driver, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range setup {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("[%s] setup %q: %v", driver, s, err)
		}
	}
	stmtErr := ""
	if _, err := db.Exec(stmt); err != nil {
		stmtErr = err.Error()
	}
	b, _ := json.Marshal(queryOne(db, probe))
	return string(b), stmtErr
}
