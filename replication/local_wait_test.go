package replication

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestApplyYieldsToWaitingLocalWrite: an apply commits soon after a local
// write starts waiting, and the next apply waits for that write alone.
func TestApplyYieldsToWaitingLocalWrite(t *testing.T) {
	s := newTestStore(t, "localyield")
	waitLocal := func() <-chan struct{} {
		done := make(chan struct{})
		go func() {
			s.waitLocal()
			close(done)
		}()
		return done
	}

	s.localLockWait(true)
	select {
	case <-waitLocal():
	case <-time.After(time.Second):
		t.Fatal("an apply waited for a local write it had not yielded to")
	}
	s.localLockWait(false)

	var ops []Op
	for i := range 20000 {
		ops = append(ops, insertOp("A", uint64(i+1), HLC(100+i), fmt.Sprintf("u%05d", i), "n", int64(i)))
	}
	type result struct {
		n   int
		err error
		at  time.Time
	}
	res := make(chan result, 1)
	go func() {
		n, err := s.ingest(context.Background(), ops, false)
		res <- result{n, err, time.Now()}
	}()
	for s.mu.TryLock() {
		s.mu.Unlock()
		runtime.Gosched()
	}
	asked := time.Now()
	s.localLockWait(true)
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.n == len(ops) {
		t.Skip("the apply took every op before the local write waited")
	}
	if took := r.at.Sub(asked); took >= holdAlone/2 {
		t.Fatalf("the apply held the lock %v after a local write began waiting for it", took)
	}

	done := waitLocal()
	select {
	case <-done:
		t.Fatal("the next apply went ahead of the local write the last one yielded to")
	case <-time.After(50 * time.Millisecond):
	}
	s.localLockWait(true)
	s.localLockWait(false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the next apply still waits after the local write it yielded to took the lock")
	}
	s.localLockWait(false)
}

// TestWaitingIngestTakesTheLockFirst: a local write that finds an ingest
// waiting for the lock lets it in first, and reads what it applied.
func TestWaitingIngestTakesTheLockFirst(t *testing.T) {
	const rounds = 3
	ctx := context.Background()
	src, _ := benchSource(t, rounds)
	ops, err := src.store.OpsSince(ctx, src.site, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, db, err := openSyncer(ctx, filepath.Join(t.TempDir(), "dst.musq"), CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := len(ops) - rounds
	if k, err := s.store.ingestAll(ctx, ops[:first]); err != nil {
		t.Fatalf("refused at %d: %v", k, err)
	}
	if _, err := db.Exec(`CREATE TABLE l(n)`); err != nil {
		t.Fatal(err)
	}
	waitFor := func(c *atomic.Int32) {
		for deadline := time.Now().Add(5 * time.Second); c.Load() == 0 && time.Now().Before(deadline); {
			time.Sleep(100 * time.Microsecond)
		}
	}
	for r, op := range ops[first:] {
		gate, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gate.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		ingested := make(chan error, 1)
		go func() { ingested <- s.store.Ingest(ctx, op, false) }()
		waitFor(&s.store.ingestWaiting)
		wrote := make(chan error, 1)
		go func() {
			_, err := db.Exec(`INSERT INTO l SELECT count(*) FROM t`)
			wrote <- err
		}()
		waitFor(&s.store.localWaiting)
		if _, err := gate.ExecContext(ctx, `COMMIT`); err != nil {
			t.Fatal(err)
		}
		gate.Close()
		if err := <-ingested; err != nil {
			t.Fatal(err)
		}
		if err := <-wrote; err != nil {
			t.Fatal(err)
		}
		var n, want int
		if err := db.QueryRow(`SELECT (SELECT n FROM l ORDER BY rowid DESC LIMIT 1), (SELECT count(*) FROM t)`).Scan(&n, &want); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("round %d: the local write read %d rows of t, want %d: it took the lock ahead of the ingest waiting for it", r, n, want)
		}
	}
}

// TestLocalCommitDoesNotWaitForAnApply: once a local write has committed, its
// Exec returns without waiting for the store, which an apply holds through
// its whole transaction; its seq still reaches the version vector.
func TestLocalCommitDoesNotWaitForAnApply(t *testing.T) {
	ctx := context.Background()
	s, db, err := openSyncer(ctx, filepath.Join(t.TempDir(), "a.musq"), CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`); err != nil {
		t.Fatal(err)
	}
	before := s.store.LocalVV()[s.store.Site()]
	s.store.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := db.Exec(`INSERT INTO t(v) VALUES(1)`)
		done <- err
	}()
	select {
	case err = <-done:
		s.store.mu.Unlock()
	case <-time.After(5 * time.Second):
		s.store.mu.Unlock()
		<-done
		t.Fatal("a local commit waited for an apply's transaction to end")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := s.store.LocalVV()[s.store.Site()]; got != before+1 {
		t.Fatalf("own seq %d in the version vector, want %d", got, before+1)
	}
}
