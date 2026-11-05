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
// log alone.
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
	schema := []string{`CREATE TABLE k(v)`, `CREATE TABLE t(id INTEGER PRIMARY KEY, v)`}
	a := open("a", aWrites.Load, append(schema, `INSERT INTO k(rowid, v) VALUES (5, 'five'), (9, 'nine')`)...)
	b := open("b", func() bool { return !aWrites.Load() }, schema...)

	if got := dump(t, a, `SELECT rowid, v FROM k`); got != "5|five\n9|nine\n" {
		t.Fatalf("a keyless table after genesis: %q", got)
	}
	for _, s := range []string{`INSERT INTO k(v) VALUES ('ten')`, `INSERT INTO t(v) VALUES ('one'), ('two')`, `DELETE FROM t WHERE id = 2`} {
		if _, err := a.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := dump(t, a, `SELECT rowid FROM k WHERE v = 'ten'`); got != "10\n" {
		t.Fatalf("the next keyless rowid: %q", got)
	}

	aWrites.Store(false)
	if _, err := b.Exec(`INSERT INTO t(v) VALUES ('three')`); err != nil {
		t.Fatal(err)
	}
	for name, db := range map[string]*sql.DB{"a": a, "b": b} {
		if got := dump(t, db, `SELECT id, v FROM t`); got != "1|one\n3|three\n" {
			t.Fatalf("%s after the new writer's insert: %q", name, got)
		}
	}
}
