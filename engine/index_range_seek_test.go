package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestIndexRangeSeekMatchesScan: a range on an indexed column ("a > 400", "a
// BETWEEN 10 AND 20") answered from the segments' column indexes (#261) gives
// what the scan gives, rows and order, over INTEGER and REAL columns, a
// column of mixed kinds no index covers, bounds of every kind, and rows
// written after the segments were made.
func TestIndexRangeSeekMatchesScan(t *testing.T) {
	queries := []string{
		`SELECT id FROM t WHERE a > 480`,
		`SELECT id, a FROM t WHERE a >= 400 AND a < 410`,
		`SELECT id FROM t WHERE a BETWEEN 10 AND 12`,
		`SELECT id FROM t WHERE 495 < a`,
		`SELECT id FROM t WHERE 5 >= a`,
		`SELECT id FROM t WHERE a > 490.5`,
		`SELECT id FROM t WHERE a <= 2.5 AND a > 0.5`,
		`SELECT id FROM t WHERE a > '497'`,
		`SELECT id FROM t WHERE a > 'x'`,
		`SELECT id FROM t WHERE a < -1`,
		`SELECT id FROM t WHERE a > NULL`,
		`SELECT id FROM t WHERE a > 490 ORDER BY a DESC, id`,
		`SELECT id FROM t WHERE a BETWEEN 100 AND 102 ORDER BY id DESC`,
		`SELECT id, s FROM t WHERE a > 495 LIMIT 5`,
		`SELECT count(*), sum(r) FROM t WHERE a BETWEEN 200 AND 210`,
		`SELECT id FROM t WHERE r BETWEEN 2.5 AND 30`,
		`SELECT id FROM t WHERE r > 1100 ORDER BY r`,
		`SELECT id FROM t WHERE r < 1 AND a = 2`,
		`SELECT id FROM t WHERE m > 2990`,
		`SELECT id FROM t WHERE m BETWEEN 10 AND 20`,
		`SELECT id FROM t WHERE s > 's2995'`,
		`SELECT id FROM t WHERE a > 498 AND s <> 's1999'`,
		`SELECT id FROM t WHERE a > ? AND a < ?`,
		`SELECT id FROM t WHERE a BETWEEN ? AND ?`,
		`SELECT id FROM t WHERE a NOT BETWEEN 1 AND 498`,
		`SELECT id FROM t WHERE a > 400 OR a < 2`,
	}
	args := []Value{{Typ: Int, I: 300}, {Typ: Int, I: 303}}
	const fill = `INSERT INTO t SELECT value, value %% 500, value * 0.37,
		CASE value %% 5 WHEN 0 THEN value WHEN 1 THEN value * 1.5 WHEN 2 THEN 'm' || value WHEN 3 THEN NULL ELSE value END,
		's' || value FROM generate_series(%d, %d)`
	for _, layout := range []string{"segments", "both", "delta"} {
		t.Run(layout, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "r.musq")
			db, err := Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { db.Close() }()
			for _, q := range []string{
				`CREATE TABLE t(id INTEGER PRIMARY KEY, a INT, r REAL, m, s TEXT)`,
				`CREATE INDEX ta ON t(a)`, `CREATE INDEX tr ON t(r)`, `CREATE INDEX tm ON t(m)`, `CREATE INDEX ts ON t(s)`,
				fmt.Sprintf(fill, 1, 3000),
			} {
				if err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if layout != "delta" {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if db, err = OpenWrite(path); err != nil {
					t.Fatal(err)
				}
			}
			if layout == "both" {
				for _, q := range []string{
					fmt.Sprintf(fill, 3001, 3040),
					`UPDATE t SET a = 499 WHERE id IN (5, 6, 7)`,
					`UPDATE t SET a = 1 WHERE a = 481`,
					`DELETE FROM t WHERE id IN (499, 999, 1499)`,
					`UPDATE t SET a = 'big' WHERE id = 10`,
				} {
					if err := db.Exec(q); err != nil {
						t.Fatal(q, err)
					}
				}
				if _, err := db.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			run := func(off bool) []string {
				saved := indexSeeksOffForTest
				indexSeeksOffForTest = off
				defer func() { indexSeeksOffForTest = saved }()
				var out []string
				for _, q := range queries {
					var a []Value
					if containsParam(q) {
						a = args
					}
					_, rows, err := db.Query(fmt.Sprintf("%s -- %v", q, off), a)
					out = append(out, fmt.Sprint(rows, err))
				}
				return out
			}
			want := run(true)
			before := segRangeSeeksServed.Load()
			got := run(false)
			if layout != "delta" && segRangeSeeksServed.Load() == before {
				t.Fatal("no range seek was served: the test checks the scan against itself")
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s:\n seek %.300s\n scan %.300s", queries[i], got[i], want[i])
				}
			}
		})
	}
}
