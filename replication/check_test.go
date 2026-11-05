package replication

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheck: Check names what Open would refuse, without writing to the
// file, and Replicated tells a file Open has taken from one it has not.
func TestCheck(t *testing.T) {
	ctx := context.Background()
	mk := func(stmts ...string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "c.musq")
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(s, err)
			}
		}
		return p
	}
	ok := mk(`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`, `CREATE INDEX t_v ON t(v)`, `CREATE TABLE u(a TEXT UNIQUE)`, `INSERT INTO t(v) VALUES (1)`)
	if err := Check(ctx, ok); err != nil {
		t.Fatalf("an ordinary database: %v", err)
	}
	for _, c := range []struct{ stmt, why string }{
		{`CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`, "triggers"},
		{`CREATE VIRTUAL TABLE f USING fts4(body)`, "virtual tables"},
		{`CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, "WITHOUT ROWID"},
	} {
		p := mk(`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`, c.stmt)
		if err := Check(ctx, p); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want one naming %s", c.stmt, err, c.why)
		}
		if r, err := Replicated(ctx, p); err != nil || r {
			t.Errorf("%s: checking it made it replicated: %v %v", c.stmt, r, err)
		}
	}
	if r, err := Replicated(ctx, ok); err != nil || r {
		t.Fatalf("an ordinary database is replicated: %v %v", r, err)
	}
	db, err := Open(ctx, ok, Leader(func() bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if r, err := Replicated(ctx, ok); err != nil || !r {
		t.Fatalf("a database Open took is not replicated: %v %v", r, err)
	}
	if err := Check(ctx, ok); err != nil {
		t.Fatalf("a replicated database: %v", err)
	}
}
