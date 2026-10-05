// Tests multi-statement scripts (separated by ";") passed to Exec. A script
// stops at the first error but keeps earlier statements applied. Result and
// RowsAffected come from the last statement, not an aggregate.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// mseRun Execs one script and reports the outcome plus what the database then
// holds, so a script that "succeeds" without running cannot pass.
func mseRun(t *testing.T, driver, dsn, script string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s open: %v", driver, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	out := "ok"
	if res, eerr := db.Exec(script); eerr != nil {
		out = "ERR"
	} else {
		ra, _ := res.RowsAffected()
		li, _ := res.LastInsertId()
		out = fmt.Sprintf("ok RA=%d LI=%d", ra, li)
	}
	var tables, urows, lrows int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables)
	db.QueryRow(`SELECT count(*) FROM u`).Scan(&urows)
	db.QueryRow(`SELECT count(*) FROM l`).Scan(&lrows)
	var sum sql.NullInt64
	db.QueryRow(`SELECT sum(x) FROM u`).Scan(&sum)
	return fmt.Sprintf("%s tables=%d u=%d l=%d sum=%v", out, tables, urows, lrows, sum)
}

func mseDiffer(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	got := mseRun(t, "sqlite", filepath.Join(dir, "musql.db"), script)
	want := mseRun(t, "sqlite3", filepath.Join(dir, "cgo.db"), script)
	if got != want {
		t.Errorf("[%s] script %q DIVERGES\n  cgo:    %s\n  musql: %s", name, script, want, got)
	}
}

func TestMultiStatementExec(t *testing.T) {
	for _, c := range []struct{ name, script string }{
		{"two-ok", `CREATE TABLE u(x); INSERT INTO u VALUES(1)`},
		{"three", `CREATE TABLE u(x); INSERT INTO u VALUES(1); INSERT INTO u VALUES(2)`},
		// Script stops at first error, keeps earlier statements
		{"second-fails", `CREATE TABLE u(x); INSERT INTO nosuch VALUES(1)`},
		{"first-fails", `INSERT INTO nosuch VALUES(1); CREATE TABLE u(x)`},
		{"third-fails", `CREATE TABLE u(x); INSERT INTO u VALUES(1); INSERT INTO nosuch VALUES(2)`},
		// Semicolon edge cases
		{"trailing-semi", `CREATE TABLE u(x); INSERT INTO u VALUES(1);`},
		{"leading-semi", `; CREATE TABLE u(x)`},
		{"doubled-semi", `CREATE TABLE u(x);; INSERT INTO u VALUES(1)`},
		// Queries in scripts discard results
		{"select-then-write", `SELECT 1; CREATE TABLE u(x)`},
		{"write-then-select", `CREATE TABLE u(x); SELECT 1`},
		{"select-middle", `CREATE TABLE u(x); SELECT 1; INSERT INTO u VALUES(1)`},
		// Semicolons in strings and comments are not boundaries
		{"semi-in-string", `CREATE TABLE u(x); INSERT INTO u VALUES('a;b')`},
		{"semi-in-comment", "CREATE TABLE u(x); -- a; comment\nINSERT INTO u VALUES(1)"},
		{"semi-in-block-comment", "CREATE TABLE u(x); /* a; b */ INSERT INTO u VALUES(1)"},
		// Trigger bodies with multiple statements
		{"trigger-body", `CREATE TABLE u(x); CREATE TABLE l(v); CREATE TRIGGER tr AFTER INSERT ON u BEGIN INSERT INTO l VALUES(new.x); END; INSERT INTO u VALUES(9)`},
		{"trigger-two-steps", `CREATE TABLE u(x); CREATE TABLE l(v); CREATE TRIGGER tr AFTER INSERT ON u BEGIN INSERT INTO l VALUES(1); INSERT INTO l VALUES(2); END; INSERT INTO u VALUES(9)`},
		{"trigger-with-case", `CREATE TABLE u(x); CREATE TABLE l(v); CREATE TRIGGER tr AFTER INSERT ON u BEGIN INSERT INTO l VALUES(CASE WHEN new.x>0 THEN 1 ELSE 0 END); END; INSERT INTO u VALUES(9)`},
		// Transactions in scripts
		{"txn-commit", `CREATE TABLE u(x); BEGIN; INSERT INTO u VALUES(1); INSERT INTO u VALUES(2); COMMIT`},
		{"txn-rollback", `CREATE TABLE u(x); BEGIN; INSERT INTO u VALUES(1); ROLLBACK`},
		// Complex migration script
		{"migration", `CREATE TABLE u(x INTEGER PRIMARY KEY); CREATE TABLE l(v); CREATE INDEX ux ON u(x); INSERT INTO u VALUES(1); INSERT INTO u VALUES(2); INSERT INTO l VALUES(9)`},
		// Single statements unchanged
		{"single-control", `CREATE TABLE u(x)`},
		{"single-semi-control", `CREATE TABLE u(x);`},
	} {
		mseDiffer(t, c.name, c.script)
	}
}

// TestMultiStatementQueryDeclined verifies that multi-statement scripts must
// be Exec'd, not Query'd.
func TestMultiStatementQueryDeclined(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, qerr := db.Query(`CREATE TABLE u(x); SELECT 1`)
	if qerr == nil {
		rows.Close()
		t.Error("Query of a multi-statement script succeeded; it must be declined")
	}
}
