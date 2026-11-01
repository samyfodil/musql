package engine

import (
	"path/filepath"
	"testing"
)

func TestIntegrityCheckPassingShapes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.musq")
	buildDB(t, path,
		`CREATE TABLE c(a INTEGER CHECK (a > 0), b TEXT)`,
		`INSERT INTO c VALUES(1, 'x'), (2, 'y')`,
		`CREATE TABLE w(k PRIMARY KEY, v CHECK (v > 0)) WITHOUT ROWID`,
	)
	for _, q := range []string{`PRAGMA integrity_check`, `PRAGMA quick_check`, `PRAGMA integrity_check(w)`} {
		wantGenRows(t, path, q, "ok")
	}
}
