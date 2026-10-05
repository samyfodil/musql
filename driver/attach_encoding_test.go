package driver_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestAttachRejectsAMismatchedEncoding: attach.c:207-211 refuses an
// attached database whose text encoding differs from main's once it has a
// format. Both files here are the engine's own format; reading their encoding
// as if they were SQLite files failed, and the check was silently skipped.
func TestAttachRejectsAMismatchedEncoding(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, enc string) string {
		p := filepath.Join(dir, name)
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, q := range []string{"PRAGMA encoding = '" + enc + "'", "CREATE TABLE x(a)", "INSERT INTO x VALUES(1)"} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		return p
	}
	main, aux := mk("main.musq", "UTF-8"), mk("aux.musq", "UTF-16le")
	db, err := sql.Open("sqlite", main)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("ATTACH '" + aux + "' AS aux")
	if err == nil || !strings.Contains(err.Error(), "same text encoding") {
		t.Fatalf("ATTACH of a UTF-16 database to a UTF-8 one: %v, want the encoding refusal", err)
	}
}
