package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestJoinAggregatesMatchC holds aggregates over joins through a secondary
// index to C SQLite's answers, row for row. These are the shapes a GROUP BY
// whose groups arrive in scan order streams (no sorter, the row record a view
// of the registers, only the columns something reads loaded), and a count
// whose row order cannot show skips the index-order sort: bare columns and
// min()/max() choosing the anchor row, order-sensitive group_concat, HAVING,
// LIMIT/OFFSET, a subquery reading the anchor, NULL join keys and texts. Run
// on rows the connection wrote itself and again after VACUUM, which reads
// them from the file.
func TestJoinAggregatesMatchC(t *testing.T) {
	setup := []string{
		`CREATE TABLE artists(id INTEGER PRIMARY KEY, name TEXT, country TEXT)`,
		`CREATE TABLE albums(id INTEGER PRIMARY KEY, artist_id INTEGER, title TEXT, year INTEGER)`,
		`CREATE TABLE tracks(id INTEGER PRIMARY KEY, album_id INTEGER, title TEXT, seconds INTEGER, plays INTEGER, rating REAL)`,
		`CREATE INDEX tracks_album ON tracks(album_id)`,
		`CREATE INDEX albums_artist ON albums(artist_id)`,
		`INSERT INTO artists(name, country) VALUES ('Nina','US'),('Fela','NG'),('Bjork','IS'),('Air','FR'),('Nobody',NULL)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 60)
			INSERT INTO albums(artist_id, title, year) SELECT CASE WHEN i % 13 = 0 THEN NULL ELSE 1 + i % 4 END, 'Album ' || (i % 17), 1960 + i % 40 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 3000)
			INSERT INTO tracks(album_id, title, seconds, plays, rating)
			SELECT CASE WHEN i % 97 = 0 THEN NULL ELSE 1 + i % 61 END, 'Track ' || (i % 50), 100 + i % 250, (i * 7919) % 100000,
				CASE WHEN i % 11 = 0 THEN NULL ELSE (i % 50) / 10.0 END FROM n`,
	}
	queries := []string{
		`SELECT al.id, count(*), sum(t.seconds) FROM albums al JOIN tracks t ON t.album_id = al.id GROUP BY al.id`,
		`SELECT al.id, al.title, count(*), max(t.plays), t.title FROM albums al JOIN tracks t ON t.album_id = al.id GROUP BY al.id`,
		`SELECT al.id, min(t.rating), t.title, t.id FROM albums al JOIN tracks t ON t.album_id = al.id GROUP BY al.id`,
		`SELECT al.id, t.title, t.seconds FROM albums al JOIN tracks t ON t.album_id = al.id GROUP BY al.id`,
		`SELECT al.id, group_concat(t.id) FROM albums al JOIN tracks t ON t.album_id = al.id AND t.id < 400 GROUP BY al.id`,
		`SELECT al.id, count(*) AS n FROM albums al JOIN tracks t ON t.album_id = al.id GROUP BY al.id HAVING n > 49`,
		`SELECT al.id, sum(t.plays) FROM albums al JOIN tracks t ON t.album_id = al.id GROUP BY al.id LIMIT 5 OFFSET 3`,
		`SELECT al.id, count(*), (SELECT name FROM artists WHERE id = al.artist_id) FROM albums al JOIN tracks t ON t.album_id = al.id GROUP BY al.id`,
		`SELECT al.title, ar.name, count(*) FROM albums al JOIN artists ar ON ar.id = al.artist_id JOIN tracks t ON t.album_id = al.id GROUP BY al.id`,
		`SELECT ar.name, sum(t.plays), count(DISTINCT al.id) FROM artists ar JOIN albums al ON al.artist_id = ar.id JOIN tracks t ON t.album_id = al.id GROUP BY ar.id`,
		`SELECT count(*) FROM albums al JOIN tracks t ON t.album_id = al.id`,
		// Visited in tracks_album's order, which the order-sensitive aggregate
		// and the bare column both show: the order a count may skip.
		`SELECT group_concat(t.id) FROM tracks t JOIN albums al ON al.id = t.album_id WHERE t.id % 7 = 0`,
		`SELECT max(t.seconds), t.id FROM tracks t JOIN albums al ON al.id = t.album_id`,
		`SELECT count(*), count(t.rating) FROM albums al JOIN tracks t ON t.album_id = al.id WHERE al.year > 1980`,
		`SELECT count(*) FROM artists ar JOIN albums al ON al.artist_id = ar.id JOIN tracks t ON t.album_id = al.id`,
		`SELECT sum(t.rating), total(t.rating), avg(t.seconds) FROM albums al JOIN tracks t ON t.album_id = al.id`,
		`SELECT min(t.title), max(t.title), t.id FROM albums al JOIN tracks t ON t.album_id = al.id`,
		`SELECT al.id, count(*) FROM albums al LEFT JOIN tracks t ON t.album_id = al.id GROUP BY al.id`,
		`SELECT al.id, json_group_array(t.id) FROM albums al JOIN tracks t ON t.album_id = al.id AND t.id < 200 GROUP BY al.id`,
		`SELECT d.k, count(*) FROM (SELECT json('[1]') AS j, album_id AS k FROM tracks) d JOIN albums al ON al.id = d.k GROUP BY d.k`,
		// Groups that do not arrive in scan order go through the sorter, whose
		// record carries only the columns read after the scan.
		`SELECT ar.name, sum(t.plays), ar.country FROM artists ar JOIN albums al ON al.artist_id = ar.id JOIN tracks t ON t.album_id = al.id GROUP BY ar.id`,
		`SELECT t.album_id, al.title, count(*), t.title FROM tracks t JOIN albums al ON al.id = t.album_id GROUP BY al.year`,
		`SELECT al.year, max(t.seconds), t.id, al.id FROM tracks t JOIN albums al ON al.id = t.album_id GROUP BY al.year ORDER BY 2 DESC, 1`,
		`SELECT t.rating, count(*), min(al.title) FROM tracks t JOIN albums al ON al.id = t.album_id GROUP BY t.rating`,
		`SELECT CASE WHEN t.id % 2 = 0 THEN t.seconds % 7 ELSE (t.seconds % 7) * 1.0 END AS k, count(*) FROM tracks t JOIN albums al ON al.id = t.album_id GROUP BY k`,
		`SELECT al.title, group_concat(t.id), count(*) FROM tracks t JOIN albums al ON al.id = t.album_id AND t.id < 300 GROUP BY al.title HAVING count(*) > 2`,
		`SELECT al.year % 5, sum(t.plays) FROM tracks t JOIN albums al ON al.id = t.album_id GROUP BY 1 DESC`,
		`SELECT al.artist_id, json_group_array(json(t.id)), al.year FROM tracks t JOIN albums al ON al.id = t.album_id AND t.id < 120 GROUP BY al.artist_id`,
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
	read := func(db *sql.DB, q string) (string, error) {
		rows, err := db.Query(q)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return "", err
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			out = append(out, fmt.Sprint(vals))
		}
		return fmt.Sprint(out), rows.Err()
	}
	for _, phase := range []string{"written", "after VACUUM"} {
		if phase == "after VACUUM" {
			if _, err := m.Exec("VACUUM"); err != nil {
				t.Fatal(err)
			}
		}
		for _, q := range queries {
			want, werr := read(c, q)
			got, gerr := read(m, q)
			if werr != nil || gerr != nil {
				if (werr == nil) != (gerr == nil) {
					t.Errorf("%s, %s: C err %v, musql err %v", phase, q, werr, gerr)
				}
				continue
			}
			if want != got {
				t.Errorf("%s, %s:\n  C     %.300s\n  musql %.300s", phase, q, want, got)
			}
		}
	}
}
