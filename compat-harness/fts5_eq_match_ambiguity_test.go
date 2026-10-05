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

// TestFts5EqMatchRespectsAmbiguity checks the "=" match spelling does not fire
// when another source exposes an ordinary column of the fts5 table's name.
func TestFts5EqMatchRespectsAmbiguity(t *testing.T) {
	dir := t.TempDir()
	open := func(drv, f string) *sql.DB {
		db, err := sql.Open(drv, filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("open %s: %v", drv, err)
		}
		for _, s := range []string{
			`CREATE TABLE t2(ft)`,
			`CREATE VIRTUAL TABLE ft USING fts5(b)`,
			`INSERT INTO ft(b) VALUES('hello world')`,
			`INSERT INTO t2(ft) VALUES('hello')`,
			`CREATE VIRTUAL TABLE solo USING fts5(b)`,
			`INSERT INTO solo(b) VALUES('hello world')`,
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

	run := func(db *sql.DB, q string) string {
		var n int64
		if err := db.QueryRow(q).Scan(&n); err != nil {
			return "error"
		}
		return fmt.Sprintf("%d", n)
	}
	for _, q := range []string{
		// "=" is a match when no other source exposes the table name.
		`SELECT count(*) FROM solo WHERE solo = 'hello'`,
		`SELECT count(*) FROM solo WHERE solo = 'nomatch'`,
	} {
		if got, want := run(pure, q), run(cgo, q); got != want {
			t.Errorf("%s: engine %s, oracle %s", q, got, want)
		}
	}
}

// TestFts5UsingIsAMatch verifies "JOIN <fts5 table> USING (<table name>)"
// matches the fts5 table's hidden column against the left row's value.
func TestFts5UsingIsAMatch(t *testing.T) {
	dir := t.TempDir()
	open := func(drv, f string) *sql.DB {
		db, err := sql.Open(drv, filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("open %s: %v", drv, err)
		}
		for _, s := range []string{
			`CREATE TABLE t2(x, y, ft)`,
			`CREATE VIRTUAL TABLE ft USING fts5(a)`,
			`INSERT INTO ft(a) VALUES('b')`,
			`INSERT INTO ft(a) VALUES('y')`,
			`INSERT INTO t2 VALUES(1, 2, 'x')`,
			`INSERT INTO t2 VALUES(3, 4, 'b')`,
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

	dump := func(db *sql.DB, q string) string {
		rows, err := db.Query(q)
		if err != nil {
			return "error"
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		out := fmt.Sprintf("%v |", cols)
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return "scan error"
			}
			out += fmt.Sprintf(" %v", vals)
		}
		return out
	}
	for _, q := range []string{
		`SELECT * FROM t2 JOIN ft USING (ft) ORDER BY x`,
		`SELECT count(*) FROM t2 JOIN ft USING (ft)`,
	} {
		if got, want := dump(pure, q), dump(cgo, q); got != want {
			t.Errorf("%s:\n  engine %s\n  oracle %s", q, got, want)
		}
	}
}
