package replication

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/samyfodil/musql/boost"
)

// TestBoostCaptures: a boost.DB onto a replicated database writes through
// connections set up as the *sql.DB's are, so its writes, an Exec's and a
// transaction's, reach the peer and WithMaxSize holds on them, while a bare
// boost.Open is still refused writes by the capture guard.
func TestBoostCaptures(t *testing.T) {
	ctx := context.Background()
	net := NewMemNetwork()
	dir := t.TempDir()
	pa, pb := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	createSchema(t, pa, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT NOT NULL)`)
	createSchema(t, pb, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT NOT NULL)`)
	a, err := Open(ctx, pa, CRDT(), memSync(net), WithSyncInterval(20*time.Millisecond), WithMaxSize(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, pb, CRDT(), memSync(net), WithSyncInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	fast, err := Boost(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fast.Exec(ctx, `INSERT INTO notes VALUES('n1', 'exec')`); err != nil {
		t.Fatal(err)
	}
	tx, err := fast.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO notes VALUES('n2', 'tx')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var size int64
	if err := fast.QueryRow(ctx, `PRAGMA max_size`).Scan(&size); err != nil || size != 1<<30 {
		t.Fatalf("PRAGMA max_size on a boost connection = %d, %v; want %d", size, err, 1<<30)
	}
	const q = `SELECT id, body FROM notes ORDER BY id`
	for deadline := time.Now().Add(10 * time.Second); dump(t, b, q) != "n1|exec\nn2|tx\n"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the boost.DB's writes did not reach the peer: %q", dump(t, b, q))
		}
	}

	bare, err := boost.Open(pa)
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	if _, err := bare.Exec(ctx, `INSERT INTO notes VALUES('n3', 'bare')`); err == nil {
		t.Fatal("a bare boost.Open wrote to a replicated database")
	}

	// Closing the boost.DB leaves the replication running.
	if err := fast.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(`INSERT INTO notes VALUES('n4', 'after')`); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); dump(t, b, `SELECT count(*) FROM notes`) != "3\n"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("sync stopped with the boost.DB: %q", dump(t, b, q))
		}
	}
	if _, err := Boost(&sql.DB{}); err == nil {
		t.Fatal("Boost took a *sql.DB Open did not return")
	}
}
