// This file gates PRAGMA recursive_triggers and its effects on trigger recursion and REPLACE conflicts.
// The scripts run against engine.DB directly so a whole cascade lives in one
// session, the way the mined TCL corpus drives it; the driver-level half of the
// same state (driver's Conn.recursiveTriggers, which is what makes the flag
// outlive an autocommit statement) gets its own case at the bottom.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

var recursiveTriggerCases = []struct {
	name  string
	stmts []string
}{
	// A self-recursive trigger: suppressed at depth 1 with the flag off, run to
	// the WHEN clause's own bound with it on.
	{"self-recursion-off-then-on", []string{
		`CREATE TABLE t(x INTEGER)`,
		`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.x < 5 BEGIN INSERT INTO t VALUES(new.x+1); END`,
		`INSERT INTO t VALUES(1)`,
		`SELECT group_concat(x) FROM t`,
		`DELETE FROM t`,
		`PRAGMA recursive_triggers=1`,
		`INSERT INTO t VALUES(1)`,
		`SELECT group_concat(x) FROM t`,
		`DELETE FROM t`,
		`PRAGMA recursive_triggers=0`,
		`INSERT INTO t VALUES(1)`,
		`SELECT group_concat(x) FROM t`,
	}},
	// Unbounded self-recursion errors, and the WHOLE statement is rolled back.
	{"unbounded-self-recursion-errors", []string{
		`PRAGMA recursive_triggers=1`,
		`CREATE TABLE t(x INTEGER)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO t VALUES(new.x+1); END`,
		`INSERT INTO t VALUES(1)`,
		`SELECT count(*) FROM t`,
		`SELECT max(x) FROM t`,
	}},
	// The depth boundary itself. SQLite evaluates WHEN inside the pushed frame
	// and tests "nFrame >= 1000" before pushing, so exactly 1000 rows land.
	{"depth-limit-boundary-1000", []string{
		`PRAGMA recursive_triggers=1`,
		`CREATE TABLE t(x INTEGER)`,
		`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.x < 1000 BEGIN INSERT INTO t VALUES(new.x+1); END`,
		`INSERT INTO t VALUES(1)`,
		`SELECT count(*), max(x) FROM t`,
	}},
	{"depth-limit-boundary-1001", []string{
		`PRAGMA recursive_triggers=1`,
		`CREATE TABLE t(x INTEGER)`,
		`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.x < 1001 BEGIN INSERT INTO t VALUES(new.x+1); END`,
		`INSERT INTO t VALUES(1)`,
		`SELECT count(*) FROM t`,
	}},
	// A cycle through two DISTINCT triggers terminates with the flag off (each
	// is suppressed when re-entered) and hits the depth limit with it on.
	{"cross-trigger-cycle", []string{
		`CREATE TABLE a(x)`,
		`CREATE TABLE b(y)`,
		`CREATE TRIGGER ta AFTER INSERT ON a BEGIN INSERT INTO b VALUES(new.x); END`,
		`CREATE TRIGGER tb AFTER INSERT ON b BEGIN INSERT INTO a VALUES(new.y); END`,
		`INSERT INTO a VALUES(1)`,
		`SELECT (SELECT count(*) FROM a), (SELECT count(*) FROM b)`,
		`DELETE FROM a`,
		`DELETE FROM b`,
		`PRAGMA recursive_triggers=1`,
		`INSERT INTO a VALUES(1)`,
		`SELECT (SELECT count(*) FROM a), (SELECT count(*) FROM b)`,
	}},
	// An UPDATE trigger that updates its own table, bounded by its WHERE.
	{"self-recursive-update-trigger", []string{
		`CREATE TABLE t(x INTEGER PRIMARY KEY, n)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN UPDATE t SET n=n+1 WHERE x=old.x AND n<3; END`,
		`UPDATE t SET n=1 WHERE x=1`,
		`SELECT x,n FROM t ORDER BY x`,
		`PRAGMA recursive_triggers=1`,
		`UPDATE t SET n=1 WHERE x=2`,
		`SELECT x,n FROM t ORDER BY x`,
	}},
	// A DELETE trigger that deletes from its own table.
	{"self-recursive-delete-trigger", []string{
		`CREATE TABLE t(x INTEGER PRIMARY KEY)`,
		`INSERT INTO t VALUES(1),(2),(3),(4),(5)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN DELETE FROM t WHERE x=old.x+1; END`,
		`DELETE FROM t WHERE x=1`,
		`SELECT group_concat(x) FROM t`,
		`PRAGMA recursive_triggers=1`,
		`DELETE FROM t WHERE x=3`,
		`SELECT group_concat(x) FROM t`,
	}},
	// The value spellings and the qualifier, which C SQLite ignores.
	{"spellings-and-qualifier", []string{
		`PRAGMA recursive_triggers=on`,
		`PRAGMA recursive_triggers=off`,
		`PRAGMA recursive_triggers=true`,
		`PRAGMA recursive_triggers=false`,
		`PRAGMA recursive_triggers=1`,
		`PRAGMA recursive_triggers=0`,
		`PRAGMA main.recursive_triggers=1`,
		`PRAGMA temp.recursive_triggers=0`,
		`PRAGMA recursive_triggers`,
	}},
	// The setter is NOT ignored inside a transaction, and survives the COMMIT --
	// where foreign_keys' setter would have been dropped.
	{"setter-works-inside-a-transaction", []string{
		`CREATE TABLE t(x INTEGER)`,
		`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.x < 4 BEGIN INSERT INTO t VALUES(new.x+1); END`,
		`BEGIN`,
		`PRAGMA recursive_triggers=1`,
		`INSERT INTO t VALUES(1)`,
		`SELECT group_concat(x) FROM t`,
		`COMMIT`,
		`DELETE FROM t`,
		`INSERT INTO t VALUES(1)`,
		`SELECT group_concat(x) FROM t`,
	}},
	// With the flag ON but no DELETE trigger, a REPLACE still runs -- the
	// decline is narrow.
	{"replace-without-delete-triggers-still-runs", []string{
		`PRAGMA recursive_triggers=1`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO log VALUES('ins ' || new.a); END`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT OR REPLACE INTO t VALUES(1,'two')`,
		`SELECT a,b FROM t`,
		`SELECT group_concat(m) FROM log`,
	}},
	// And with the flag OFF the pre-existing rule still holds: the victim's
	// DELETE triggers do NOT fire.
	{"replace-with-delete-triggers-and-the-flag-off", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(m)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del ' || old.a); END`,
		`CREATE TRIGGER tdb BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('bdel ' || old.a); END`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT OR REPLACE INTO t VALUES(1,'two')`,
		`SELECT a,b FROM t`,
		`SELECT count(*) FROM log`,
	}},
}

// TestRecursiveTriggersPragma replays each script through both engines and
// requires the same accept/reject decision and the same rows.
func TestRecursiveTriggersPragma(t *testing.T) {
	for _, tc := range recursiveTriggerCases {
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
			cgodb.SetMaxOpenConns(1) // one logical connection: the flag must span statements

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

// TestRecursiveTriggersReplaceFiresDeleteTriggers is what used to be
// TestRecursiveTriggersReplaceDeclined, on the same three fixtures. It asserted
// a clean decline, and had been failing on main with "expected a decline, got
// success" for all three once the shape was implemented
// (compileReplaceVictimDeletePlans, engine/trigger.go;
// fireReplaceVictimDeleteRow, engine/vdbe_write.go). The decline text it also
// checked for -- "recursive_triggers is ON" -- is no longer produced for any
// of them.
//
// It asserts agreement with the live oracle now. The fixtures are unchanged
// except for a trailing read, so the comparison is over the resulting rows
// rather than merely over "did it error"; each trigger body is left as the
// original's "SELECT 1" so the only thing under test is the FIRING, not what a
// body does. compat-harness/batch_l_test.go carries the cases with real
// side-effect bodies, and engine/replace_victim_codegen_test.go asserts the
// INSERT halves genuinely COMPILE -- which is what neither this file nor that
// one could show on its own.
func TestRecursiveTriggersReplaceFiresDeleteTriggers(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"insert-or-replace-on-the-rowid", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN SELECT 1; END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT OR REPLACE INTO t VALUES(1,'two')`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT changes()`,
		}},
		{"insert-or-replace-on-a-unique-index", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE t(a, b UNIQUE)`,
			`CREATE TRIGGER td BEFORE DELETE ON t BEGIN SELECT 1; END`,
			`INSERT INTO t VALUES('x',1)`,
			`INSERT OR REPLACE INTO t VALUES('y',1)`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT changes()`,
		}},
		{"update-or-replace", []string{
			`PRAGMA recursive_triggers=1`,
			`CREATE TABLE t(a INTEGER UNIQUE, b TEXT)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN SELECT 1; END`,
			`INSERT INTO t VALUES(1,'one')`,
			`INSERT INTO t VALUES(2,'two')`,
			`UPDATE OR REPLACE t SET a=a+1 WHERE a>=1`,
			`SELECT a,b FROM t ORDER BY a`,
			`SELECT changes()`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "recursive-triggers-replace-"+tc.name, tc.stmts) })
	}
}

// TestRecursiveTriggersSpansDriverStatements is the driver half: the flag is
// CONNECTION state, and this driver opens a fresh engine.DB per autocommit
// statement, so without driver's Conn.recursiveTriggers the cascade below
// would stop one level in. Compared against the oracle through the same
// worker-based path every other driver-level gate uses.
func TestRecursiveTriggersSpansDriverStatements(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t(x INTEGER)`,
		`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.x < 5 BEGIN INSERT INTO t VALUES(new.x+1); END`,
		`PRAGMA recursive_triggers=1`,
		`INSERT INTO t VALUES(1)`,
		`SELECT group_concat(x) FROM t`,
		`PRAGMA recursive_triggers`,
		`PRAGMA recursive_triggers=0`,
		`DELETE FROM t`,
		`INSERT INTO t VALUES(1)`,
		`SELECT group_concat(x) FROM t`,
		`PRAGMA recursive_triggers`,
	}
	differ(t, "recursive-triggers-across-driver-statements", stmts)
}
