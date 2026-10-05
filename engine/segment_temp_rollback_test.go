package engine

import (
	"path/filepath"
	"testing"
)

// TestTempTableRollsBack gates that TEMP table rows are restored on ROLLBACK.
func TestTempTableRollsBack(t *testing.T) {
	nw, err := Create(filepath.Join(t.TempDir(), "tr.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Discard()
	for _, s := range []string{
		`CREATE TEMP TABLE t2(a, b)`,
		`INSERT INTO t2 VALUES(1, 2)`,
		`BEGIN`,
		`INSERT INTO t2 VALUES(3, 4)`,
	} {
		if err := nw.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	_, rows, _ := nw.Query(`SELECT * FROM t2`, nil)
	t.Logf("in transaction: %d rows (want 2)", len(rows))
	if err := nw.Exec(`ROLLBACK`); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
	_, rows, _ = nw.Query(`SELECT * FROM t2`, nil)
	t.Logf("after rollback: %d rows (want 1)", len(rows))
	if len(rows) != 1 {
		t.Errorf("temp rollback: %d rows, want 1", len(rows))
	}
}
