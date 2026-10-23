package engine

import (
	"path/filepath"
	"testing"
)

// TestCommitHookReportsCommittedState: every commit, a delta append or a
// rewrite, reports the state CommitState reads back from the files.
func TestCommitHookReportsCommittedState(t *testing.T) {
	var got []CommitInfo
	Configure(WithCommitHook(func(c CommitInfo) { got = append(got, c) }))
	defer Configure()
	path := filepath.Join(t.TempDir(), "h.musq")
	s, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1, 'x')`,
		`UPDATE t SET b = 'y'`,
		`CREATE INDEX tb ON t(b)`,
		`DELETE FROM t`,
	} {
		n := len(got)
		if err := s.Exec(q); err != nil {
			t.Fatal(q, err)
		}
		if _, err := s.Commit(); err != nil {
			t.Fatal(q, err)
		}
		if len(got) != n+1 {
			t.Fatalf("%s: %d hook calls, want 1", q, len(got)-n)
		}
		var st CommitInfo
		if err := CommitState(path, func(c CommitInfo) error { st = c; return nil }); err != nil {
			t.Fatal(err)
		}
		if h := got[len(got)-1]; h != st {
			t.Errorf("%s: hook %+v, files %+v", q, h, st)
		}
	}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("a commit with nothing to write called the hook: %d calls", len(got))
	}
}
