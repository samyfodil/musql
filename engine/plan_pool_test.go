package engine

import (
	"path/filepath"
	"testing"
	"time"
)

// TestPlanPoolHandsOnAClosedSessionsPlans: a session opened after another
// closed on the same database takes over its plans, which answer from the
// data as it is and are dropped once the schema moves.
func TestPlanPoolHandsOnAClosedSessionsPlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.musq")
	open := func() *Session {
		s, err := OpenOrCreateWait(path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	exec := func(s *Session, q string) {
		if _, _, err := s.ExecArgs(q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const q = "SELECT sum(a) FROM t"
	sum := func(s *Session) int64 {
		_, rows, err := s.Query(q, nil)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: %v %v", q, rows, err)
		}
		return rows[0][0].I
	}
	cached := func(s *Session) bool {
		rp, err := s.ReadPager()
		if err != nil {
			t.Fatal(err)
		}
		defer rp.Close()
		return rp.cachedPlan(q) != nil
	}

	s := open()
	exec(s, "CREATE TABLE t(a)")
	exec(s, "INSERT INTO t VALUES (1), (2)")
	if got := sum(s); got != 3 {
		t.Fatalf("sum = %d, want 3", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open()
	if !cached(s) {
		t.Fatal("the new session did not take over the closed one's plan")
	}
	exec(s, "INSERT INTO t VALUES (4)")
	if got := sum(s); got != 7 {
		t.Fatalf("after an insert, sum = %d, want 7", got)
	}
	exec(s, "CREATE TABLE u(b)")
	if cached(s) {
		t.Fatal("a plan survived a schema change")
	}
	if got := sum(s); got != 7 {
		t.Fatalf("after a schema change, sum = %d, want 7", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if takePooledPlans(path) == nil {
		t.Fatal("a closed session left no plans")
	}
	if takePooledPlans(path) != nil {
		t.Fatal("the pool handed the same plans out twice")
	}
}
