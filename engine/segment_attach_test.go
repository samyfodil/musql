package engine

import (
	"path/filepath"
	"testing"
)

// Tests ATTACH of a segment database and reading through the attached session.
func TestAttachSegmentDatabase(t *testing.T) {
	dir := t.TempDir()
	aux := filepath.Join(dir, "aux.musq")
	nw2, err := Create(aux)
	if err != nil {
		t.Fatal(err)
	}
	if err := nw2.Exec(`CREATE TABLE b(x)`); err != nil {
		t.Fatal(err)
	}
	if err := nw2.Close(); err != nil {
		t.Fatal(err)
	}
	nw, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Discard()
	if err := nw.Exec(`CREATE TABLE a(y)`); err != nil {
		t.Fatal(err)
	}
	if err := nw.Exec(`ATTACH DATABASE '` + aux + `' AS aux`); err != nil {
		t.Fatalf("ATTACH: %v", err)
	}
	for _, c := range []struct {
		q    string
		want int
	}{
		{`SELECT name FROM aux.sqlite_master`, 1},
		{`SELECT name FROM sqlite_master`, 1},
		{`SELECT count(*) FROM aux.b`, 1},
		{`SELECT y FROM a`, 0},
	} {
		_, rows, qerr := nw.Query(c.q, nil)
		if qerr != nil {
			t.Errorf("%s: %v", c.q, qerr)
			continue
		}
		if len(rows) != c.want {
			t.Errorf("%s: %d rows, want %d", c.q, len(rows), c.want)
		}
	}
}
