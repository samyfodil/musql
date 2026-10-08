package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestWriteIndexSeekMatchesTheWalk runs the same writes with the column seek on
// and off, on a held session (the row store the driver keeps), and compares
// every statement's change count and the whole table after it.
func TestWriteIndexSeekMatchesTheWalk(t *testing.T) {
	setup := []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, s TEXT, m, r REAL)`,
		`CREATE INDEX t_sec ON t(sec)`,
	}
	for i := 1; i <= 400; i++ {
		sec := fmt.Sprint(i % 37)
		if i%23 == 0 {
			sec = "NULL"
		}
		m := fmt.Sprint(i % 5)
		switch i % 7 {
		case 0:
			m = "'3'"
		case 1:
			m = "3.0"
		}
		setup = append(setup, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %d, 'v%d', %s, %d.5)`, i, sec, i%4, i%9, m, i%6))
	}
	writes := []string{
		`UPDATE t SET k = k + 1 WHERE sec = 5`,
		`UPDATE t SET k = k + 10 WHERE sec = '7'`,    // TEXT key, INTEGER column: affinity makes it 7
		`UPDATE t SET sec = sec + 1 WHERE sec = 8`,   // moves rows between keys
		`UPDATE t SET k = 0 WHERE 9 = sec AND k > 1`, // key on the left, another conjunct
		`DELETE FROM t WHERE sec = 11`,
		`UPDATE t SET k = -1 WHERE m = 3`,      // a column holding TEXT and REAL: refused, walks
		`UPDATE t SET k = -2 WHERE r = 2.5`,    // REAL probe
		`UPDATE t SET k = -3 WHERE sec = NULL`, // matches nothing
		`INSERT INTO t(sec, k, s) VALUES (5, 100, 'new')`,
		`UPDATE t SET k = k * 2 WHERE sec = 5`,         // sees the inserted row
		`ALTER TABLE t ADD COLUMN z INTEGER DEFAULT 4`, // short rows read as 4
		`UPDATE t SET k = 77 WHERE z = 4 AND sec = 12`,
		`UPDATE t SET z = 9 WHERE sec = 13`,
		`UPDATE t SET k = 55 WHERE z = 4`, // short rows and stored 4s
		`UPDATE OR REPLACE t SET id = id + 1000 WHERE sec = 14`,
		`DELETE FROM t WHERE sec = 6 AND id % 2 = 0`,
	}
	dir := t.TempDir()
	seekPath, walkPath := filepath.Join(dir, "seek.musq"), filepath.Join(dir, "walk.musq")
	for _, p := range []string{seekPath, walkPath} {
		buildDB(t, p, setup...)
		execDB(t, p, "VACUUM")
	}
	seek, err := OpenWrite(seekPath)
	if err != nil {
		t.Fatal(err)
	}
	defer seek.Discard()
	walk, err := OpenWrite(walkPath)
	if err != nil {
		t.Fatal(err)
	}
	defer walk.Discard()
	dump := func(n *Session) string {
		_, rows, err := n.Query(`SELECT * FROM t ORDER BY id`, nil)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	served := writeIndexSeeksServed.Load()
	for _, w := range writes {
		gotN, _, gotErr := seek.ExecArgs(w, nil)
		writeIndexSeekOffForTest = true
		wantN, _, wantErr := walk.ExecArgs(w, nil)
		writeIndexSeekOffForTest = false
		if (gotErr == nil) != (wantErr == nil) || gotN != wantN {
			t.Fatalf("%s: seek changed %d (%v), walk %d (%v)", w, gotN, gotErr, wantN, wantErr)
		}
		if g, wt := dump(seek), dump(walk); g != wt {
			t.Fatalf("%s: tables differ\n seek %.300s\n walk %.300s", w, g, wt)
		}
	}
	if writeIndexSeeksServed.Load()-served < 8 {
		t.Fatalf("the column seek served %d writes: the comparison proves nothing", writeIndexSeeksServed.Load()-served)
	}
}
