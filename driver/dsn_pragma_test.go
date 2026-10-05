package driver

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestDSNPragma: "_pragma=<p>" runs "PRAGMA <p>" on every new connection, in
// order, and a pragma that fails -- or a value that is not one PRAGMA -- fails
// the open instead of being ignored.
func TestDSNPragma(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.db")
	db, err := sql.Open("sqlite", "file:"+p+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for q, want := range map[string]int64{`PRAGMA foreign_keys`: 1, `PRAGMA busy_timeout`: 5000} {
		var got int64
		if err := db.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	// Foreign keys are really on, not just reported on.
	for _, s := range []string{`CREATE TABLE parent(id INTEGER PRIMARY KEY)`, `CREATE TABLE child(pid REFERENCES parent(id))`} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO child VALUES(7)`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("an orphan insert: %v, want the FOREIGN KEY error", err)
	}

	for _, bad := range []string{
		"_pragma=max_page_count(10)",             // a setter this format declines
		"_pragma=foreign_keys(1);DROP+TABLE+x",   // unreadable as a query
		"_pragma=foreign_keys(1)%3BDROP+TABLE+x", // readable, and not one PRAGMA
	} {
		db, err := sql.Open("sqlite", "file:"+p+"?"+bad)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Ping(); err == nil {
			t.Errorf("%s: opened, want an error", bad)
		}
		db.Close()
	}
}
