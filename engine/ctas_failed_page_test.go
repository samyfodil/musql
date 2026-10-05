package engine

import (
	"path/filepath"
	"testing"
)

// TestFailedCTASFreesItsBtree verifies that a failed CREATE TABLE ... AS SELECT
// cleans up its allocated b-tree without leaving orphaned pages.
func TestFailedCTASFreesItsBtree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`CREATE TABLE m1(a)`, `INSERT INTO m1 VALUES(1)`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Exec(`CREATE TABLE m19 AS SELECT nosuchcolumn FROM m1`); err == nil {
		t.Fatal("the CTAS was expected to fail; the fixture needs a different failing SELECT")
	}
	if err := db.Exec(`CREATE TABLE after(x)`); err != nil {
		t.Fatalf("a later statement failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rp, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	cols, rows, err := rp.QueryArgs(`PRAGMA integrity_check`, nil)
	if err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	var got []string
	for _, r := range rows {
		for _, v := range r {
			got = append(got, valueText(v))
		}
	}
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("integrity_check after a failed CTAS: cols=%v rows=%q; want one \"ok\" -- a page the failed statement allocated is still in the file", cols, got)
	}
}
