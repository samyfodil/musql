// Engine-level tests for schema reload. These verify durability across
// Close/reopen and that reloads don't destroy existing rows.
package engine

import (
	"path/filepath"
	"testing"
)

// TestReloadFromImageKeepsThisSessionsInsertsOverAnExistingFile verifies
// that schema reload preserves rows inserted before the reload.
func TestReloadFromImageKeepsThisSessionsInsertsOverAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reload.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(a, b)`,
		`INSERT INTO t VALUES(1, 'one')`,
	} {
		if _, _, err := db.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Second session over the existing file with real pages.
	db, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`INSERT INTO t VALUES(2, 'two')`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_schema SET sql='CREATE TABLE t(a,b,c)' WHERE name='t'`,
		`PRAGMA writable_schema=RESET`,
	} {
		if _, _, err := db.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cols, rows, err := p.QueryArgs(`SELECT a, b, c FROM t ORDER BY a`, nil)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(cols) != 3 {
		t.Fatalf("the reloaded three-column definition did not survive Close/reopen: cols=%v", cols)
	}
	if len(rows) != 2 {
		t.Fatalf("row count after a reload over an existing file: got %d, want 2 -- the insert this session made before the RESET was lost (see noteReloadedFromImage)", len(rows))
	}
	if rows[0][0].I != 1 || rows[1][0].I != 2 {
		t.Fatalf("rows after a reload: %v", rows)
	}
}
