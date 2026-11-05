package replication

import (
	"database/sql"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestQuorumRowidsAreTheTables: with a Quorum, rowids are assigned as on a
// single node, 1, 2, 3, and a keyless table keeps the rowids it had before
// replication. A writer that takes over goes on above every rowid the table
// ever had, the old writer's deleted one included, which it learned from the
// log alone -- in a table the log created too -- and so does a writer that
// reopens its database, from its clock.
func TestQuorumRowidsAreTheTables(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	var lg fakeLog
	var aWrites atomic.Bool
	aWrites.Store(true)
	open := func(name string, writer func() bool, ddl ...string) *sql.DB {
		p := filepath.Join(dir, name+".db")
		createSchema(t, p, ddl...)
		db, err := Open(ctx, p, Leader(writer, lg.quorum()), WithSite(name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	bWrites := func() bool { return !aWrites.Load() }
	schema := []string{`CREATE TABLE k(v)`, `CREATE TABLE t(id INTEGER PRIMARY KEY, v)`}
	a := open("a", aWrites.Load, append(schema, `INSERT INTO k(rowid, v) VALUES (5, 'five'), (9, 'nine')`)...)
	b := open("b", bWrites, schema...)

	if got := dump(t, a, `SELECT rowid, v FROM k`); got != "5|five\n9|nine\n" {
		t.Fatalf("a keyless table after genesis: %q", got)
	}
	for _, s := range []string{`INSERT INTO k(v) VALUES ('ten')`, `INSERT INTO t(v) VALUES ('one'), ('two')`, `DELETE FROM t WHERE id = 2`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, v)`, `INSERT INTO u(v) VALUES ('one'), ('two')`, `DELETE FROM u WHERE id = 2`} {
		if _, err := a.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := dump(t, a, `SELECT rowid FROM k WHERE v = 'ten'`); got != "10\n" {
		t.Fatalf("the next keyless rowid: %q", got)
	}

	aWrites.Store(false)
	for _, s := range []string{`INSERT INTO u(v) VALUES ('three')`, `DELETE FROM u WHERE id = 3`, `INSERT INTO t(v) VALUES ('three')`} {
		if _, err := b.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for name, db := range map[string]*sql.DB{"a": a, "b": b} {
		if got := dump(t, db, `SELECT id, v FROM t`); got != "1|one\n3|three\n" {
			t.Fatalf("%s after the new writer's insert: %q", name, got)
		}
		if got := dump(t, db, `SELECT id, v FROM u`); got != "1|one\n" {
			t.Fatalf("%s: u after the new writer's insert and delete: %q", name, got)
		}
	}

	// b reopens: its floor comes from its clock, tombstones included.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	lg.nodes = lg.nodes[:1]
	b = open("b", bWrites)
	if _, err := b.Exec(`INSERT INTO u(v) VALUES ('four')`); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, b, `SELECT id, v FROM u`); got != "1|one\n4|four\n" {
		t.Fatalf("u after b reopened: %q", got)
	}
}
