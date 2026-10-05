package driver

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A TEMP table created before main's tables must not share a main table's
// routing root. Main's tables are re-keyed by their place in main's file at
// every catalog rewrite, so a temp table keyed in that range was shadowed by
// the main table that took its number: tr_ok's INSERT INTO log wrote a row
// neither log nor ttrig showed (found by TestTrustedSchemaOffEnforced).
func TestTempTableRootDoesNotShadowMain(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TEMP TABLE ttrig(a)`,
		`CREATE TABLE log(a)`,
		`CREATE TABLE other(a)`,
		`CREATE TABLE trg_ok(a)`,
		`CREATE TRIGGER tr_ok AFTER INSERT ON trg_ok BEGIN INSERT INTO log VALUES(1); END`,
		`INSERT INTO trg_ok VALUES(1)`,
		`INSERT INTO ttrig VALUES(7)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM log`:    1,
		`SELECT count(*) FROM trg_ok`: 1,
		`SELECT count(*) FROM ttrig`:  1,
		`SELECT a FROM ttrig`:         7,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
}
