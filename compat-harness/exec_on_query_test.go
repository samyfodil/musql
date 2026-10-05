// Tests Exec'ing statements that return rows.
// it reports whatever the previous statement changed, so the same Exec answers
// 0, 1 or 3 depending only on what ran before it. engine/vdbe_write.go already
// records that measurement as the reason a RETURNING statement is declined on
// the Exec path. Reproducing it would be bug-compatibility, not parity, so
// these cases assert that the statement RAN -- and what the database then
// holds -- rather than what it counted.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// eoqExec Execs q and reports only whether it errored, then reads the database
// back so a statement that "succeeded" without running is caught.
func eoqExec(t *testing.T, driver, dsn string, setup []string, q, read string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s open: %v", driver, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, s := range setup {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s setup %q: %v", driver, s, eerr)
		}
	}
	out := "ok"
	if _, eerr := db.Exec(q); eerr != nil {
		out = "err"
	}
	if read == "" {
		return out
	}
	var v any
	if serr := db.QueryRow(read).Scan(&v); serr != nil {
		return out + " read-err"
	}
	return fmt.Sprintf("%s read=%v", out, v)
}

func eoqDiffer(t *testing.T, setup []string, q, read string) {
	t.Helper()
	dir := t.TempDir()
	got := eoqExec(t, "sqlite", filepath.Join(dir, "musql.db"), setup, q, read)
	want := eoqExec(t, "sqlite3", filepath.Join(dir, "cgo.db"), setup, q, read)
	if got != want {
		t.Errorf("Exec %q DIVERGES\n  cgo:    %s\n  musql: %s", q, want, got)
	}
}

var eoqSetup = []string{
	`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
	`INSERT INTO t VALUES(1,'one'),(2,'two')`,
}

func TestExecOnRowReturningStatement(t *testing.T) {
	for _, c := range []struct{ q, read string }{
		{`SELECT 1`, ``},
		{`SELECT * FROM t`, ``},
		{`SELECT count(*) FROM t`, ``},
		{`SELECT * FROM t WHERE a = 99`, ``}, // no rows
		{`SELECT a FROM t ORDER BY a`, ``},
		{`VALUES(1,2)`, ``},
		{`WITH x AS (SELECT 1 AS v) SELECT v FROM x`, ``},
		{`SELECT * FROM t JOIN t AS u ON t.a = u.a`, ``},
		{`SELECT nosuchcolumn FROM t`, ``}, // still an ERROR on both
		{`SELECT * FROM nosuchtable`, ``},  // still an ERROR on both
		// ...and the shapes that already worked, as controls
		{`INSERT INTO t VALUES(3,'three')`, `SELECT count(*) FROM t`},
		{`UPDATE t SET b='z' WHERE a=1`, `SELECT b FROM t WHERE a=1`},
		{`DELETE FROM t WHERE a=2`, `SELECT count(*) FROM t`},
		{`CREATE TABLE u(x)`, `SELECT count(*) FROM sqlite_master`},
		{`PRAGMA table_info(t)`, ``},
	} {
		eoqDiffer(t, eoqSetup, c.q, c.read)
	}
}

// TestExecOnQueryStillRuns is the half that matters: an Exec'd query must
// actually EXECUTE. A bare SELECT has no side effect to show that with, so
// this drives a write through a view's INSTEAD OF trigger.
func TestExecOnQueryStillRuns(t *testing.T) {
	eoqDiffer(t, []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(v)`,
		`CREATE VIEW tv AS SELECT a, b FROM t`,
		`CREATE TRIGGER tvi INSTEAD OF INSERT ON tv BEGIN INSERT INTO log VALUES(new.a); END`,
	}, `INSERT INTO tv VALUES(7,'x')`, `SELECT count(*) FROM log`)
}

// TestExecOnReturningThroughExec is the case that was declined outright until
// its premise was measured. Exec'ing a RETURNING statement used to error, and
// erroring meant THE WRITE DID NOT HAPPEN -- justified on the grounds that
// mattn's RowsAffected was "genuinely nondeterministic ... there is no value
// musql could return here that matches byte-for-byte".
//
// It is deterministic. mattn steps such a statement ONCE, so when it emits a
// row the count it reads is the PRECEDING statement's, and when it emits none
// the statement ran to completion and the count is its own (necessarily 0,
// since a RETURNING clause emits one row per row changed). Both halves are
// reproducible from state the Conn already tracks.
//
// So this compares RowsAffected AS A VALUE -- not merely "both succeeded" --
// while varying the preceding statement across the range that made the number
// look random, and reads the table back so a matching count over a lost write
// cannot pass.
func TestExecOnReturningThroughExec(t *testing.T) {
	base := `CREATE TABLE t(id INTEGER PRIMARY KEY, v)`
	for _, c := range []struct{ name, prev, stmt string }{
		{"after-create", ``, `INSERT INTO t(v) VALUES(5) RETURNING id`},
		{"after-1-row", `INSERT INTO t(v) VALUES(1)`, `INSERT INTO t(v) VALUES(5) RETURNING id`},
		{"after-3-row", `INSERT INTO t(v) VALUES(1),(2),(3)`, `INSERT INTO t(v) VALUES(5) RETURNING id`},
		{"after-7-row", `INSERT INTO t(v) VALUES(1),(2),(3),(4),(5),(6),(7)`, `INSERT INTO t(v) VALUES(5) RETURNING id`},
		{"after-update-2", `INSERT INTO t(v) VALUES(1),(2)`, `UPDATE t SET v=9 RETURNING id`},
		{"after-delete-2", `INSERT INTO t(v) VALUES(1),(2)`, `DELETE FROM t RETURNING id`},
		{"multi-row-returning", `INSERT INTO t(v) VALUES(1)`, `INSERT INTO t(v) VALUES(5),(6),(7) RETURNING id`},
		{"update-returning-3", `INSERT INTO t(v) VALUES(1),(2),(3)`, `UPDATE t SET v=v+1 RETURNING id`},
		{"delete-returning-3", `INSERT INTO t(v) VALUES(1),(2),(3)`, `DELETE FROM t RETURNING id`},
		// emits NOTHING: the statement runs to completion, so the count is its own
		{"update-returning-0", `INSERT INTO t(v) VALUES(1),(2),(3)`, `UPDATE t SET v=v+1 WHERE id>99 RETURNING id`},
		{"delete-returning-0", `INSERT INTO t(v) VALUES(1),(2),(3)`, `DELETE FROM t WHERE id>99 RETURNING id`},
		// INSERT ... SELECT ... RETURNING: the engine declined this until it
		// grew the SELECT source's own row-image capture, and
		// TestInsertSelectReturningIsAnEngineDecline held the place until then.
		{"insert-select-returning", `INSERT INTO t(v) VALUES(1),(2)`, `INSERT INTO t(v) SELECT v FROM t RETURNING id`},
		{"insert-select-returning-0", `INSERT INTO t(v) VALUES(1),(2)`, `INSERT INTO t(v) SELECT v FROM t WHERE id>99 RETURNING id`},
	} {
		c := c
		setup := []string{base}
		if c.prev != "" {
			setup = append(setup, c.prev)
		}
		dir := t.TempDir()
		got := eoqResult(t, "sqlite", filepath.Join(dir, "musql.db"), setup, c.stmt)
		want := eoqResult(t, "sqlite3", filepath.Join(dir, "cgo.db"), setup, c.stmt)
		if got != want {
			t.Errorf("[%s] Exec %q DIVERGES\n  cgo:    %s\n  musql: %s", c.name, c.stmt, want, got)
		}
	}
}

// eoqResult Execs one statement and reports RowsAffected, LastInsertId AND
// what the table then holds -- so a right count over a lost write fails.
func eoqResult(t *testing.T, driver, dsn string, setup []string, q string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s open: %v", driver, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, s := range setup {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s setup %q: %v", driver, s, eerr)
		}
	}
	res, eerr := db.Exec(q)
	if eerr != nil {
		return "ERR"
	}
	ra, _ := res.RowsAffected()
	li, _ := res.LastInsertId()
	var n int
	var sum sql.NullInt64
	db.QueryRow(`SELECT count(*), sum(v) FROM t`).Scan(&n, &sum)
	return fmt.Sprintf("RA=%d LI=%d rows=%d sum=%v", ra, li, n, sum)
}

// TestExecOnQueryPrepared runs the same through an explicitly prepared
// statement, which is the path database/sql uses for db.Exec anyway but is
// worth pinning directly since Stmt is where the guard lived.
func TestExecOnQueryPrepared(t *testing.T) {
	for _, q := range []string{`SELECT 1`, `SELECT * FROM t`, `SELECT ?`} {
		q := q
		dir := t.TempDir()
		run := func(driver, dsn string) string {
			db, err := sql.Open(driver, dsn)
			if err != nil {
				t.Fatalf("%s open: %v", driver, err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			for _, s := range eoqSetup {
				if _, eerr := db.Exec(s); eerr != nil {
					t.Fatalf("%s setup: %v", driver, eerr)
				}
			}
			st, perr := db.Prepare(q)
			if perr != nil {
				return "prepare-err"
			}
			defer st.Close()
			var args []any
			if q == `SELECT ?` {
				args = append(args, 1)
			}
			if _, eerr := st.Exec(args...); eerr != nil {
				return "err"
			}
			return "ok"
		}
		got := run("sqlite", filepath.Join(dir, "musql.db"))
		want := run("sqlite3", filepath.Join(dir, "cgo.db"))
		if got != want {
			t.Errorf("prepared Exec %q DIVERGES\n  cgo: %s\n  musql: %s", q, want, got)
		}
	}
}
