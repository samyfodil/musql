package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestRewriteRebasesTheSession holds a session across its own writes and a
// VACUUM, as a driver connection does. The table it wrote used to stay an
// in-memory store of every row for the session's whole life (rebase), so a
// write scan never reached the segments; after the rewrite it must read from
// the file -- and still hold exactly the rows it wrote.
func TestRewriteRebasesTheSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER)`, `BEGIN`}
	for i := 1; i <= 4000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d)`, i, i%7, i*3))
	}
	stmts = append(stmts, `COMMIT`, `VACUUM`)
	for _, s := range stmts {
		if err := n.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if _, err := n.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if tbl := n.findTableMeta("t"); tbl.rows != nil && tbl.rows.seg == nil {
		t.Fatalf("after the rewrite t is still an in-memory store of %d rows", len(tbl.rows.m))
	}
	SegRowsSelectedForTest()
	// A range, not "k = 3": an equality takes the row store's column seek
	// (emitWriteIndexSeekHint) and never reaches the filter this checks.
	if err := n.Exec(`UPDATE t SET v = v + 1 WHERE k >= 3 AND k <= 3`); err != nil {
		t.Fatal(err)
	}
	if jitEnabled && SegRowsSelectedForTest() == 0 {
		t.Error("the UPDATE's scan never reached the segment filter")
	}
	_, rows, err := n.Query(`SELECT count(*), sum(v), sum(k) FROM t`, nil)
	if err != nil {
		t.Fatal(err)
	}
	// sum(v) = 3 * 4000*4001/2 plus one per k=3 row (3, 10, ... 3997: 572 of them).
	if got := fmt.Sprint(rows[0][0].I, rows[0][1].I, rows[0][2].I); got != fmt.Sprint(4000, 3*4000*4001/2+572, 11997) {
		t.Errorf("after the rebase: count, sum(v), sum(k) = %s", got)
	}
}
