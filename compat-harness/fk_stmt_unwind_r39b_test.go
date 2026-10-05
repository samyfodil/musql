// Tests foreign-key statement unwinding when a violation is detected.
// Statements must be rolled back completely, including rows applied through
// non-journalled routes, and triggers must be correctly undone.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// r39bFKUnwindCases each end with the same four readbacks so a case that
// diverges says WHICH channel saw it: the row store, the parent table, the
// trigger log, and the two counters.
var r39bFKUnwindCases = []struct {
	name  string
	stmts []string
}{
	// ---- the fallback, with no trigger anywhere ----
	//
	// STRICT declines to the fallback (compileInsertStmt: a STRICT table's
	// per-row type check has no opcode), so this plain INSERT of an orphan is
	// the whole bug with nothing else in the picture.
	{"strict-table-insert", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c INT REFERENCES p(k)) STRICT`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
		`INSERT INTO t(a,b,c) VALUES(2,'w',100)`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	// A schema-qualified target reached the same defect by a completely
	// different route, which is why it is here alongside the others.
	{"schema-qualified-insert", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
		`INSERT INTO main.t(a,b,c) VALUES(2,'w',100)`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	// ...and so does UPDATE ... FROM, which is an UPDATE rather than an INSERT.
	{"update-from", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`,
		`CREATE TABLE src(a INTEGER, b TEXT, c)`,
		`INSERT INTO src VALUES(2,'w',100)`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',10)`,
		`UPDATE t SET b = src.b, c = src.c FROM src WHERE src.a = t.a`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},

	// ---- the fallback, with a trigger whose side effect must go too ----
	//
	// The trigger's log row is the second observation: SQLite's statement
	// rollback takes it back, while its total_changes() still counts it
	// (vdbe.c:1314). A gate that only checked the target table would pass on a
	// half-fix.
	{"upsert-do-update-under-a-before-insert-trigger", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10'),(99,'p99')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',10)`,
		`CREATE TRIGGER g1 BEFORE INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('bi',new.a,new.b); END`,
		`INSERT INTO t(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO UPDATE SET c = excluded.c + 1`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	// The same upsert, but the trigger the DO UPDATE arm itself fires.
	{"upsert-do-update-under-an-after-update-trigger", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10'),(99,'p99')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',10)`,
		`CREATE TRIGGER g1 AFTER UPDATE ON t BEGIN INSERT INTO log(e,x,y) VALUES('au',old.a,new.b); END`,
		`INSERT INTO t(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO UPDATE SET c = excluded.c + 1`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	{"insert-select-under-a-before-insert-trigger", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`,
		`CREATE TABLE src(a INTEGER, b TEXT, c)`,
		`INSERT INTO src VALUES(2,'w',100)`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
		`CREATE TRIGGER g1 BEFORE INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('bi',new.a,new.b); END`,
		`INSERT INTO t(a,b,c) SELECT a,b,c FROM src`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	// The PARENT side reached through the fallback: a RESTRICT referrer refuses
	// the delete, and the BEFORE DELETE trigger's log row goes back with it.
	{"parent-restrict-delete-under-a-before-delete-trigger", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10'),(20,'p20')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, c REFERENCES p(k) ON DELETE RESTRICT)`,
		`INSERT INTO t VALUES(1,10)`,
		`CREATE TRIGGER g1 BEFORE DELETE ON p BEGIN INSERT INTO log(e,x,y) VALUES('bd',old.k,old.v); END`,
		`DELETE FROM p WHERE k = 10`,
		`SELECT a,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},

	// ---- controls: the shapes that were already right ----
	//
	// The compiled path, which journals and therefore always unwound. If one of
	// these ever diverges the bug is in the storage opcodes, not the fallback.
	{"compiled-insert-values-is-the-control", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
		`INSERT INTO t(a,b,c) VALUES(2,'w',100)`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	{"compiled-update-is-the-control", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',10)`,
		`UPDATE t SET c = 100 WHERE a = 2`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	// A statement that SUCCEEDS through the fallback in an FK schema must be
	// left entirely alone -- the snapshot is taken there too, and taking one is
	// not allowed to change anything.
	{"a-successful-fallback-statement-is-untouched", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10'),(20,'p20')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c INT REFERENCES p(k)) STRICT`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
		`INSERT INTO main.t(a,b,c) VALUES(2,'w',20)`,
		`UPDATE main.t SET c = 10 WHERE a = 2`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	// Inside an explicit transaction the unwind must be STATEMENT-scoped: the
	// earlier statement's row survives the failed one, and the COMMIT lands it.
	{"the-unwind-is-statement-scoped-inside-a-transaction", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c INT REFERENCES p(k)) STRICT`,
		`BEGIN`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
		`INSERT INTO t(a,b,c) VALUES(2,'w',100)`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`COMMIT`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	// The same, through a SAVEPOINT, and with the DDL of the surrounding
	// transaction untouched.
	{"the-unwind-is-statement-scoped-inside-a-savepoint", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c INT REFERENCES p(k)) STRICT`,
		`SAVEPOINT sp`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
		`INSERT INTO t(a,b,c) VALUES(2,'w',100)`,
		`RELEASE sp`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
	// foreign_keys=OFF takes no snapshot at all, so the statement must land in
	// full -- the orphan included, which is what the pragma means.
	{"foreign-keys-off-keeps-the-orphan", []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO p(k,v) VALUES(10,'p10')`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c INT REFERENCES p(k)) STRICT`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
		`INSERT INTO t(a,b,c) VALUES(2,'w',100)`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT k,v FROM p ORDER BY k`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
	}},
}

// TestR39BForeignKeyStatementUnwind replays each script against engine.DB and
// C SQLite in lockstep.
//
// If this fails with the target table holding a row whose key has no parent
// while both engines reported "FOREIGN KEY constraint failed", the unwind is
// the suspect: engine/vdbe_write.go's runWrite must reach
// unwindFKViolation, because wc.rollback() alone unwinds only what the storage
// OPCODES journaled and returns early on an empty journal.
func TestR39BForeignKeyStatementUnwind(t *testing.T) {
	for _, tc := range r39bFKUnwindCases {
		t.Run(tc.name, func(t *testing.T) {
			godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer cgodb.Close()
			cgodb.SetMaxOpenConns(1) // one logical connection: BEGIN must span statements

			for i, stmt := range tc.stmts {
				if tclIsQuery(stmt) {
					goCols, goRows, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
					if panicked {
						t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, stmt, panicVal)
					}
					cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, stmt)
					if (qerr != nil) != (cerr != nil) {
						t.Fatalf("stmt #%d %q: query error disagreement\n  go:  %v\n  cgo: %v", i, stmt, qerr, cerr)
					}
					if qerr != nil {
						continue
					}
					if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
						t.Fatalf("stmt #%d %q: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v",
							i, stmt, reason, goCols, goRows, cgoCols, cgoRows)
					}
					continue
				}
				execErr, panicked, panicVal := tclSafeExecArgs(godb, stmt)
				if panicked {
					t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, stmt, panicVal)
				}
				_, cerr := cgodb.Exec(stmt)
				if (execErr != nil) != (cerr != nil) {
					t.Fatalf("stmt #%d %q: exec error disagreement\n  go:  %v\n  cgo: %v", i, stmt, execErr, cerr)
				}
			}
		})
	}
}
