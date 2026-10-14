package engine

import (
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/internal/mmapfile"
)

// A held session trusts the lock file's change word between statements
// (segment_change.go) only while it is the even value its last full check saw.
// Every way another connection can move the pair must move the word: a delta
// append, a rewrite (DDL), a compaction, and a VACUUM.
func TestChangeWordSeesEveryChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.musq")
	a, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	b, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// Where files are read rather than mapped there is no word to trust, and
	// every check is a full one; the counts below must hold either way.
	if b.change == nil && mmapfile.CanMapAt {
		t.Fatal("no change word mapped on a version-6 file")
	}
	count := func(s *Session) int64 {
		t.Helper()
		if _, err := s.RefreshIfStale(); err != nil {
			t.Fatal(err)
		}
		_, rows, err := s.Query("SELECT count(*) FROM t", nil)
		if err != nil {
			t.Fatal(err)
		}
		return rows[0][0].I
	}
	if n := count(b); n != 0 {
		t.Fatalf("count %d", n)
	}
	count(b) // a second statement with nothing changed: the word answers
	if b.change != nil && !b.changeSeenOK {
		t.Fatal("the session did not record the word after a full check")
	}
	n := int64(0)
	for _, step := range []string{
		"INSERT INTO t VALUES (1)",           // a delta append
		"CREATE TABLE u(y)",                  // a rewrite
		"INSERT INTO t VALUES (2)",           // another append
		"VACUUM",                             // a rewrite asked for
		"INSERT INTO t SELECT x + 10 FROM t", // an append after it
	} {
		if err := a.Exec(step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if _, err := a.Commit(); err != nil {
			t.Fatal(err)
		}
		_, rows, err := a.Query("SELECT count(*) FROM t", nil)
		if err != nil {
			t.Fatal(err)
		}
		n = rows[0][0].I
		if got := count(b); got != n {
			t.Fatalf("after %q the other session counts %d, want %d", step, got, n)
		}
	}
	// A compaction from outside any session.
	if err := a.Exec("INSERT INTO t VALUES (99)"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := CompactSegmentFile(path); err != nil {
		t.Fatal(err)
	}
	if got := count(b); got != n+1 {
		t.Fatalf("after a compaction the other session counts %d, want %d", got, n+1)
	}
}

// An odd word is a change in progress, or one whose writer died before it
// stored the even one: a session must check in full while it stands, and
// never record it.
func TestChangeWordOddIsNeverTrusted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.musq")
	a, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	b, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	if err := storeChangeWord(path, nil, 0x1235); err != nil { // a writer that began
		t.Fatal(err)
	}
	if _, err := b.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	if b.changeSeenOK {
		t.Fatal("an odd word was recorded")
	}
	// The dead writer's change, made with no word stored after it: the full
	// check must find it.
	if err := a.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := storeChangeWord(path, nil, 0x1235); err != nil { // as if it died there
		t.Fatal(err)
	}
	if moved, err := b.RefreshIfStale(); err != nil || !moved {
		t.Fatalf("the session did not see the change under an odd word: %v %v", moved, err)
	}
	_, rows, err := b.Query("SELECT count(*) FROM t", nil)
	if err != nil || rows[0][0].I != 1 {
		t.Fatalf("%v %v", rows, err)
	}
}
