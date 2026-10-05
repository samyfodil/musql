package engine

import (
	"path/filepath"
	"testing"
)

// TestR38ANotIndexedDistinctAgg verifies DISTINCT aggregate with NOT INDEXED.
func TestR38ANotIndexedDistinctAgg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r38a.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE t1( x INTEGER, y VARCHAR(8) )",
		"INSERT INTO t1 VALUES(1,'true')",
		"INSERT INTO t1 VALUES(0,'false')",
		"INSERT INTO t1 VALUES(NULL,'NULL')",
		"CREATE INDEX t1i1 ON t1(x)",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, rows, err := p.Query("SELECT group_concat(DISTINCT x) FROM t1 NOT INDEXED")
	if err != nil {
		t.Fatalf("declined: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("shape: %v", rows)
	}
	if got := string(rows[0][0].S); got != "1,0" {
		t.Fatalf("got %q want %q", got, "1,0")
	}
}
