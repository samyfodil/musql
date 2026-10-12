package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestHeldTransactionRewriteRebases is TestRewriteRebasesTheSession for a
// DRIVER-held transaction, which commits with the hold still marked: the
// rebase was skipped there, and every table the transaction loaded stayed in
// memory with no index to seek -- an indexed equality sorted the whole table
// per query.
func TestHeldTransactionRewriteRebases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	for _, s := range []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, v INTEGER)`, `CREATE INDEX t_sec ON t(sec)`} {
		if err := n.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.Commit(); err != nil {
		t.Fatal(err)
	}
	n.MarkHeldTransaction()
	if err := n.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3000; i++ {
		if err := n.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d)`, i, (i*37)%3000, i)); err != nil {
			t.Fatal(err)
		}
	}
	// A catalog change makes this commit a rewrite, as a bulk load does.
	for _, s := range []string{`CREATE TABLE u(x)`, `COMMIT`} {
		if err := n.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.Commit(); err != nil {
		t.Fatal(err)
	}
	n.EndHeldTransaction()
	// Rebased, the table reads from the file: not loaded, or loaded with
	// nothing over the segments.
	if tbl := n.findTableMeta("t"); tbl.rows != nil && len(tbl.rows.m) > 0 {
		t.Fatalf("after the held transaction's rewrite t still holds %d rows in memory", len(tbl.rows.m))
	}
	_, rows, err := n.Query(`SELECT count(*), sum(v) FROM t WHERE sec = 37`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rows[0][0].I, rows[0][1].I); got != "1 1" {
		t.Errorf("sec = 37: count, sum(v) = %s", got)
	}
}
