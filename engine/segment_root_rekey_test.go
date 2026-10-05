package engine

import (
	"path/filepath"
	"testing"
)

// TestRewriteKeepsEachTablesRoot holds one session across autocommit
// statements, as the driver does. A catalog rewrite used to re-key every table
// to its directory position while the session kept the number it held, so
// after a non-tail DROP a new table's rows were read back as an older table's
// (SegmentFile.RootOf). The compaction leg checks the roots and the catalog's
// rowids both survive a delta fold, which rebuilds every table's entry.
func TestRewriteKeepsEachTablesRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`CREATE TABLE a(x)`, `CREATE TABLE b(x)`, `DROP TABLE a`,
		`CREATE TABLE c(y)`, `INSERT INTO c VALUES (1)`} {
		if err := n.Exec(s); err != nil {
			t.Fatal(err)
		}
		if _, err := n.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	q := `SELECT (SELECT count(*) FROM b), (SELECT count(*) FROM c), ` +
		`(SELECT group_concat(rowid || ':' || name) FROM sqlite_master)`
	check := func(n *Session, when string) {
		t.Helper()
		_, rows, err := n.Query(q, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := rows[0][0].I; got != 0 {
			t.Errorf("%s: count(*) FROM b = %d, want 0", when, got)
		}
		if got := rows[0][1].I; got != 1 {
			t.Errorf("%s: count(*) FROM c = %d, want 1", when, got)
		}
		if got := string(rows[0][2].S); got != "2:b,3:c" {
			t.Errorf("%s: sqlite_master = %q, want 2:b,3:c", when, got)
		}
	}
	check(n, "after the rewrites")
	if did, err := n.CompactIfLargerThan(0); err != nil || !did {
		t.Fatalf("compaction: did=%v err=%v", did, err)
	}
	check(n, "after compaction")
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Discard()
	check(r, "reopened")
}
