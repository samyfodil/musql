package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestConnRowidRange: a range set on a Conn reaches its main database's
// inserts, and survives a transaction that clears the table and rolls back
// (the rollback reinstates the whole row map, and the range's cached max with
// it) -- the next insert must not reuse a rowid the restored rows hold.
func TestConnRowidRange(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(c any) error { c.(*Conn).SetRowidRange(100, 200); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(v)`,
		`INSERT INTO t VALUES('a'),('b')`, // 100, 101
		`BEGIN`,
		`DELETE FROM t`,
		`INSERT INTO t VALUES('x')`, // 100, into the emptied range
		`ROLLBACK`,
		`INSERT INTO t VALUES('c')`, // 102
	} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var got string
	if err := conn.QueryRowContext(ctx, `SELECT group_concat(rowid||v) FROM (SELECT rowid, v FROM t ORDER BY rowid)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "100a,101b,102c"; got != want {
		t.Fatalf("rows %s, want %s", got, want)
	}
}

// TestPreCommitFailureLeavesNothing: when the capture hook fails a commit, the
// statement's rows are gone from the connection that ran it too -- autocommit
// and an explicit transaction alike. They are in the row stores (the statement
// itself succeeded), and were read back by the same connection before.
func TestPreCommitFailureLeavesNothing(t *testing.T) {
	RegisterConnectionHook(func(c *Conn, dsn string) error {
		if strings.Contains(dsn, "failcommit=1") {
			c.SetCaptureHooks(&CaptureHooks{PreCommit: func(context.Context, *Conn, []engine.RowChange) error {
				return errors.New("hook refused")
			}})
		}
		return nil
	})
	p := filepath.Join(t.TempDir(), "f.db")
	plain, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.Exec(`CREATE TABLE t(a)`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p+"?failcommit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`INSERT INTO t VALUES(1)`); err == nil {
		t.Fatal("autocommit: the refused commit succeeded")
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO t VALUES(2)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("transaction: the refused commit succeeded")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the connection that ran the refused commits sees %d rows, want 0", n)
	}
}

// TestBeginImmediateLocks: BEGIN IMMEDIATE takes the write lock at the BEGIN,
// as C's does (build.c:5258): another connection's write is BUSY while it is
// open, a read is not, the transaction's own COMMIT succeeds, and COMMIT and
// ROLLBACK both give the lock back. Before, the BEGIN took nothing, the other
// write went through, and the IMMEDIATE transaction was the one that failed.
func TestBeginImmediateLocks(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "i.db")
	open := func() *sql.Conn {
		t.Helper()
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if _, err := c.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := open(), open()
	exec := func(c *sql.Conn, s string) error {
		_, err := c.ExecContext(ctx, s)
		return err
	}
	must := func(c *sql.Conn, s string) {
		t.Helper()
		if err := exec(c, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	must(a, `CREATE TABLE t(v)`)
	for _, end := range []string{`COMMIT`, `ROLLBACK`} {
		for _, begin := range []string{`BEGIN IMMEDIATE`, `BEGIN EXCLUSIVE`} {
			must(a, begin)
			if err := exec(b, `INSERT INTO t VALUES('b')`); err == nil {
				t.Fatalf("%s ... %s: another connection wrote inside it", begin, end)
			}
			if begin == `BEGIN IMMEDIATE` {
				var n int
				if err := b.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
					t.Fatalf("%s: a reader was locked out: %v", begin, err)
				}
			}
			must(a, `INSERT INTO t VALUES('a')`)
			must(a, end)
			must(b, `INSERT INTO t VALUES('b')`) // the lock is back
		}
	}
	var got string
	if err := b.QueryRowContext(ctx, `SELECT group_concat(v, '') FROM t`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "abab" + "bb"; got != want {
		t.Fatalf("rows %q, want %q", got, want)
	}
}

// TestCaptureSeesMainOnly: the capture hook is handed main's changes and never
// an ATTACHed database's -- neither its DDL (which arrived with its qualifier
// stripped) nor its rows (which arrived as a change to a non-temp table of the
// same name). A TEMP change arrives marked Temp, for the consumer to skip.
func TestCaptureSeesMainOnly(t *testing.T) {
	var seen []string
	RegisterConnectionHook(func(c *Conn, dsn string) error {
		if strings.Contains(dsn, "mainonly=1") {
			c.SetCaptureHooks(&CaptureHooks{PreCommit: func(_ context.Context, _ *Conn, chs []engine.RowChange) error {
				for _, ch := range chs {
					seen = append(seen, fmt.Sprintf("%d %s %v %s", ch.Kind, ch.Table, ch.Temp, ch.SQL))
				}
				return nil
			}})
		}
		return nil
	})
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db")+"?mainonly=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`ATTACH '` + filepath.Join(dir, "aux.db") + `' AS aux`,
		`CREATE TABLE aux.u(a)`,
		`INSERT INTO aux.u VALUES(1)`,
		`CREATE TABLE u(a)`,
		`INSERT INTO u VALUES(2)`,
		`CREATE TEMP TABLE x(a)`,
		`INSERT INTO x VALUES(3)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	got := strings.Join(seen, "; ")
	want := fmt.Sprintf("%d  false CREATE TABLE u(a); %d u false ; %d  false CREATE TEMP TABLE x(a); %d x true ",
		engine.RowSchema, engine.RowInsert, engine.RowSchema, engine.RowInsert)
	if got != want {
		t.Fatalf("hook saw\n%s\nwant\n%s", got, want)
	}
}

// TestRewriteKeepsNoMappedRows: a file rewrite (any DDL) swaps the segment
// mapping, so every row a session KEEPS across it must not point into the old
// one. Two kinds were kept and faulted the next rewrite's segment builder: a
// table renamed this session (not in the file under its new name, so not
// reloaded), and a TEMP table whose rows were copied out of a main table.
func TestRewriteKeepsNoMappedRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.db")
	a, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetMaxOpenConns(1)
	run := func(db *sql.DB, stmts ...string) {
		t.Helper()
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	run(a, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`, `INSERT INTO notes VALUES('n0', 'mapped text')`)
	run(b, `CREATE TEMP TABLE copy AS SELECT * FROM notes`) // b's rows now come from the mapping
	run(a, `INSERT INTO notes VALUES('n1', 'another commit')`)
	run(b, `ALTER TABLE notes RENAME TO memos`, `CREATE TABLE t2(x)`, `INSERT INTO t2 SELECT body FROM temp.copy`)
	var memos, t2 string
	if err := b.QueryRow(`SELECT group_concat(body, ',') FROM (SELECT body FROM memos ORDER BY id)`).Scan(&memos); err != nil {
		t.Fatal(err)
	}
	if err := b.QueryRow(`SELECT group_concat(x) FROM t2`).Scan(&t2); err != nil {
		t.Fatal(err)
	}
	if memos != "mapped text,another commit" || t2 != "mapped text" {
		t.Fatalf("memos %q t2 %q", memos, t2)
	}
}

// TestDropRecreateKeepsNoRows: a table dropped and created again under its
// name in one transaction is a NEW, empty table. The commit's rewrite reloaded
// every table the file holds a table of the same name for, so the dropped
// table's rows came back -- under the new table's column names. C answers an
// empty table.
func TestDropRecreateKeepsNoRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE x(id TEXT PRIMARY KEY, b TEXT)`,
		`INSERT INTO x VALUES('old', 'B')`,
		`BEGIN`,
		`DROP TABLE x`,
		`CREATE TABLE x(id TEXT PRIMARY KEY, a TEXT)`,
		`COMMIT`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, c := range []*sql.DB{db, openFresh(t, p)} {
		var n int
		if err := c.QueryRow(`SELECT count(*) FROM x`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("the recreated table has %d rows, want 0", n)
		}
	}
}

func openFresh(t *testing.T, p string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestFailedCommitLeavesNoTemp: a COMMIT that fails -- another connection
// committed while the transaction was open -- leaves NONE of the transaction
// behind, its temp half included. Temp was made durable before main's commit
// could fail, so the temp rows and tables survived a transaction whose main
// rows were gone.
func TestFailedCommitLeavesNoTemp(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "x.db")
	a := openFresh(t, p)
	b := openFresh(t, p)
	ca, err := a.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	for _, s := range []string{`CREATE TABLE t(v)`, `CREATE TEMP TABLE tl(v)`, `BEGIN`,
		`INSERT INTO tl VALUES('temp')`, `CREATE TEMP TABLE tx(v)`, `INSERT INTO t VALUES('main')`} {
		if _, err := ca.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := b.Exec(`INSERT INTO t VALUES('other')`); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.ExecContext(ctx, `COMMIT`); err == nil {
		t.Fatal("the COMMIT succeeded over another connection's commit")
	}
	var n int
	if err := ca.QueryRowContext(ctx, `SELECT count(*) FROM tl`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the failed transaction's temp row survived (%d rows)", n)
	}
	if _, err := ca.ExecContext(ctx, `CREATE TEMP TABLE tx(v)`); err != nil {
		t.Fatalf("the failed transaction's temp table survived: %v", err)
	}
}

// TestReopenRaceStaysFresh: a commit that lands while a connection's session
// is being rebuilt -- after the rebuild read the file, before it stamped it --
// is seen by that connection's next statement. Stamped after the read, the
// session took that commit for one it had loaded, and the connection answered
// from the older state for as long as nothing else was committed.
func TestReopenRaceStaysFresh(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.db")
	r := openFresh(t, p)
	r.SetMaxOpenConns(1)
	w := openFresh(t, p)
	exec := func(db *sql.DB, s string) {
		t.Helper()
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	count := func() int {
		t.Helper()
		var s string
		if err := r.QueryRow(`SELECT group_concat(v) FROM t`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return len(strings.Split(s, ","))
	}
	exec(r, `CREATE TABLE t(v)`)
	exec(r, `INSERT INTO t VALUES(1)`)
	count()
	exec(w, `INSERT INTO t VALUES(2)`) // r is stale now: its next read rebuilds
	engine.ReopenedHookForTest = func() {
		engine.ReopenedHookForTest = nil
		exec(w, `INSERT INTO t VALUES(3)`) // lands mid-rebuild
	}
	defer func() { engine.ReopenedHookForTest = nil }()
	count()
	if n := count(); n != 3 {
		t.Fatalf("after the commit that raced the rebuild, the connection sees %d rows, want 3", n)
	}
}
