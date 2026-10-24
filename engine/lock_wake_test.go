package engine

import (
	"path/filepath"
	"testing"
	"time"
)

// TestLockWaiterWakesOnRelease: a connection waiting for a lock another
// connection of this process holds takes it when that one lets go, not at its
// next poll -- with the poll an hour away, only the release can wake it.
func TestLockWaiterWakesOnRelease(t *testing.T) {
	defer func(d time.Duration) { busyRetryInterval = d }(busyRetryInterval)
	busyRetryInterval = time.Hour
	path := filepath.Join(t.TempDir(), "w.musq")
	holder, err := openSegmentLockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	waiter, err := openSegmentLockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	if err := acquireLock(holder, segStateByte, 2, true, time.Second); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { got <- acquireLock(waiter, segStateByte, 2, true, time.Minute) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-got:
		t.Fatalf("the waiter took a lock still held: %v", err)
	default:
	}
	if err := releaseLock(holder, segStateByte, 2); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the release did not wake the waiter")
	}
}
