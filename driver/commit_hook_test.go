package driver

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestCommitHookThroughDriver: the engine's commit hook sees every commit a
// database/sql connection makes, autocommit and explicit, and none for a
// rolled-back transaction or a read.
func TestCommitHookThroughDriver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.musq")
	var mu sync.Mutex
	var counters []uint32
	engine.Configure(engine.WithCommitHook(func(c engine.CommitInfo) {
		if c.Path != path {
			return
		}
		mu.Lock()
		counters = append(counters, c.Counter)
		mu.Unlock()
	}))
	defer engine.Configure()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`, `SELECT * FROM t`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO t VALUES(2)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO t VALUES(3)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO t VALUES(4)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(counters) != 3 {
		t.Fatalf("hook saw %d commits (%v), want 3", len(counters), counters)
	}
	for i := 1; i < len(counters); i++ {
		if counters[i] <= counters[i-1] {
			t.Errorf("counters not increasing: %v", counters)
		}
	}
}
