package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSegmentDeltaOrphanBatchNotResurrectedByNextCommit verifies that an incomplete
// batch (with records but no commit marker) is not retroactively committed.
func TestSegmentDeltaOrphanBatchNotResurrectedByNextCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w1.db")
	exec := func(n *Session, stmts ...string) {
		t.Helper()
		for _, s := range stmts {
			if _, _, err := n.ExecArgs(s, nil); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	read := func() string {
		t.Helper()
		n, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		_, rows, err := n.Query(`SELECT a FROM t UNION ALL SELECT 'u'||b FROM u`, nil)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(string(r[0].S) + ";")
		}
		return b.String()
	}

	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(n, `CREATE TABLE t(a)`, `CREATE TABLE u(b)`, `INSERT INTO t VALUES('base1'),('base2')`)
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}

	// The batch to orphan: a whole commit's records, then cut off its trailer.
	// (The baseline is DDL, which rewrites the file, so there may be no delta
	// yet: absent reads as empty.)
	deltaPath := segDeltaPath(path)
	before, err := os.ReadFile(deltaPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(n, `UPDATE t SET a = 'ORPHAN-' || a`)
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before)+segDeltaTrailerSz {
		t.Fatalf("the UPDATE did not append a batch to the delta (%d -> %d bytes); the fixture needs one to orphan", len(before), len(after))
	}
	if err := os.WriteFile(deltaPath, after[:len(after)-segDeltaTrailerSz], 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "base1;base2;" {
		t.Fatalf("the orphan is visible before any further commit: %s", got)
	}

	// One ordinary, unrelated commit -- DML, so it APPENDS (DDL would rewrite the
	// file and never reach the append point this is about).
	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(n, `INSERT INTO u VALUES(1)`)
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	// Both halves: the orphan stays dead, AND the new commit is live. Appending
	// past the orphan instead of over it loses the new commit -- replay stops at
	// the orphan's missing trailer -- which is the delta's form of the same bug.
	if got := read(); got != "base1;base2;u1;" {
		t.Errorf("after the next commit t|u reads %s, want base1;base2;u1; (ORPHAN resurrected, or the commit lost behind it)", got)
	}
}
