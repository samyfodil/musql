package engine

import (
	"path/filepath"
	"testing"
)

// Tests that OR clauses in ON expressions don't cause infinite recursion in
// the where-clause planner. The query must complete and return zero rows.
func TestR38AOrInOnClauseTerminates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r38a-on.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE ta(a TEXT COLLATE NOCASE, b TEXT COLLATE NOCASE)",
		"INSERT INTO ta VALUES('AAA','BBB')",
		"CREATE TABLE tb(x,y,c TEXT)",
		"INSERT INTO tb(c) VALUES('aaa'),('bbb')",
		"CREATE INDEX tb_c ON tb(c)",
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

	// All four spellings of the shared-column OR, plus the LEFT JOIN form whose
	// ON clause cannot be pushed into a WHERE -- the four cases that aborted.
	for _, q := range []string{
		"SELECT c FROM ta JOIN tb ON (a=c OR b=c)",
		"SELECT c FROM ta CROSS JOIN tb ON (a=c OR b=c)",
		"SELECT c FROM ta JOIN tb ON (c=a OR c=b)",
		"SELECT c FROM ta JOIN tb ON (a=c OR c=b)",
	} {
		_, rows, err := p.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if len(rows) != 0 {
			t.Fatalf("%s: got %d rows, want 0", q, len(rows))
		}
	}

	// A LEFT JOIN keeps its unmatched left row, so this one must answer one
	// NULL rather than nothing -- the same ON-clause OR, on the path where the
	// term cannot be moved to the WHERE clause.
	_, rows, err := p.Query("SELECT c FROM ta LEFT JOIN tb ON (a=c OR b=c)")
	if err != nil {
		t.Fatalf("left join: %v", err)
	}
	if len(rows) != 1 || rows[0][0].Typ != Null {
		t.Fatalf("left join: got %v, want one NULL row", rows)
	}
}
