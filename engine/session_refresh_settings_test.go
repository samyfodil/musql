package engine

import (
	"path/filepath"
	"testing"
)

// A connection's PRAGMA settings survive another connection's commit, which
// rebuilds this session's DB from the file (Session.reopen ->
// adoptConnSettingsFrom).
func TestRefreshKeepsConnectionPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	a, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Discard()
	for _, s := range []string{
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`CREATE TABLE ch(pk REFERENCES p(k))`,
		`INSERT INTO p VALUES(1)`,
	} {
		if e := a.Exec(s); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := a.Commit(); e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{`PRAGMA foreign_keys=ON`, `PRAGMA reverse_unordered_selects=1`} {
		if e := a.Exec(s); e != nil {
			t.Fatal(e)
		}
	}
	b, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	if e := b.Exec(`INSERT INTO p VALUES(2)`); e != nil {
		t.Fatal(e)
	}
	if _, e := b.Commit(); e != nil {
		t.Fatal(e)
	}
	b.Close()
	if did, e := a.RefreshIfStale(); e != nil || !did {
		t.Fatalf("refresh: %v, %v", did, e)
	}
	if e := a.Exec(`INSERT INTO ch VALUES(99)`); e == nil {
		t.Error("an orphan child row was accepted: foreign_keys was lost by the refresh")
	}
	if !a.pragmaState.ReverseUnorderedSelects() {
		t.Error("reverse_unordered_selects was lost by the refresh")
	}
}
