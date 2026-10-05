package driver

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// A FILE THAT IS NOT ONE OF THIS ENGINE'S DATABASES IS AN ERROR, never an
// implicit conversion -- a SQLite database included, which musql-convert
// converts explicitly.
func TestForeignFileIsDeclined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	if err := os.WriteFile(path, []byte("not a database of this engine"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, _ := sql.Open(DriverName, path)
	defer db.Close()
	if _, err := db.Exec(`SELECT 1`); err == nil {
		t.Fatal("the driver accepted a file of another format")
	}
}
