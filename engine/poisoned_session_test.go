package engine

import (
	"path/filepath"
	"testing"
)

// TestPoisonedSessionPublishesNothing: neither a commit nor a close publishes
// a failed commit's rows before the rebuild (Poison).
func TestPoisonedSessionPublishesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.musq")
	buildDB(t, path, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT UNIQUE)`, `INSERT INTO t VALUES(1, 'one')`)
	open := func() *Session {
		s, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Discard() })
		return s
	}
	rows := func() string {
		return render(t, open(), `SELECT group_concat(v, ',') FROM (SELECT v FROM t ORDER BY id)`)
	}
	want := render(t, open(), `SELECT 'one'`)
	for _, end := range []string{"commit", "close"} {
		s := open()
		if err := s.Exec(`INSERT INTO t VALUES(2, 'lost')`); err != nil {
			t.Fatal(err)
		}
		s.Poison()
		switch end {
		case "commit":
			if _, err := s.Commit(); err == nil {
				t.Fatal("a poisoned session committed")
			}
		case "close":
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if got := rows(); got != want {
			t.Fatalf("after a poisoned session's %s: %s, want %s", end, got, want)
		}
	}
	s := open()
	if err := s.Exec(`INSERT INTO t VALUES(2, 'lost')`); err != nil {
		t.Fatal(err)
	}
	s.Poison()
	mustCommit(t, s, `INSERT INTO t VALUES(2, 'kept')`)
	if got, want := rows(), render(t, open(), `SELECT 'one,kept'`); got != want {
		t.Fatalf("after the rebuild: %s, want %s", got, want)
	}
}
