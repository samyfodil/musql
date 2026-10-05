package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestAttachOpensTheFile verifies ATTACH and DETACH file handling.
func TestAttachOpensTheFile(t *testing.T) {
	t.Run("attach", func(t *testing.T) {
		for ci, tc := range []struct{ name, path string }{
			{"nonexistent directory", "/nonexistent/dir/x.db"},
			{"a directory, not a file", "/tmp"},
			{"a new file next to main", "%DIR%/brand-new.db"},
		} {
			ci, tc := ci, tc
			t.Run(fmt.Sprintf("%02d-%s", ci, tc.name), func(t *testing.T) {
				var msg [2]string
				for i, drv := range []string{"sqlite3", "sqlite"} {
					dir := t.TempDir()
					db, err := sql.Open(drv, filepath.Join(dir, "main.db"))
					if err != nil {
						t.Fatal(err)
					}
					db.SetMaxOpenConns(1)
					if _, err := db.Exec(`CREATE TABLE t(a)`); err != nil {
						t.Fatal(err)
					}
					path := tc.path
					if path == "%DIR%/brand-new.db" {
						path = filepath.Join(dir, "brand-new.db")
					}
					if _, e := db.Exec(`ATTACH DATABASE '` + path + `' AS z`); e != nil {
						msg[i] = e.Error()
					}
					db.Close()
				}
				if tc.path == "%DIR%/brand-new.db" {
					if (msg[0] == "") != (msg[1] == "") {
						t.Errorf("new file: cgo=%q musql=%q", msg[0], msg[1])
					}
					return
				}
				if msg[0] != msg[1] {
					t.Errorf("%s\n  cgo: %q\n  mus: %q", tc.path, msg[0], msg[1])
				}
			})
		}
	})

	t.Run("detach-temp", func(t *testing.T) {
		for ci, pre := range []string{
			``,
			`PRAGMA temp_store=2`,
			`CREATE TEMP TABLE tt(x)`,
			`CREATE TEMP VIEW tv AS SELECT 1`,
			`SELECT * FROM temp.sqlite_master`,
			`SELECT count(*) FROM temp.sqlite_master`,
		} {
			ci, pre := ci, pre
			t.Run(fmt.Sprintf("%02d", ci), func(t *testing.T) {
				var msg [2]string
				for i, drv := range []string{"sqlite3", "sqlite"} {
					db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
					if err != nil {
						t.Fatal(err)
					}
					db.SetMaxOpenConns(1)
					if _, err := db.Exec(`CREATE TABLE t(a)`); err != nil {
						t.Fatal(err)
					}
					if pre != "" {
						if rows, qerr := db.Query(pre); qerr == nil {
							for rows.Next() {
							}
							rows.Close()
						} else if _, eerr := db.Exec(pre); eerr != nil {
							t.Fatalf("%s: %v", pre, eerr)
						}
					}
					if _, e := db.Exec(`DETACH temp`); e != nil {
						msg[i] = e.Error()
					}
					db.Close()
				}
				if msg[0] != msg[1] {
					t.Errorf("after %q\n  cgo: %q\n  mus: %q", pre, msg[0], msg[1])
				}
			})
		}
	})
}
