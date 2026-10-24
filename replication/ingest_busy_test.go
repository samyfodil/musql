package replication

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Applying a peer's ops while a local writer commits loses neither: an apply
// used to begin deferred, fail with SQLITE_BUSY whenever a local commit landed
// meanwhile, and leave the op to the next anti-entropy round.
func TestIngestDoesNotLoseToLocalWriters(t *testing.T) {
	const n = 500
	src, _ := benchSource(t, n)
	ops, err := src.store.OpsSince(context.Background(), src.site, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, db, err := openSyncer(context.Background(), filepath.Join(t.TempDir(), "dst.musq"), CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE l(id INTEGER PRIMARY KEY, v)`); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var local atomic.Int64
	// One writer: two would also lose to each other, which is not an apply's
	// doing.
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.Exec(`INSERT INTO l(v) VALUES(?)`, time.Now().UnixNano()); err != nil {
				t.Errorf("local write: %v", err)
				return
			}
			local.Add(1)
			time.Sleep(time.Millisecond)
		}
	})
	for lo := 0; lo < len(ops); lo += 64 {
		page := ops[lo:min(lo+64, len(ops))]
		if k, err := s.store.ingestAll(context.Background(), page); err != nil {
			t.Errorf("ops %d..%d: refused at %d: %v", lo, lo+len(page), k, err)
		}
		for _, op := range page {
			if err := s.store.Ingest(context.Background(), op, false); err != nil {
				t.Errorf("op %d again: %v", op.Seq, err)
			}
		}
	}
	close(stop)
	wg.Wait()
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&rows); err != nil || rows != n {
		t.Fatalf("count(*) = %d, %v; want %d", rows, err, n)
	}
	if local.Load() == 0 {
		t.Fatal("no local write committed")
	}
}
