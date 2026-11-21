package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSchemaSegSeededAtOpen holds a session's first statement to the catalog
// segment its open rendered, and every read of sqlite_master, before and after
// the catalog changes, to the answer of a session that renders its own.
func TestSchemaSegSeededAtOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.musq")
	s, err := OpenOrCreateWait(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE)",
		"CREATE INDEX tb ON t(b, a)",
		"CREATE VIEW v AS SELECT b FROM t",
		"INSERT INTO t VALUES(1,'x'),(2,'y')",
	} {
		if err := s.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	delta, derr := os.ReadFile(segDeltaPath(path))
	run := func(seedOff bool) (string, int64) {
		schemaSegSeedOffForTest = seedOff
		defer func() { schemaSegSeedOffForTest = false }()
		copyPath := filepath.Join(t.TempDir(), "c.musq")
		if err := os.WriteFile(copyPath, base, 0o600); err != nil {
			t.Fatal(err)
		}
		if derr == nil {
			if err := os.WriteFile(segDeltaPath(copyPath), delta, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		s, err := OpenOrCreateWait(copyPath, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		before := schemaSegHits.Load()
		var out string
		read := func() {
			rp, err := s.ReadPager()
			if err != nil {
				t.Fatal(err)
			}
			_, rows, err := rp.QueryArgs("SELECT type, name, tbl_name, rootpage, sql FROM sqlite_master", nil)
			if err != nil {
				t.Fatal(err)
			}
			out += fmt.Sprint(rows) + "\n"
		}
		read()
		hits := schemaSegHits.Load() - before
		for _, q := range []string{"CREATE TABLE u(c)", "DROP INDEX tb", "CREATE TEMP TABLE w(d)"} {
			if err := s.Exec(q); err != nil {
				t.Fatal(err)
			}
			read()
		}
		return out, hits
	}
	seeded, hits := run(false)
	plain, plainHits := run(true)
	if seeded != plain {
		t.Fatalf("seeded:\n%s\nplain:\n%s", seeded, plain)
	}
	if hits != 1 || plainHits != 0 {
		t.Fatalf("first statement served the open's segment %d times seeded, %d unseeded", hits, plainHits)
	}
}
