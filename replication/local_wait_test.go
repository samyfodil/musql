package replication

import (
	"context"
	"fmt"
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
