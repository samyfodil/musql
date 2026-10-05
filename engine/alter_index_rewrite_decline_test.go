package engine

// ALTER TABLE RENAME COLUMN must roll back completely when it cannot rewrite
// a dependent index, rather than leaving a half-modified schema.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRenameColumnDeclinesRatherThanHalfApplyingAnIndexRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a, c)`,
		`CREATE INDEX i1 ON t1(a)`,
		`INSERT INTO t1 VALUES(1, 2)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	var ix *indexMeta
	for _, cand := range db.indexes {
		if cand.name == "i1" {
			ix = cand
		}
	}
	if ix == nil {
		t.Fatal("index i1 not registered")
	}

	// Broken stored text to trigger the rewrite failure.
	const brokenSQL = `CREATE INDEX i1 ON t1(`
	ix.sql = brokenSQL
	tbl := db.findTableMeta("t1")
	if tbl == nil {
		t.Fatal("table t1 not registered")
	}
	beforeTableSQL := tbl.sql
	beforeCols := append([]string(nil), ix.cols...)
	beforeColName := tbl.cols[0].Name

	err = db.Exec(`ALTER TABLE t1 RENAME COLUMN a TO x`)
	if err == nil {
		t.Fatal("RENAME COLUMN succeeded although index i1's text could not be rewritten -- " +
			"that persists a schema whose index names a column that no longer exists")
	}
	if !strings.Contains(err.Error(), "i1") {
		t.Errorf("error should name the index it could not rewrite, got: %v", err)
	}

	// Verify the state was rolled back.
	if tbl.sql != beforeTableSQL {
		t.Errorf("table sql not restored:\n got %q\nwant %q", tbl.sql, beforeTableSQL)
	}
	if tbl.cols[0].Name != beforeColName {
		t.Errorf("column name not restored: got %q want %q", tbl.cols[0].Name, beforeColName)
	}
	if strings.Join(ix.cols, ",") != strings.Join(beforeCols, ",") {
		t.Errorf("index cols not restored: got %v want %v", ix.cols, beforeCols)
	}
	if ix.sql != brokenSQL {
		t.Errorf("index sql not restored: got %q want %q", ix.sql, brokenSQL)
	}
	db.Discard()
}
