package driver_test

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestMemoryDSNsCreateNoFilesInCWD verifies that in-memory database DSNs
// do not create files in the working directory.
func TestMemoryDSNsCreateNoFilesInCWD(t *testing.T) {
	before, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(before))
	for _, e := range before {
		seen[e.Name()] = true
	}

	for _, dsn := range []string{
		":memory:",
		"file::memory:",
		"file:memdbname?mode=memory",
		"file:sharedname?mode=memory&cache=shared",
	} {
		func() {
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatalf("open %q: %v", dsn, err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1) // keep a shared-cache memory db alive
			if _, err := db.Exec(`CREATE TABLE t(a)`); err != nil {
				t.Fatalf("%q: create: %v", dsn, err)
			}
			if _, err := db.Exec(`INSERT INTO t VALUES(1)`); err != nil {
				t.Fatalf("%q: insert: %v", dsn, err)
			}
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
				t.Fatalf("%q: select: %v", dsn, err)
			}
			if n != 1 {
				t.Errorf("%q: count = %d, want 1", dsn, n)
			}
		}()
	}

	after, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after {
		if !seen[e.Name()] {
			t.Errorf("in-memory DSNs created %q in the working directory; "+
				"memory databases must be backed under os.TempDir()", e.Name())
		}
	}
}
