package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSeekServedAfterALoadWithoutAWrite: a table the session holds live but
// has not written -- CREATE INDEX loads one -- is still answered from the
// committed segments' equality index. Refusing it there made every TEXT, REAL
// and BLOB equality a full scan (TestTypeCoverageVsC ran past its timeout). A
// row the session has written and not committed must still be found.
func TestSeekServedAfterALoadWithoutAWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, tv TEXT, rv REAL, bv BLOB)`}
	for i := 1; i <= 2000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'k%d', %d.5, x'%04x')`, i, i, i, i))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)

	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if err := n.Exec(`CREATE INDEX t_tv ON t(tv)`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT id FROM t WHERE tv = 'k77'`,
		`SELECT id FROM t WHERE rv = 77.5`,
		`SELECT id FROM t WHERE bv = x'004d'`,
	} {
		segIndexSeeksServed = 0
		_, rows, err := n.Query(q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if len(rows) != 1 || rows[0][0].I != 77 {
			t.Errorf("%s: %v, want 77", q, typedRows(rows))
		}
		if segIndexSeeksServed == 0 {
			t.Errorf("%s: answered by a scan, not the segments' equality index", q)
		}
	}

	w, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Discard()
	if err := w.Exec(`INSERT INTO t VALUES(5000, 'k77', 0, x'00')`); err != nil {
		t.Fatal(err)
	}
	_, rows, err := w.Query(`SELECT count(*) FROM t WHERE tv = 'k77'`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(typedRows(rows)) != fmt.Sprint(typedRows([][]Value{{{Typ: Int, I: 2}}})) {
		t.Errorf("uncommitted row not found by the seek: %v", typedRows(rows))
	}
}
