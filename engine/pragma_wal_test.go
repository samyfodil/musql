package engine

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestWalCheckpointQueryDeclinesOverALog holds the read side of PRAGMA
// wal_checkpoint to the exec side's split. It used to count the frames of a
// SQLite "-wal" this format never has, so over a non-empty delta it answered
// 0|0|0 where C answers 0|N|N, while Exec of the same pragma declined.
func TestWalCheckpointQueryDeclinesOverALog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.musq")
	buildDB(t, path, `PRAGMA journal_mode=wal`, `CREATE TABLE t(a)`)
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if _, _, err := n.Query(`PRAGMA wal_checkpoint`, nil); err != nil {
		t.Fatalf("empty log: %v, want 0|0|0", err)
	}
	if err := n.Exec(`INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`PRAGMA wal_checkpoint`, `PRAGMA wal_checkpoint(passive)`} {
		if _, rows, err := n.Query(q, nil); !errors.Is(err, errVDBEUnsupported) {
			t.Errorf("%s over a non-empty log: rows %v err %v, want a decline", q, rows, err)
		}
	}
}
