package driver

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadIntoAnEmptyTableCommitsBySize: a small load into an empty table
// commits as a delta append and a large one by a rewrite; both read back.
func TestLoadIntoAnEmptyTableCommitsBySize(t *testing.T) {
	for _, tc := range []struct {
		rows    int
		rewrite bool
	}{{1, false}, {3, false}, {40000, true}} {
		t.Run(fmt.Sprint(tc.rows, " rows"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.musq")
			w, err := sql.Open(DriverName, path)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			for _, s := range []string{`CREATE TABLE big(id INTEGER PRIMARY KEY, v TEXT)`,
				`INSERT INTO big WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 100) SELECT i, 'v' || i FROM c`,
				`CREATE TABLE q(id INTEGER PRIMARY KEY, v TEXT)`} {
				if _, err := w.Exec(s); err != nil {
					t.Fatalf("%q: %v", s, err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := w.Begin()
			if err != nil {
				t.Fatal(err)
			}
			st, err := tx.Prepare(`INSERT INTO q VALUES (?, ?)`)
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= tc.rows; i++ {
				if _, err := st.Exec(i, fmt.Sprintf("%050d", i)); err != nil {
					t.Fatal(err)
				}
			}
			st.Close()
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if rewrote := !bytes.Equal(before, after); rewrote != tc.rewrite {
				t.Errorf("the commit rewrote the database file: %v, want %v", rewrote, tc.rewrite)
			}
			want := fmt.Sprint(tc.rows, " ", tc.rows*(tc.rows+1)/2)
			read := func(db *sql.DB) string {
				t.Helper()
				var n, sum int
				if err := db.QueryRow(`SELECT count(*), coalesce(sum(id), 0) FROM q`).Scan(&n, &sum); err != nil {
					t.Fatal(err)
				}
				return fmt.Sprint(n, " ", sum)
			}
			r, err := sql.Open(DriverName, path)
			if err != nil {
				t.Fatal(err)
			}
			if got := read(r); got != want {
				t.Errorf("a second handle read %q while the writer was open, want %q", got, want)
			}
			r.Close()
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			f, err := sql.Open(DriverName, path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if got := read(f); got != want {
				t.Errorf("reopened, the file holds %q, want %q", got, want)
			}
		})
	}
}
