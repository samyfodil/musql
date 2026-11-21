package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSegJoinEmptyLevelMatchesLoop: a join in which a level that is not
// optional lets no row through its own terms has no joined row, and the
// kernel answers it from those terms alone -- grouped, whole-join, ORDER BY
// with LIMIT, over two and three levels, with the empty level first, inner or
// last -- as the loop does. An optional level with no row still gives every
// outer row its NULL row.
func TestSegJoinEmptyLevelMatchesLoop(t *testing.T) {
	setup := []string{
		`CREATE TABLE a(id INTEGER PRIMARY KEY, z INTEGER, s TEXT)`,
		`CREATE TABLE b(id INTEGER PRIMARY KEY, k INTEGER, x INTEGER, y INTEGER, w TEXT)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, v TEXT)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 40) INSERT INTO a SELECT i, i % 9, CASE i % 3 WHEN 0 THEN 'p' ELSE 'q' END FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 3000) INSERT INTO b SELECT i, i % 41, (i * 7) % 13, (i * 11) % 17, 'w' || (i % 5) FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < 12) INSERT INTO c SELECT i, CASE i % 2 WHEN 0 THEN 'p' ELSE 'q' END FROM n`,
	}
	queries := []string{
		`SELECT a.z, count(*), sum(b.x * b.y) FROM a JOIN b ON b.k = a.id WHERE a.z > 100 GROUP BY a.z`,
		`SELECT a.z, count(*), sum(b.x * b.y) FROM a JOIN b ON b.k = a.id WHERE b.x > 50 GROUP BY a.z`,
		`SELECT a.s, total(b.y), group_concat(b.id) FROM b JOIN a ON a.id = b.k WHERE a.s = 'none' GROUP BY a.s`,
		`SELECT count(*), sum(b.x), max(a.s) FROM a JOIN b ON b.k = a.id WHERE b.w = 'none'`,
		`SELECT count(*), sum(b.x), max(a.s) FROM a JOIN b ON b.k = a.id WHERE a.z IN (20, 21) AND b.y < 3`,
		`SELECT c.v, count(*), sum(b.x) FROM a JOIN b ON b.k = a.id JOIN c ON c.id = b.x WHERE c.v = 'r' GROUP BY c.v`,
		`SELECT a.id, b.id FROM a JOIN b ON b.k = a.id WHERE a.z < 0 ORDER BY b.y, b.id LIMIT 5`,
		`SELECT a.id, count(b.id), group_concat(b.id) FROM a LEFT JOIN b ON b.k = a.id AND b.x > 50 GROUP BY a.id`,
		`SELECT count(*), count(b.id) FROM a LEFT JOIN b ON b.k = a.id AND b.w = 'none'`,
		`SELECT a.z, count(*), sum(b.x) FROM a JOIN b ON b.k = a.id WHERE a.z > 3 GROUP BY a.z`,
	}
	run := func(off bool) []string {
		segPeepholesOffForTest = off
		defer func() { segPeepholesOffForTest = false }()
		db, err := OpenOrCreate(filepath.Join(t.TempDir(), "e.musq"))
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
	want := run(true)
	segJoinEmptyWalks.Store(0)
	got := run(false)
	if n := segJoinEmptyWalks.Load(); jitEnabled && n < 4 {
		t.Errorf("the kernel found %d joins empty from their levels' terms, want at least 4", n)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s:\n  loop   %.300s\n  kernel %.300s", queries[i], want[i], got[i])
		}
	}
}
