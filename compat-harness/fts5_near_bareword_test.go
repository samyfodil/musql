//go:build sqlite_fts5

package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// Tests that NEAR is only an operator in FTS5 MATCH queries when followed by
// parentheses; a bare NEAR is treated as a word. Control cases test that other
// barewords before parentheses are not incorrectly special.
func TestFts5NearIsOnlySpecialBeforeLP(t *testing.T) {
	dir := t.TempDir()
	open := func(drv, f string) *sql.DB {
		db, err := sql.Open(drv, filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("open %s: %v", drv, err)
		}
		for _, s := range []string{
			`CREATE VIRTUAL TABLE x1 USING fts5(x)`,
			`INSERT INTO x1(x) VALUES('abc def')`,
		} {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s [%s]: %v", s, drv, err)
			}
		}
		return db
	}
	cgo, pure := open("sqlite3", "c.db"), open("sqlite", "g.db")
	defer cgo.Close()
	defer pure.Close()

	run := func(db *sql.DB, m string) string {
		rows, err := db.Query(`SELECT rowid FROM x1 WHERE x1 MATCH ?`, m)
		if err != nil {
			return "error"
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return fmt.Sprintf("%d rows", n)
	}
	for _, m := range []string{
		`abc NEAR def`,
		`abc NEAR ".." NEAR def`,
		`NEAR`,
		`NEAR NEAR`,
		`NEAR(abc def)`, // the operator: the one that matches
		`near(abc def)`, // control: not exactly "NEAR"
		`Near(abc def)`, // control
		`foo(abc def)`,  // control
	} {
		if got, want := run(pure, m), run(cgo, m); got != want {
			t.Errorf("MATCH %q: engine %s, oracle %s", m, got, want)
		}
	}
}
