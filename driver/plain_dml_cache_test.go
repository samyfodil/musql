package driver

import (
	"fmt"
	"testing"
)

func TestPlainDMLCacheBounded(t *testing.T) {
	const limit = 256
	var c Conn
	for i := 0; i < 4*limit+1; i++ {
		q := fmt.Sprintf("INSERT INTO t VALUES (%d)", i)
		want := true
		if i%2 != 0 {
			q = fmt.Sprintf("PRAGMA user_version = %d", i)
			want = false
		}
		if got := c.plainDML(q); got != want {
			t.Fatalf("plainDML(%q) = %v, want %v", q, got, want)
		}
		if n := len(c.plainStmts); n > limit {
			t.Fatalf("after %d distinct statements: cached %d, limit %d", i+1, n, limit)
		}
	}

	// Both verdicts must still be correct when an evicted text is seen again.
	for _, tc := range []struct {
		query string
		plain bool
	}{
		{"INSERT INTO t VALUES (0)", true},
		{"PRAGMA user_version = 1", false},
	} {
		if _, ok := c.plainStmts[tc.query]; ok {
			t.Fatalf("old statement still cached: %q", tc.query)
		}
		if got := c.plainDML(tc.query); got != tc.plain {
			t.Fatalf("reclassified %q = %v, want %v", tc.query, got, tc.plain)
		}
	}
}

func TestPlainDMLCacheHitAtCapacity(t *testing.T) {
	const limit = 256
	var c Conn
	for i := 0; i < limit-1; i++ {
		c.plainDML(fmt.Sprintf("INSERT INTO t VALUES (%d)", i))
	}
	c.plainDML("PRAGMA user_version")

	// Alternate true and false hits so each also exercises the map lookup,
	// rather than only the last-text fast path. A hit must not evict anything.
	for _, q := range []string{"INSERT INTO t VALUES (0)", "PRAGMA user_version"} {
		want := q != "PRAGMA user_version"
		for repeat := 0; repeat < 2; repeat++ {
			if got := c.plainDML(q); got != want {
				t.Fatalf("plainDML(%q) = %v, want %v", q, got, want)
			}
			if n := len(c.plainStmts); n != limit {
				t.Fatalf("cache hit changed entry count to %d, want %d", n, limit)
			}
		}
	}
}
