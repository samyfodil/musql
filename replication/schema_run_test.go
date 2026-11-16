package replication

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaRunInstallsAsOneOpAtATime: a peer's ops ingested in a batch, its
// schema ops in a row installed together, leave the same catalog, rows and
// clock as the same ops ingested one at a time. Both sides wrote a schema
// apart, one table defined differently on each, so the reducer has a
// conflict to settle, and the peer's ops sort before this node's own. A
// schema run installed wrong is refused (checkInstalled) and the ops apply
// again one at a time, so the batch must also have taken one transaction.
func TestSchemaRunInstallsAsOneOpAtATime(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	build := func(name string, stmts ...string) {
		_, db, err := openSyncer(ctx, filepath.Join(dir, name+".db"), CRDT(), WithSite(name))
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range stmts {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", name, q, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	build("a",
		`CREATE TABLE u(id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE INDEX u_name ON u(name)`,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT)`,
		`INSERT INTO u(name) VALUES ('ann'), ('bo')`,
		`CREATE TABLE s(k TEXT PRIMARY KEY, v INT)`,
		`CREATE INDEX s_v ON s(v)`,
		`ALTER TABLE u ADD COLUMN age INT`,
		`INSERT INTO s VALUES ('x', 1), ('y', 2)`,
		`INSERT INTO t(a) VALUES ('ta')`,
	)
	build("b",
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b INT)`,
		`CREATE TABLE r(id INTEGER PRIMARY KEY, w TEXT)`,
		`INSERT INTO t(a, b) VALUES ('tb', 7)`,
		`INSERT INTO r(w) VALUES ('rw')`,
	)
	a, adb, err := openSyncer(ctx, filepath.Join(dir, "a.db"), CRDT(), WithSite("a"))
	if err != nil {
		t.Fatal(err)
	}
	ops, err := a.store.OpsSince(ctx, "a", 0, 0)
	adb.Close()
	if err != nil {
		t.Fatal(err)
	}
	ingested := func(name string, ingest func(*Syncer)) string {
		sub := filepath.Join(dir, name)
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		copyFiles(t, dir, sub, "b.db")
		s, db, err := openSyncer(ctx, filepath.Join(sub, "b.db"), CRDT(), WithSite("b"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ingest(s)
		return dumpSchemaAndRows(t, db) + dumpClockDB(t, db)
	}
	batch := ingested("batch", func(s *Syncer) {
		before := s.store.ingestGot.Load()
		if k, err := s.store.ingestAll(ctx, ops); err != nil {
			t.Fatalf("batch: op %d: %v", k, err)
		}
		// One transaction: a run refused rolls back and its ops apply again
		// an op at a time, in a second one.
		if n := s.store.ingestGot.Load() - before; n != 1 {
			t.Fatalf("the batch took %d transactions, want 1", n)
		}
	})
	single := ingested("single", func(s *Syncer) {
		for _, op := range ops {
			if err := s.store.Ingest(context.Background(), op, false); err != nil {
				t.Fatalf("one at a time: %s/%d: %v", op.Site, op.Seq, err)
			}
		}
	})
	if batch != single {
		t.Fatalf("a batch left:\n%s\none op at a time left:\n%s", batch, single)
	}
	for _, want := range []string{"ann", "ta", "rw"} {
		if !strings.Contains(batch, want) {
			t.Fatalf("%q is in no table:\n%s", want, batch)
		}
	}
}

// dumpSchemaAndRows is db's user catalog and every table's rows.
func dumpSchemaAndRows(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	var tables []string
	rows, err := db.Query(catalogQuery)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, name, tbl string
		var q sql.NullString
		if err := rows.Scan(&typ, &name, &tbl, &q); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %s %s %s\n", typ, name, tbl, q.String)
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	rows.Close()
	for _, tbl := range tables {
		fmt.Fprintf(&b, "-- %s\n%s", tbl, dump(t, db, `SELECT * FROM `+q(tbl)+` ORDER BY 1`))
	}
	return b.String()
}
