package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestAppendLoadRearmsAfterAnotherRun: a transaction that reads a table
// between its inserts drains the table's append load at every read, and the
// next insert arms a new one only after as many inserts again as armed the
// first -- not at once, every insert, each load a chunk per column. Every row
// still lands.
func TestAppendLoadRearmsAfterAnotherRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.musq")
	buildDB(t, path, `CREATE TABLE t(id INTEGER PRIMARY KEY, v)`, `INSERT INTO t VALUES(1, 1)`)
	s, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	s.SetCommitsEachStatement(true)
	if err := s.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	tbl := s.tableNamed("t")
	const n = 3 * 256
	arms := 0
	for i := 2; i < 2+n; i++ {
		if err := s.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d, %d)`, i, i)); err != nil {
			t.Fatal(err)
		}
		if tbl.bulk != nil {
			arms++
		}
		if _, rows, err := s.Query(`SELECT count(*) FROM t`, nil); err != nil {
			t.Fatal(err)
		} else if got := rows[0][0].I; got != int64(i) {
			t.Fatalf("after %d rows: count(*) = %d", i, got)
		}
	}
	if err := s.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}
	if arms == 0 || arms > n/appendArmAfter {
		t.Fatalf("%d append loads armed over %d inserts, each drained by the next read", arms, n)
	}
}
