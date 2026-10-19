//go:build sqlite_fts5

package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestFts5RankBoundMatchMatchesC: rank, bm25, snippet and highlight over a
// MATCH whose query is a bound parameter -- the way an application searches --
// answer as C SQLite does, statement by statement and query after query on the
// same prepared statement, so a plan built for one query's scores is never
// reused for the next. Plain, a CTE, a derived table and an external-content
// table, as a page searching a table's text index does.
func TestFts5RankBoundMatchMatchesC(t *testing.T) {
	setup := []string{
		`CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT)`,
		`INSERT INTO notes(body) VALUES ('the quick brown fox'), ('quick quick fox fox'), ('lazy dog sleeps all day'),
			('a fox and a dog'), ('the ocean is deep'), ('deep sleep is good for the dog')`,
		`CREATE VIRTUAL TABLE t USING fts5(body)`,
		`INSERT INTO t(rowid, body) SELECT id, body FROM notes`,
		`CREATE VIRTUAL TABLE ext USING fts5(body, content='notes', content_rowid='id')`,
		`INSERT INTO ext(ext) VALUES ('rebuild')`,
	}
	queries := []string{
		`SELECT rowid, rank FROM t WHERE t MATCH ? ORDER BY rank`,
		`SELECT rowid, bm25(t) FROM t WHERE t MATCH ? ORDER BY rowid`,
		`SELECT rowid, highlight(t, 0, '[', ']'), snippet(t, 0, '<', '>', '…', 3) FROM t WHERE t MATCH ? ORDER BY rank`,
		`SELECT rowid FROM t WHERE t MATCH ? AND rowid > 1 ORDER BY rank LIMIT 3`,
		`WITH k AS (SELECT rowid AS id, rank FROM t WHERE t MATCH ? ORDER BY rank LIMIT 3) SELECT * FROM k`,
		`SELECT * FROM (SELECT rowid AS id, rank AS r FROM t WHERE t MATCH ? ORDER BY rank) ORDER BY id`,
		`SELECT rowid, rank FROM ext WHERE ext MATCH ? ORDER BY rank`,
	}
	binds := []any{"fox", "dog", `"deep" OR "quick"`, "nothing", "fox dog"}

	dir := t.TempDir()
	open := func(driver, name string) *sql.DB {
		db, err := sql.Open(driver, filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		for _, s := range setup {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s %s: %v", driver, s, err)
			}
		}
		return db
	}
	c, m := open("sqlite3", "c.db"), open("sqlite", "m.musq")
	read := func(db *sql.DB, q string, arg any) ([][]any, error) {
		rows, err := db.Query(q, arg)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			out = append(out, vals)
		}
		return out, rows.Err()
	}
	for _, q := range queries {
		for _, b := range binds {
			want, werr := read(c, q, b)
			got, gerr := read(m, q, b)
			if fmt.Sprint(werr) != fmt.Sprint(gerr) && (werr == nil) != (gerr == nil) {
				t.Errorf("%s [%v]: C err %v, musql err %v", q, b, werr, gerr)
				continue
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s [%v]:\n  C     %v\n  musql %v", q, b, want, got)
			}
		}
	}
}
