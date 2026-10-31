package engine

import (
	"path/filepath"
	"testing"
)

// TestPoisonedSessionPublishesNothing: a session whose commit failed keeps
// the statement's rows until its rebuild (Poison), and neither a commit nor a
// close before that rebuild may publish them. A replicated writer's refused
// hand-off left its row there, the connection pool closed the connection, and
// the close committed the row: durable, with no op, and the caller's retry
// failed UNIQUE.
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
