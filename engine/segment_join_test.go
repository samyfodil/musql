package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSegJoinSessionFollowsWrites runs join aggregates between writes on one
// held session, where the join kernel reads the session's own store and keeps
// its reading between statements -- extending it when the only writes were
// appends -- and holds every answer to the same session's with the kernel's
// recognisers off. A write that is not an append, followed by one that is,
// before the next read, is the case an extension must refuse.
func TestSegJoinSessionFollowsWrites(t *testing.T) {
	setup := []string{
		`CREATE TABLE albums(id INTEGER PRIMARY KEY, year INTEGER NOT NULL, tag TEXT)`,
		`CREATE TABLE tracks(id INTEGER PRIMARY KEY, album_id INTEGER NOT NULL, seconds INTEGER NOT NULL, rating REAL NOT NULL)`,
		`CREATE INDEX tracks_album ON tracks(album_id)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 20) INSERT INTO albums SELECT i, 1990 + i % 3, CASE i % 3 WHEN 0 THEN 'abc' WHEN 1 THEN 'ABC' ELSE 'xyz' END FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 500) INSERT INTO tracks SELECT i, 1 + i % 21, 60 + i % 90, (i % 5) / 4.0 FROM n`,
	}
	queries := []string{
		`SELECT count(*), sum(t.seconds), total(t.rating) FROM albums al JOIN tracks t ON t.album_id = al.id`,
		`SELECT al.id, count(*), sum(t.seconds), avg(t.rating) FROM albums al JOIN tracks t ON t.album_id = al.id GROUP BY al.id`,
		`SELECT al.year, max(t.seconds), count(*) FROM tracks t JOIN albums al ON al.id = t.album_id GROUP BY al.year`,
		`SELECT al.year, count(*) FROM tracks t JOIN albums al ON al.id = t.album_id WHERE al.tag LIKE 'a%' GROUP BY al.year`,
	}
	writes := [][]string{
		{`INSERT INTO tracks(album_id, seconds, rating) VALUES (3, 1000, 0.5)`},
		{`INSERT INTO tracks(album_id, seconds, rating) VALUES (4, 7, 0.25)`, `INSERT INTO tracks(album_id, seconds, rating) VALUES (5, 9, 1)`},
		{`UPDATE tracks SET seconds = seconds * 2 WHERE album_id = 6`, `INSERT INTO tracks(album_id, seconds, rating) VALUES (6, 19, 0.5)`},
		{`DELETE FROM tracks WHERE id = 5`, `INSERT INTO tracks(album_id, seconds, rating) VALUES (6, 23, 0.5)`},
		{`INSERT INTO tracks(id, album_id, seconds, rating) VALUES (-3, 2, 5, 0.5)`, `INSERT INTO tracks(album_id, seconds, rating) VALUES (2, 6, 0.5)`},
		{`UPDATE albums SET year = 1 WHERE id = 2`, `INSERT INTO tracks(album_id, seconds, rating) VALUES (2, 8, 0.5)`},
		{`INSERT INTO tracks(album_id, seconds, rating) VALUES (9, 1, 0.125)`},
		// Rewriting a row this session appended keeps the overlay sorted; only
		// the count of writes that were not appends tells.
		{`UPDATE tracks SET seconds = 77777 WHERE id = (SELECT max(id) FROM tracks)`, `INSERT INTO tracks(album_id, seconds, rating) VALUES (9, 2, 0.25)`},
		{`DELETE FROM albums WHERE id = 9`},
		{`INSERT INTO tracks(album_id, seconds, rating) VALUES (10, 2, 0.5)`},
		// A LIKE's answer follows the setting, not only the rows.
		{`PRAGMA case_sensitive_like = 1`},
		{`PRAGMA case_sensitive_like = 0`},
	}
	run := func(off bool) []string {
		segPeepholesOffForTest = off
		defer func() { segPeepholesOffForTest = false }()
		db, err := OpenOrCreate(filepath.Join(t.TempDir(), "j.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		exec := func(s string) {
			if _, _, err := db.ExecArgs(s, nil); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		for _, s := range setup {
			exec(s)
		}
		db.CommitIfAutocommit()
		exec("BEGIN")
		var out []string
		read := func() {
			for _, q := range queries {
				_, rows, err := db.Query(q, nil)
				out = append(out, fmt.Sprint(rows, err))
			}
		}
		read()
		for _, w := range writes {
			for _, s := range w {
				exec(s)
			}
			read()
		}
		exec("COMMIT")
		read()
		return out
	}
	SegJoinHitsForTest()
	want := run(true)
	if SegJoinHitsForTest() != 0 {
		t.Fatal("the reference run used the join kernel")
	}
	got := run(false)
	if SegJoinHitsForTest() == 0 {
		t.Fatal("the join kernel answered nothing")
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("read %d (query %d):\n  loop   %.300s\n  kernel %.300s", i, i%len(queries), want[i], got[i])
		}
	}
}

// TestSegJoinMultiOrMatchesLoop holds the join kernel to the loop over a
// MULTI_OR inner level, whose rows the loop orders per outer row by the
// first OR term each satisfies, then that term's index: answers whose order
// shows it -- group_concat, a bare column, ORDER BY ... LIMIT ties -- must
// match exactly, with the kernel answering.
func TestSegJoinMultiOrMatchesLoop(t *testing.T) {
	setup := []string{
		`CREATE TABLE a(id INTEGER PRIMARY KEY, z INTEGER, s TEXT)`,
		`CREATE TABLE b(id INTEGER PRIMARY KEY, k INTEGER, x INTEGER, y INTEGER, w TEXT)`,
		`CREATE INDEX b_x ON b(x)`, `CREATE INDEX b_y ON b(y DESC)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 40) INSERT INTO a SELECT i, i % 9, CASE i % 3 WHEN 0 THEN 'p' ELSE 'q' END FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 6000) INSERT INTO b SELECT i, i % 41, (i * 7) % 13, (i * 11) % 17, 'w' || (i % 5) FROM n`,
		`DELETE FROM b WHERE id % 97 = 0`,
		`ANALYZE`,
	}
	queries := []string{
		`SELECT a.id, group_concat(b.id) FROM a JOIN b ON b.k = a.id WHERE b.x = 1 OR b.y = a.z GROUP BY a.id`,
		`SELECT a.id, group_concat(b.id), b.w FROM a JOIN b ON b.k = a.id WHERE b.x = 1 OR b.y = a.z GROUP BY a.id`,
		`SELECT a.id, json_group_array(b.y) FROM a JOIN b ON b.k = a.id WHERE b.y = a.z OR b.x = 4 GROUP BY a.id`,
		`SELECT a.id, b.id FROM a JOIN b ON b.k = a.id WHERE b.x = 1 OR b.y = a.z ORDER BY a.id LIMIT 30`,
		`SELECT count(*), group_concat(b.id, ',') FROM a JOIN b ON b.k = a.id WHERE b.x = 3 OR b.y = a.z`,
		// Range terms: each pass's rows in its index's order, b_y descending.
		`SELECT a.id, group_concat(b.id) FROM a JOIN b ON b.k = a.id WHERE b.x = 1 OR b.y BETWEEN a.z AND a.z + 1 GROUP BY a.id`,
		`SELECT a.id, group_concat(b.id) FROM a JOIN b ON b.k = a.id WHERE b.x IN (1, 2) OR b.y = a.z GROUP BY a.id`,
		`SELECT a.s, b.id, count(*) FROM a JOIN b ON b.k = a.id WHERE b.x = 2 OR b.y = a.z GROUP BY a.s`,
		`SELECT a.id, b.id, b.x FROM a JOIN b ON b.k = a.id WHERE b.x = 1 OR b.y = a.z ORDER BY a.z LIMIT 7`,
		`SELECT a.id, b.id FROM a JOIN b ON b.k = a.id WHERE b.y = a.z OR b.x = a.z ORDER BY b.w DESC LIMIT 9 OFFSET 2`,
		`SELECT a.z, json_group_array(b.id) FROM a JOIN b ON b.k = a.id WHERE (b.x = 1 OR b.y = a.z) AND b.id > 100 GROUP BY a.z`,
	}
	run := func(off bool) []string {
		segPeepholesOffForTest = off
		defer func() { segPeepholesOffForTest = false }()
		db, err := OpenOrCreate(filepath.Join(t.TempDir(), "m.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range setup {
			if _, _, err := db.ExecArgs(s, nil); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
			db.CommitIfAutocommit()
		}
		var out []string
		for _, q := range queries {
			_, rows, err := db.Query(q, nil)
			out = append(out, fmt.Sprint(rows, err))
		}
		return out
	}
	SegJoinHitsForTest()
	want := run(true)
	got := run(false)
	if SegJoinHitsForTest() == 0 {
		t.Fatal("the join kernel answered nothing")
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s:\n  loop   %.300s\n  kernel %.300s", queries[i], want[i], got[i])
		}
	}
}
