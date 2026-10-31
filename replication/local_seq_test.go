package replication

import (
	"context"
	"path/filepath"
	"testing"
)

// TestLocalOpsNumberPastAnUnnotedCommit: a local commit numbers its ops on
// from the last seq this node noted, and when a commit it has not noted yet
// holds the next one -- another connection's, between its commit and its note
// -- it numbers them past the log's last instead: every op is in the log once,
// at seqs with no hole.
func TestLocalOpsNumberPastAnUnnotedCommit(t *testing.T) {
	ctx := context.Background()
	s, db, err := openSyncer(ctx, filepath.Join(t.TempDir(), "a.musq"), CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`); err != nil {
		t.Fatal(err)
	}
	insert := func(v int) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO t(v) VALUES(?)`, v); err != nil {
			t.Fatal(err)
		}
	}
	for v := range 3 {
		insert(v)
	}
	own := s.store.ownSeq.Load()
	s.store.ownSeq.Store(own - 2) // as if the last two commits were not noted yet
	insert(3)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for v := 4; v < 6; v++ {
		if _, err := tx.Exec(`INSERT INTO t(v) VALUES(?)`, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT seq, op, tbl, pk FROM main._repl_oplog WHERE site=? ORDER BY seq`, s.store.Site())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var want uint64
	pks := map[string]bool{}
	for rows.Next() {
		var seq uint64
		var kind int64
		var tbl, pk []byte
		if err := rows.Scan(&seq, &kind, &tbl, &pk); err != nil {
			t.Fatal(err)
		}
		if want++; seq != want {
			t.Fatalf("seq %d where %d comes next", seq, want)
		}
		if OpKind(kind) != OpSchema {
			if pks[string(tbl)+string(pk)] {
				t.Fatalf("seq %d logs a row an earlier op did", seq)
			}
			pks[string(tbl)+string(pk)] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(pks) != 6 || want != own+3 {
		t.Fatalf("%d ops of rows, to seq %d; want 6, to seq %d", len(pks), want, own+3)
	}
	if got := s.store.LocalVV()[s.store.Site()]; got != own+3 {
		t.Fatalf("own seq %d in the version vector, want %d", got, own+3)
	}
}
