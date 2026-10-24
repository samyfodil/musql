package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestInstallSegmentFile: an installed file replaces the database for a session
// that had it loaded, as a commit after the replaced state's last -- even when
// the installed file's own counter is lower -- and a stale counter is refused.
func TestInstallSegmentFile(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "dst.musq"), filepath.Join(dir, "src.musq")
	run := func(s *Session, qs ...string) {
		t.Helper()
		for _, q := range qs {
			if err := s.Exec(q); err != nil {
				t.Fatal(q, err)
			}
			if _, err := s.Commit(); err != nil {
				t.Fatal(q, err)
			}
		}
	}
	query := func(s *Session, q string) [][]Value {
		t.Helper()
		if _, err := s.RefreshIfStale(); err != nil {
			t.Fatal(err)
		}
		_, rows, err := s.Query(q, nil)
		if err != nil {
			t.Fatal(q, err)
		}
		return rows
	}
	state := func(p string) uint32 {
		t.Helper()
		var c uint32
		if err := CommitState(p, func(i CommitInfo) error { c = i.Counter; return nil }); err != nil {
			t.Fatal(err)
		}
		return c
	}
	d, err := Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	run(d, `CREATE TABLE t(a INTEGER PRIMARY KEY)`, `INSERT INTO t VALUES(1)`, `INSERT INTO t VALUES(2)`, `INSERT INTO t VALUES(3)`)
	s, err := Create(src)
	if err != nil {
		t.Fatal(err)
	}
	run(s, `CREATE TABLE u(b TEXT)`, `INSERT INTO u VALUES('x')`)
	s.Close()
	if _, err := CompactSegmentFile(src); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWrite(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if rows := query(r, `SELECT count(*) FROM t`); rows[0][0].I != 3 {
		t.Fatalf("before: %v", rows)
	}
	before := state(dst)
	if srcCtr := state(src); srcCtr >= before {
		t.Fatalf("src counter %d, want below dst's %d for the test to mean anything", srcCtr, before)
	}
	if err := InstallSegmentFile(dst, src, before-1); !errors.Is(err, ErrStateMoved) {
		t.Fatalf("stale counter: %v, want ErrStateMoved", err)
	}
	if err := InstallSegmentFile(dst, src, before); err != nil {
		t.Fatal(err)
	}
	if after := state(dst); after <= before {
		t.Fatalf("counter %d after the install, want past %d", after, before)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src still there: %v", err)
	}
	for _, s := range []*Session{r, d} {
		if rows := query(s, `SELECT b FROM u`); len(rows) != 1 || string(rows[0][0].S) != "x" {
			t.Fatalf("after: %v", rows)
		}
		if _, _, err := s.Query(`SELECT count(*) FROM t`, nil); err == nil {
			t.Fatal("t survived the install")
		}
	}
	run(r, `INSERT INTO u VALUES('y')`)
	if rows := query(d, `SELECT count(*) FROM u`); rows[0][0].I != 2 {
		t.Fatalf("a commit after the install: %v", rows)
	}
}
