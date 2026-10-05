package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestExportCarriesTheHeaderFields: a database this engine wrote, exported and
// handed to C, must answer the header pragmas as the database C writes from the
// same SQL does. The import already carried page size, text encoding and
// auto_vacuum into the catalog; the export wrote every file at the caller's
// page size, UTF-8 and auto_vacuum NONE, so the round trip lost all three.
func TestExportCarriesTheHeaderFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
	}{
		{"page_size-1024", []string{`PRAGMA page_size=1024`}},
		{"page_size-65536", []string{`PRAGMA page_size=65536`}},
		{"utf16le", []string{`PRAGMA encoding='UTF-16le'`}},
		{"utf16be", []string{`PRAGMA encoding='UTF-16be'`}},
		{"auto_vacuum-full", []string{`PRAGMA auto_vacuum=1`}},
		{"auto_vacuum-incremental", []string{`PRAGMA auto_vacuum=2`}},
		{"all-three", []string{`PRAGMA page_size=2048`, `PRAGMA encoding='UTF-16le'`, `PRAGMA auto_vacuum=2`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stmts := append(tc.setup,
				`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
				`INSERT INTO t VALUES(1, 'héllo'), (2, 'wörld')`,
			)
			answers := func(path string) []string {
				db, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var out []string
				for _, q := range []string{`PRAGMA page_size`, `PRAGMA encoding`, `PRAGMA auto_vacuum`, `SELECT * FROM t ORDER BY a`, `PRAGMA integrity_check`} {
					out = append(out, renderQuery(db, q))
				}
				return out
			}
			paths := map[string]string{}
			for _, drv := range []string{"sqlite3", "sqlite"} {
				p := filepath.Join(dir, drv+".db")
				db, err := sql.Open(drv, p)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range stmts {
					if _, eerr := db.Exec(s); eerr != nil {
						t.Fatalf("%s %q: %v", drv, s, eerr)
					}
				}
				db.Close()
				paths[drv] = p
			}
			want := answers(paths["sqlite3"])
			got := answers(exportedForOracle(t, paths["sqlite"]))
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("C over the export: %s\n  want (C's own file): %s", got[i], want[i])
				}
			}
		})
	}
}

// TestVacuumIntoCopyHeaderFields: the copy VACUUM INTO writes answers the header
// pragmas as C's copy does. It is a fresh file, so a wal source's copy is in
// rollback mode, and a deferred page_size / auto_vacuum request sizes it. The
// segment branch copied the source's catalog whole and answered wal / 4096 / 0.
func TestVacuumIntoCopyHeaderFields(t *testing.T) {
	answers := map[string][]string{}
	for _, drv := range []string{"sqlite3", "sqlite"} {
		dir := t.TempDir()
		db, err := sql.Open(drv, filepath.Join(dir, "src.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, s := range []string{
			`PRAGMA journal_mode=wal`, `CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`,
			`PRAGMA page_size=1024`, `PRAGMA auto_vacuum=2`,
			`VACUUM INTO '` + filepath.Join(dir, "c.db") + `'`,
		} {
			if _, eerr := db.Exec(s); eerr != nil {
				t.Fatalf("%s %q: %v", drv, s, eerr)
			}
		}
		db.Close()
		c, err := sql.Open(drv, filepath.Join(dir, "c.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`PRAGMA journal_mode`, `PRAGMA page_size`, `PRAGMA auto_vacuum`, `SELECT * FROM t`} {
			answers[drv] = append(answers[drv], renderQuery(c, q))
		}
		c.Close()
	}
	for i, want := range answers["sqlite3"] {
		if got := answers["sqlite"][i]; got != want {
			t.Errorf("VACUUM INTO copy: musql %s, C %s", got, want)
		}
	}
}
