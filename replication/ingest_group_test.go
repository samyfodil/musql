package replication

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestIngestGroupsConcurrentCalls: Ingest calls that arrive while one applies
// share the next transaction, and each still gets its own op's result: every
// op lands, and the one refused -- a second, different op at a seq its site
// already used -- is refused alone.
func TestIngestGroupsConcurrentCalls(t *testing.T) {
	s := newTestStore(t, "group")
	ctx := context.Background()
	var ops []Op
	for i := range 200 {
		ops = append(ops, insertOp("A", uint64(i+1), HLC(100+i), fmt.Sprintf("u%03d", i), "n", int64(i)))
	}
	if err := s.Ingest(ctx, ops[0], false); err != nil {
		t.Fatal(err)
	}
	commits := s.applied.Load()
	bad := insertOp("A", 1, 999, "other", "x", 1)
	errs := make([]error, len(ops))
	var badErr error
	var wg sync.WaitGroup
	for i := 1; i < len(ops); i++ {
		wg.Go(func() { errs[i] = s.Ingest(ctx, ops[i], false) })
		if i == len(ops)/2 {
			wg.Go(func() { badErr = s.Ingest(ctx, bad, false) })
		}
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}
	if badErr == nil || !strings.Contains(badErr.Error(), "two different ops") {
		t.Fatalf("a second op at a seq already used: %v, want it refused", badErr)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil || n != len(ops) {
		t.Fatalf("users = %d, %v; want %d", n, err, len(ops))
	}
	if got := s.applied.Load() - commits; got >= uint64(len(ops)/2) {
		t.Fatalf("%d commits for %d ops arriving together, want them grouped", got, len(ops)-1)
	}
}
