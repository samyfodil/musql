package replication

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlainCopy: a plain copy of a live replicated database holds its schema
// and rows and nothing of replication -- a user table named like the
// bookkeeping stays, a withheld row is in no table and so not in the copy --
// any connection writes it, and opened through replication it is a new
// database whose rows are its site's first ops. The source is untouched.
func TestPlainCopy(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	c.open("a",
		`CREATE TABLE u(id TEXT PRIMARY KEY, email TEXT UNIQUE)`,
		`CREATE TABLE "_repl_x"(a)`,
		`CREATE TABLE k(a, b)`,
		`CREATE TABLE ai(id INTEGER PRIMARY KEY AUTOINCREMENT, v)`,
		`INSERT INTO k VALUES(1, x'00'), (2.5, 'two')`,
		`INSERT INTO "_repl_x" VALUES('mine')`,
		`INSERT INTO ai(v) VALUES('one')`,
		`PRAGMA user_version = 7`)
	c.open("b")
	const q = `SELECT id, email FROM u ORDER BY id`
	c.converge("", `SELECT count(*) FROM k`)
	c.partition(true)
	c.exec("a", `INSERT INTO u VALUES('a1', 'x@')`)
	c.exec("b", `INSERT INTO u VALUES('b1', 'x@')`)
	c.partition(false)
	c.converge("b1|x@\n", q)
	if held, err := Withheld(ctx, c.dbs["a"]); err != nil || len(held) != 1 {
		t.Fatalf("withheld: %v %v", held, err)
	}

	tables := `SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name`
	rows := []string{q, `SELECT rowid, a, typeof(a), b, typeof(b) FROM k ORDER BY rowid`, `SELECT * FROM "_repl_x"`, `SELECT * FROM ai`}
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := PlainCopy(ctx, filepath.Join(c.dir, "a.db"), dst); err != nil {
		t.Fatal(err)
	}
	if err := PlainCopy(ctx, filepath.Join(c.dir, "a.db"), dst); err == nil {
		t.Fatal("a copy over an existing file")
	}
	plain, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if got := dump(t, plain, tables); got != "_repl_x\nai\nk\nsqlite_sequence\nu\n" {
		t.Fatalf("the copy's tables:\n%s", got)
	}
	for _, r := range rows {
		if got, want := dump(t, plain, r), dump(t, c.dbs["a"], r); got != want {
			t.Fatalf("%s: the copy has\n%s\nthe source\n%s", r, got, want)
		}
	}
	if got := dump(t, plain, `PRAGMA user_version`); got != "7\n" {
		t.Fatalf("user_version %q", got)
	}
	for _, s := range []string{`INSERT INTO u VALUES('p1', 'p@')`, `DELETE FROM k WHERE a = 1`, `CREATE TABLE z(a)`} {
		if _, err := plain.Exec(s); err != nil {
			t.Fatalf("%s on the copy: %v", s, err)
		}
	}
	plain.Close()

	// The source still replicates.
	c.exec("a", `INSERT INTO k VALUES(3, 'three')`)
	c.converge("3\n", `SELECT count(*) FROM k`)

	// Opened through replication, the copy is a database of its own.
	c2 := newCluster(t)
	if err := PlainCopy(ctx, filepath.Join(c.dir, "a.db"), filepath.Join(c2.dir, "x.db")); err != nil {
		t.Fatal(err)
	}
	c2.open("x")
	c2.open("y")
	got := c2.converge("", append([]string{tables}, rows...)...)
	if !strings.Contains(got[0], "_repl_meta") || got[1] != "b1|x@\n" || !strings.Contains(got[2], "three") {
		t.Fatalf("the copy's own replication: %q", got)
	}
}
