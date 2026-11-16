package replication

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestQuorumEntryAppliesWhole: a transaction committed through the Quorum
// writes two tables, and no reader on either node sees one row without the
// other. Apply ingested an entry's ops one at a time, a transaction each, so a
// reader between them saw the first table's row alone.
func TestQuorumEntryAppliesWhole(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	var lg fakeLog
	yes, no := func() bool { return true }, func() bool { return false }
	open := func(name string, writer func() bool) *sql.DB {
		p := filepath.Join(dir, name+".db")
		createSchema(t, p, `CREATE TABLE a(id INTEGER PRIMARY KEY, v)`, `CREATE TABLE b(id INTEGER PRIMARY KEY, v)`)
		db, err := Open(ctx, p, Leader(writer, lg.quorum()), WithSite(name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	w, f := open("w", yes), open("f", no)

	var stop atomic.Bool
	var torn atomic.Int64
	var wg sync.WaitGroup
	for _, db := range []*sql.DB{w, f} {
		wg.Go(func() {
			for !stop.Load() {
				var d int
				if err := db.QueryRow(`SELECT (SELECT count(*) FROM a) - (SELECT count(*) FROM b)`).Scan(&d); err != nil {
					t.Error(err)
					return
				}
				if d != 0 {
					torn.Add(1)
				}
			}
		})
	}
	for i := range 300 {
		tx, err := w.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{`INSERT INTO a VALUES (?, 'x')`, `INSERT INTO b VALUES (?, 'y')`} {
			if _, err := tx.Exec(s, i); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if n := torn.Load(); n > 0 {
		t.Fatalf("readers saw a transaction half-applied %d times", n)
	}
	for name, db := range map[string]*sql.DB{"w": w, "f": f} {
		if got := dump(t, db, `SELECT count(*), (SELECT count(*) FROM b) FROM a`); got != fmt.Sprintf("%d|%d\n", 300, 300) {
			t.Fatalf("%s: %q", name, got)
		}
	}
}
