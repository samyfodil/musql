// PRAGMA query_only must refuse all write statements.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

// queryOnlySetup builds the test schema.
var queryOnlySetup = []string{
	`CREATE TABLE t1(a)`,
	`CREATE TABLE u(a)`,
	`CREATE INDEX u_a ON u(a)`,
	`INSERT INTO t1 VALUES(123),(456)`,
}

// queryOnlyProbes are statements tested with query_only=ON.
var queryOnlyProbes = []string{
	// Refused by both.
	`INSERT INTO t1 VALUES(789)`,
	`DELETE FROM t1`,
	`UPDATE t1 SET a=a+1`,
	`DELETE FROM t1 WHERE 0`,
	`UPDATE t1 SET a=a WHERE 0`,
	`INSERT INTO t1 SELECT * FROM u`,
	`REPLACE INTO t1 VALUES(1)`,
	`CREATE TABLE t2(b)`,
	`CREATE INDEX t1a ON t1(a)`,
	`CREATE VIEW v1 AS SELECT 1`,
	`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT 1; END`,
	`DROP TABLE t1`,
	`DROP INDEX u_a`,
	`ALTER TABLE t1 RENAME TO t9`,
	`ANALYZE`,
	`VACUUM`,
	`REINDEX`,
	`CREATE TEMP TABLE tt(x)`,
	`PRAGMA user_version=4`,
	`PRAGMA schema_version=9`,
	`PRAGMA incremental_vacuum`,
	`BEGIN IMMEDIATE`,
	`BEGIN EXCLUSIVE`,
	// Accepted by both.
	`SELECT a FROM t1 ORDER BY a`,
	`SELECT count(*) FROM t1`,
	`BEGIN`,
	`COMMIT`,
	`BEGIN DEFERRED`,
	`ROLLBACK`,
	`SAVEPOINT sp`,
	`RELEASE sp`,
	`PRAGMA integrity_check`,
	`PRAGMA user_version`,
	`PRAGMA table_info(t1)`,
	`PRAGMA foreign_keys=ON`,
	`PRAGMA foreign_keys=OFF`,
	`PRAGMA journal_mode=delete`,
	`PRAGMA secure_delete=1`,
	`PRAGMA case_sensitive_like=1`,
	`PRAGMA case_sensitive_like=0`,
	`PRAGMA cache_size=100`,
	// A COMPILE-time error still wins over the readonly one -- both engines
	// error, which is all the exec side compares, but the shape is pinned.
	`INSERT INTO nosuchtable VALUES(1)`,
	`INSERT INTO t1(nosuchcol) VALUES(1)`,
	`INSERT INTO t1 VALUES(1,2)`,
}

// TestQueryOnlyPragma runs the whole probe list with the flag ON, then turns it
// off and requires writing to work again -- and the table to be untouched by
// everything above.
func TestQueryOnlyPragma(t *testing.T) {
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

	run := func(stmts []string, fatalOnDisagree bool) {
		t.Helper()
		for i, stmt := range stmts {
			if tclIsQuery(stmt) {
				goCols, goRows, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
				if panicked {
					t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, stmt, panicVal)
				}
				cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, stmt)
				if (qerr != nil) != (cerr != nil) {
					t.Errorf("%q: query error disagreement\n  go:  %v\n  cgo: %v", stmt, qerr, cerr)
					continue
				}
				if qerr != nil {
					continue
				}
				if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
					t.Errorf("%q: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v",
						stmt, reason, goCols, goRows, cgoCols, cgoRows)
				}
				continue
			}
			execErr, panicked, panicVal := tclSafeExecArgs(godb, stmt)
			if panicked {
				t.Fatalf("stmt #%d %q: engine PANICKED: %v", i, stmt, panicVal)
			}
			_, cerr := cgodb.Exec(stmt)
			if (execErr != nil) != (cerr != nil) {
				msg := "%q: exec error disagreement\n  go:  %v\n  cgo: %v"
				if fatalOnDisagree {
					t.Fatalf(msg, stmt, execErr, cerr)
				}
				t.Errorf(msg, stmt, execErr, cerr)
			}
		}
	}

	run(queryOnlySetup, true)
	run([]string{`PRAGMA query_only=ON`}, true)
	run(queryOnlyProbes, false)
	run([]string{
		`SELECT a FROM t1 ORDER BY a`, // nothing above changed a thing
		`PRAGMA query_only=OFF`,
		`INSERT INTO t1 VALUES(789)`,
		`SELECT a FROM t1 ORDER BY a`,
		`UPDATE t1 SET a=a+1`,
		`SELECT a FROM t1 ORDER BY a`,
	}, true)
}

// TestQueryOnlySetterInsideTransaction pins the rule that differs from
// foreign_keys': the setter is NOT ignored inside a transaction, and it is
// still set after the COMMIT.
func TestQueryOnlySetterInsideTransaction(t *testing.T) {
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
	cgodb.SetMaxOpenConns(1)

	for _, stmt := range []string{
		`CREATE TABLE t(a)`,
		`BEGIN`,
		`PRAGMA query_only=1`,
		`INSERT INTO t VALUES(1)`, // refused, inside the transaction
		`COMMIT`,
		`INSERT INTO t VALUES(2)`, // still refused after it
		`PRAGMA query_only=0`,
		`INSERT INTO t VALUES(3)`,
	} {
		execErr, panicked, panicVal := tclSafeExecArgs(godb, stmt)
		if panicked {
			t.Fatalf("%q: engine PANICKED: %v", stmt, panicVal)
		}
		_, cerr := cgodb.Exec(stmt)
		if (execErr != nil) != (cerr != nil) {
			t.Fatalf("%q: exec error disagreement\n  go:  %v\n  cgo: %v", stmt, execErr, cerr)
		}
	}
	goCols, goRows, qerr, _, _ := tclSafeGoQuery(godb, `SELECT a FROM t ORDER BY a`)
	if qerr != nil {
		t.Fatalf("final SELECT: %v", qerr)
	}
	cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, `SELECT a FROM t ORDER BY a`)
	if cerr != nil {
		t.Fatalf("final SELECT on the oracle: %v", cerr)
	}
	if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
		t.Fatalf("final SELECT: %s\n  go:  %v\n  cgo: %v", reason, goRows, cgoRows)
	}
}

// TestQueryOnlySpansDriverStatements is the driver half: the flag is CONNECTION
// state and this driver opens a fresh engine.DB per autocommit statement, so
// without driver's Conn.queryOnly the write below would succeed where real
// SQLite refuses it -- a wrong answer, not a gap.
func TestQueryOnlySpansDriverStatements(t *testing.T) {
	differ(t, "query-only-across-driver-statements", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA query_only=ON`,
		`PRAGMA query_only`,
		`INSERT INTO t VALUES(2)`,
		`CREATE TABLE t2(b)`,
		`SELECT a FROM t ORDER BY a`,
		`PRAGMA query_only=OFF`,
		`INSERT INTO t VALUES(3)`,
		`SELECT a FROM t ORDER BY a`,
	})
}
