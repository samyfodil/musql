package engine

// Tests that rowid is hidden from UPDATE/DELETE scope on WITHOUT ROWID tables.

import (
	"strings"
	"testing"
)

// TestWriteScanScopeHidesRowidOnWithoutRowid verifies rowid scope rules.
func TestWriteScanScopeHidesRowidOnWithoutRowid(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE wr(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`INSERT INTO wr VALUES('p',1),('q',2)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	for _, s := range []string{
		`UPDATE wr SET b=99 WHERE rowid=1`,
		`DELETE FROM wr WHERE rowid=1`,
		`UPDATE wr SET b=98 WHERE wr.rowid=1`,
		`UPDATE wr SET b=97 WHERE _rowid_=1`,
		`UPDATE wr SET b=96 WHERE oid=1`,
	} {
		if err := db.Exec(s); err == nil {
			t.Errorf("%q: succeeded; a WITHOUT ROWID table has no rowid pseudo-column and C SQLite errors", s)
		} else if !strings.Contains(err.Error(), "no such column") {
			t.Errorf("%q: %v\n  want a \"no such column\" error", s, err)
		}
	}
	p, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatal(perr)
	}
	_, rows, qerr := p.Query(`SELECT a,b FROM wr ORDER BY a`)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) != 2 {
		t.Fatalf("the table holds %d rows, want 2 -- a rejected statement modified data", len(rows))
	}
	// An ordinary WITHOUT ROWID write must be untouched by the same change.
	if err := db.Exec(`UPDATE wr SET b=b+10 WHERE a='p'`); err != nil {
		t.Fatalf("ordinary WITHOUT ROWID update: %v", err)
	}
}
