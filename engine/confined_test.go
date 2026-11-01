package engine

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSetConfinedLocksQueryOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.musq")
	buildDB(t, path, `CREATE TABLE t(a)`)
	s, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	s.SetConfined(true)
	if err := s.Exec(`PRAGMA query_only = OFF`); !errors.Is(err, ErrConfined) {
		t.Fatalf("query_only = OFF: %v, want ErrConfined", err)
	}
	if err := s.Exec(`PRAGMA query_only = ON`); err != nil {
		t.Fatalf("query_only = ON: %v", err)
	}
	if err := s.Exec(`INSERT INTO t VALUES(1)`); err == nil {
		t.Fatal("a read-only confined session wrote")
	}
}
