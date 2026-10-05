package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestFts3OptimizeMidTransactionUntainted tests that fts3 optimize() works
// on pending segments within a transaction.
func TestFts3OptimizeMidTransactionUntainted(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE ft USING fts3(x)`,
		`INSERT INTO ft VALUES('a one'), ('b one'), ('c one')`,
		`BEGIN`,
		`INSERT INTO ft VALUES('a one'), ('b one'), ('c one')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, rows, err := p.QueryArgs(`SELECT docid, optimize(ft) FROM ft WHERE ft MATCH 'one'`, nil)
	if err != nil {
		t.Fatalf("SELECT ... optimize(ft) ...: %v", err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%d=%s", r[0].I, r[1].S))
	}
	want := []string{
		"1=Index optimized",
		"2=Index already optimal",
		"3=Index already optimal",
		"4=Index already optimal",
		"5=Index already optimal",
		"6=Index already optimal",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v rows, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFts3OptimizeMidTransactionTainted confirms optimize() still declines
// once a statement fts3_txn.go's model does not account for has run in the
// same transaction -- here a CREATE TABLE, one of the DDL forms this file's
// header comment (fts3_txn.go) lists as an unmodelled flush point. Real fts3
// would flush the accumulating INSERT into its own real segment there, which
// this engine's eager rewrite has no way to tell apart from "nothing changed"
// without db.fts3TxnTaint -- so "Never wrong" means declining rather than
// guessing at the segment count optimize() would report.
func TestFts3OptimizeMidTransactionTainted(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE ft USING fts3(x)`,
		`BEGIN`,
		`INSERT INTO ft VALUES('a one'), ('b one'), ('c one')`,
		`CREATE TABLE unrelated(y)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, _, err = p.QueryArgs(`SELECT docid, optimize(ft) FROM ft WHERE ft MATCH 'one'`, nil)
	if err == nil {
		t.Fatal("optimize() after an untracked DDL statement: want a decline, got success")
	}
}
