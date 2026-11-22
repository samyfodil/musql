package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestColumnViewRendersNoCatalog: a session renders its catalog once, at open.
// The committed column view (DB.segColumns) a columnar read opens holds table
// segments only, and still answers as the session's rows do, before and after
// a commit.
func TestColumnViewRendersNoCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.musq")
	s, err := OpenOrCreateWait(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b INTEGER, c TEXT UNIQUE)",
		"CREATE INDEX tb ON t(b)",
		"CREATE VIEW v AS SELECT b FROM t",
		"INSERT INTO t VALUES(1,10,'x'),(2,20,'y'),(3,30,'z')",
	} {
		if err := s.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	before := schemaSegBuilds.Load()
	s, err = OpenOrCreateWait(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	read := func(q string) string {
		rp, err := s.ReadPager()
		if err != nil {
			t.Fatal(err)
		}
		_, rows, err := rp.QueryArgs(q, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, r := range rows {
			for _, v := range r {
				if v.Typ == Int {
					out += fmt.Sprintf("%d ", v.I)
				} else {
					out += valueText(v) + " "
				}
			}
		}
		return out
	}
	const q = "SELECT count(*), sum(b), max(c) FROM t WHERE b > 10"
	if got := read(q); got != "2 50 z " {
		t.Fatalf("got %s", got)
	}
	if s.DB.segColumns == nil {
		t.Fatal("no column view was opened")
	}
	if n := schemaSegBuilds.Load() - before; n != 1 {
		t.Fatalf("%d catalogs rendered for one session, want 1", n)
	}
	if got := read("SELECT count(*) FROM sqlite_master"); got != "4 " {
		t.Fatalf("sqlite_master: %s", got)
	}
	if err := s.Exec("INSERT INTO t VALUES(4,40,'w')"); err != nil {
		t.Fatal(err)
	}
	if got := read(q); got != "3 90 z " {
		t.Fatalf("after a commit: %s", got)
	}
}
