package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// TestSegJoinAggregateMatchesC holds the columnar join aggregate
// (engine/segment_join.go) to C SQLite's answers on tables it can serve --
// INTEGER keys with no NULLs, the rows all in segments after VACUUM -- and
// checks that it served them, so a decline cannot pass for a match. The keys
// include rows naming a parent that does not exist, negative and far-apart
// rowids, and a sum whose terms may overflow, which must decline.
func TestSegJoinAggregateMatchesC(t *testing.T) {
	setup := []string{
		`CREATE TABLE artists(id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`CREATE TABLE albums(id INTEGER PRIMARY KEY, artist_id INTEGER NOT NULL, year INTEGER NOT NULL)`,
		`CREATE TABLE tracks(id INTEGER PRIMARY KEY, album_id INTEGER NOT NULL, seconds INTEGER NOT NULL, plays INTEGER NOT NULL, rating REAL NOT NULL)`,
		`CREATE INDEX tracks_album ON tracks(album_id)`,
		`CREATE TABLE big(id INTEGER PRIMARY KEY, album_id INTEGER NOT NULL, v INTEGER NOT NULL)`,
		`INSERT INTO artists VALUES (-5,'neg'),(1,'a'),(2,'b'),(3,'c'),(1000000000000,'far')`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 80)
			INSERT INTO albums SELECT i, CASE i % 5 WHEN 0 THEN -5 WHEN 4 THEN 1000000000000 WHEN 3 THEN 77 ELSE i % 3 + 1 END, 1960 + i % 30 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 4000)
			INSERT INTO tracks SELECT i, CASE WHEN i % 50 = 0 THEN 500 + i ELSE 1 + i % 80 END, 100 + i % 250, (i * 7919) % 100000 - 30000, (i % 7) / 2.0 FROM n`,
		`INSERT INTO big VALUES (1, 1, 9223372036854775000), (2, 1, 900), (3, 2, -9223372036854775000), (4, 2, -900)`,
	}
	queries := []struct {
		q     string
		serve bool
	}{
		{`SELECT count(*) FROM albums al JOIN tracks t ON t.album_id = al.id`, true},
		{`SELECT count(*) FROM tracks t JOIN albums al ON al.id = t.album_id`, true},
		{`SELECT count(*), sum(t.seconds), min(t.plays), max(t.plays), count(t.seconds) FROM albums al JOIN tracks t ON t.album_id = al.id`, true},
		{`SELECT sum(t.plays) + 1, count(*) * 2 FROM tracks t JOIN albums al ON al.id = t.album_id`, true},
		{`SELECT count(*), sum(al.year) FROM artists ar JOIN albums al ON al.artist_id = ar.id JOIN tracks t ON t.album_id = al.id`, true},
		{`SELECT count(*), max(ar.id) FROM tracks t JOIN albums al ON al.id = t.album_id JOIN artists ar ON ar.id = al.artist_id`, true},
		{`SELECT count(*) FROM albums al JOIN artists ar ON ar.id = al.artist_id`, true},
		{`SELECT count(*), sum(t.seconds) FROM albums al JOIN tracks t ON t.album_id = al.id WHERE al.id > 1000`, false},
		{`SELECT sum(t.rating) FROM albums al JOIN tracks t ON t.album_id = al.id`, false},
		{`SELECT sum(b.v) FROM albums al JOIN big b ON b.album_id = al.id`, false},
		{`SELECT count(*), sum(b.v) FROM big b JOIN albums al ON al.id = b.album_id WHERE b.id < 0`, false},
	}

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
	if _, err := m.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	read := func(db *sql.DB, q string) (string, error) {
		var out []any
		rows, err := db.Query(q)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return "", err
			}
			out = append(out, vals)
		}
		return fmt.Sprint(out), rows.Err()
	}
	for _, tc := range queries {
		want, werr := read(c, tc.q)
		engine.SegJoinHitsForTest()
		got, gerr := read(m, tc.q)
		hits := engine.SegJoinHitsForTest()
		if (werr == nil) != (gerr == nil) || werr == nil && want != got {
			t.Errorf("%s:\n  C     %s %v\n  musql %s %v", tc.q, want, werr, got, gerr)
		}
		if served := hits > 0; served != tc.serve {
			t.Errorf("%s: served by the join kernel %v, want %v", tc.q, served, tc.serve)
		}
	}
}
