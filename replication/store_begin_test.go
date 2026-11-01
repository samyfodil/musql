package replication

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestBeginWaitsForTheWriteLock: a store transaction waits while another
// connection holds the write lock and takes it once it is let go; one whose
// context is done gives up after its first attempt's busy wait.
func TestBeginWaitsForTheWriteLock(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	c.openWith("a", []Option{WithSyncInterval(time.Hour)}, `CREATE TABLE t(id INTEGER PRIMARY KEY)`)
	st := syncerOf(t, c.dbs["a"]).store
	other, err := sql.Open("sqlite", filepath.Join(c.dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	holder, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.begin(cancelled); err == nil {
		t.Fatal("a done context took the lock")
	}
	got := make(chan error, 1)
	go func() {
		tx, err := st.begin(ctx)
		if err == nil {
			tx.ExecContext(ctx, `ROLLBACK`)
			tx.Close()
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-got:
		t.Fatalf("took the lock another connection holds: %v", err)
	default:
	}
	if _, err := holder.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal(errors.New("never took the lock once it was let go"))
	}
}
