package engine

import (
	"path/filepath"
	"testing"
)

// TestFts5ConfigVersionIsReplacedNotAppended tests that FTS5 config updates replace
// existing rows, not append duplicates.
func TestFts5ConfigVersionIsReplacedNotAppended(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "fts5cfg.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE VIRTUAL TABLE xyz USING fts5(x)`,
		`INSERT INTO xyz(rowid, x) VALUES
			(1,'one document'),(2,'two document'),(3,'three document'),
			(4,'four document'),(5,'five document'),(6,'six document')`,
		`INSERT INTO xyz(xyz, rank) VALUES('secure-delete', 1)`,
		`BEGIN`,
		`INSERT INTO xyz(rowid, x) VALUES(7, 'seven document')`,
		`SAVEPOINT one`,
		`DELETE FROM xyz WHERE rowid = 4`,
		`ROLLBACK TO one`,
		`DELETE FROM xyz WHERE rowid = 3`,
		`COMMIT`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	// Check that exactly one version row exists with the correct value.
	pg, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := pg.QueryArgs(`SELECT v FROM xyz_config WHERE k='version'`, nil)
	if err != nil {
		t.Fatalf("select version: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("xyz_config has %d 'version' rows, want exactly 1: %v", len(rows), rows)
	}
	if rows[0][0].Typ != Int || rows[0][0].I != fts5SecureDeleteVersion {
		t.Fatalf("version = %+v, want the integer %d", rows[0][0], fts5SecureDeleteVersion)
	}
}
