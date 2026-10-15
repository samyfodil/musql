package fsio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The retry loop itself, with the platform's verdict replaced: it is what runs
// on Windows, and this runs it everywhere.
func TestRenameWithRetriesOnlyTransientFailures(t *testing.T) {
	transient := errors.New("transient")
	retryable := renameRetryableFor(transient)
	defer retryable()

	calls := 0
	var slept time.Duration
	rename := func(string, string) error {
		calls++
		if calls < 4 {
			return transient
		}
		return nil
	}
	if err := renameWith(rename, "a", "b", time.Second, func(d time.Duration) { slept += d }); err != nil {
		t.Fatalf("a rename that succeeds on its fourth try: %v", err)
	}
	if calls != 4 || slept == 0 {
		t.Fatalf("calls=%d slept=%v, want 4 calls with waits between", calls, slept)
	}

	// Past the limit, the failure is the answer.
	calls = 0
	always := func(string, string) error { calls++; return transient }
	if err := renameWith(always, "a", "b", 10*time.Millisecond, func(time.Duration) {}); !errors.Is(err, transient) {
		t.Fatalf("got %v, want the transient error once the limit passed", err)
	}
	if calls < 2 {
		t.Fatalf("calls=%d: it gave up without retrying", calls)
	}

	// Any other failure is returned at once.
	other := errors.New("other")
	calls = 0
	if err := renameWith(func(string, string) error { calls++; return other }, "a", "b", time.Second, func(time.Duration) {}); !errors.Is(err, other) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want the error after one call", err, calls)
	}
}

func TestRenameReplaces(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "new"), filepath.Join(dir, "db")
	if err := os.WriteFile(to, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(from, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(to); err != nil || string(b) != "new" {
		t.Fatalf("%q, %v", b, err)
	}
}

// renameRetryableFor makes err, and only err, retryable until the returned
// func restores the platform's verdict.
func renameRetryableFor(err error) func() {
	saved := renameRetryable
	renameRetryable = func(e error) bool { return errors.Is(e, err) }
	return func() { renameRetryable = saved }
}
