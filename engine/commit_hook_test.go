package engine

import (
	"os"
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

// TestReplaceStateIsSeenByHeldSession: a copy kept by writing its files
// directly -- the bytes another file's commits appended, then a new segment
// file -- is read by a session already open on it once it checks for change, as the
// driver does before each statement.
func TestReplaceStateIsSeenByHeldSession(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.musq"), filepath.Join(dir, "dst.musq")
	w, err := Create(src)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	run := func(q string) {
		t.Helper()
		if err := w.Exec(q); err != nil {
			t.Fatal(q, err)
		}
		if _, err := w.Commit(); err != nil {
			t.Fatal(q, err)
		}
	}
	state := func() CommitInfo {
		t.Helper()
		var c CommitInfo
		if err := CommitState(src, func(i CommitInfo) error { c = i; return nil }); err != nil {
			t.Fatal(err)
		}
		return c
	}
	copyRange := func(from, to string, off, end int64) {
		t.Helper()
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(to, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteAt(b[off:end], off); err != nil {
			t.Fatal(err)
		}
	}
	run(`CREATE TABLE t(a INTEGER PRIMARY KEY)`)
	run(`INSERT INTO t VALUES(1)`)
	c := state()
	copyRange(src, dst, 0, fileLen(t, src))
	copyRange(src+".delta", dst+".delta", 0, c.DeltaEnd)
	r, err := OpenWrite(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	count := func() int64 {
		t.Helper()
		if _, err := r.RefreshIfStale(); err != nil {
			t.Fatal(err)
		}
		_, rows, err := r.Query(`SELECT count(*) FROM t`, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rows[0][0].I
	}
	if n := count(); n != 1 {
		t.Fatalf("count %d, want 1", n)
	}
	run(`INSERT INTO t VALUES(2)`)
	c2 := state()
	if err := ReplaceState(dst, func() error {
		copyRange(src+".delta", dst+".delta", c.DeltaEnd, c2.DeltaEnd)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("after an appended batch: count %d, want 2", n)
	}
	run(`VACUUM`)
	run(`INSERT INTO t VALUES(3)`)
	c3 := state()
	if err := ReplaceState(dst, func() error {
		os.Remove(dst + ".delta")
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst+".tmp", b, 0o644); err != nil {
			return err
		}
		if err := os.Rename(dst+".tmp", dst); err != nil {
			return err
		}
		if c3.DeltaEnd > 0 {
			copyRange(src+".delta", dst+".delta", 0, c3.DeltaEnd)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 3 {
		t.Fatalf("after a new segment file: count %d, want 3", n)
	}
}

func fileLen(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
