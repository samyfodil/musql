package replication

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestApplyYieldsToWaitingLocalWrite: an apply holding the write lock commits
// what it has soon after a local write starts waiting for the lock, instead of
// holding it as long as it may when nothing waits, and the next apply waits
// for that write to have taken the lock -- for that one only. A local write
// waiting while no apply yielded to it holds no apply off.
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
