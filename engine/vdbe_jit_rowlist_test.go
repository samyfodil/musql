package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestVMJITSegCapableRowList: a join whose cursors could read their tables'
// segments in place, but are handed over on their row lists (both are
// materialized), reads every kind of column -- INTEGER, REAL, TEXT, NULL --
// several to a run and again after each move, as the VDBE reads them (#128).
func TestVMJITSegCapableRowList(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	countSyncs(t)
	path := filepath.Join(t.TempDir(), "rowlist.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE o (id INTEGER PRIMARY KEY, k INTEGER, r REAL, s TEXT)`,
		`CREATE TABLE i (id INTEGER PRIMARY KEY, fk INTEGER, r REAL, s TEXT, n)`,
		`CREATE INDEX ifk ON i(fk)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	// Enough rows that closing folds them into segments, so the columns read
	// as the kinds the segments hold (segColKind).
	for _, q := range []string{
		`INSERT INTO o SELECT value, value, value / 4.0, 'o-' || value FROM generate_series(1, 3)`,
		`INSERT INTO i SELECT value, value % 4, value * 1.5, 'payload-' || value, CASE WHEN value % 3 = 0 THEN NULL ELSE value END FROM generate_series(1, 40000)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	saved := joinSeekDisabled
	joinSeekDisabled = true
	defer func() { joinSeekDisabled = saved }()
	queries := []string{
		`SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k`,
		`SELECT o.s, i.s, i.r, o.r FROM o JOIN i ON i.fk = o.k WHERE i.r > o.r`,
		`SELECT i.n, i.s, o.k, i.fk, i.s FROM o JOIN i ON i.fk = o.k AND i.n IS NULL`,
		`SELECT count(*), sum(i.r), group_concat(i.s) FROM o JOIN i ON i.fk = o.k AND o.s < i.s`,
	}
	// A pager of its own each time, so no program compiled with the JIT off
	// is reused with it on.
	run := func(on bool) []string {
		old := vmJITEnabled
		vmJITEnabled = on
		defer func() { vmJITEnabled = old }()
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		var out []string
		for _, q := range queries {
			_, rows, err := p.QueryArgs(q, nil)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			out = append(out, fmt.Sprint(rows))
		}
		return out
	}
	want := run(false)
	jitPointerModes(t, func(t *testing.T, mode string) {
		before := vmJITCursorSyncs.Load()
		got := run(true)
		if vmJITCursorSyncs.Load() == before {
			t.Fatalf("%s: no entry advanced a cursor natively: the test checks the VDBE against itself", mode)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: %s\nvdbe %.300s\njit  %.300s", mode, queries[i], want[i], got[i])
			}
		}
	})
}

// TestVMJITSegCapableUnpredicted: the same, with columns no segment kind is
// predicted for -- every column of a table whose rows are all in the delta,
// and a column of mixed kinds or a BLOB in one with segments -- read with
// predicted ones in the same runs: on row lists, on segments (a scan's
// cursor), and on both when rows written after the segments were made join
// them (#128).
func TestVMJITSegCapableUnpredicted(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	countSyncs(t)
	saved := joinSeekDisabled
	joinSeekDisabled = true
	defer func() { joinSeekDisabled = saved }()
	queries := []string{
		`SELECT a.id, b.id FROM u a JOIN u b ON b.k = a.k WHERE a.id <= 40`,
		`SELECT a.s, b.s, a.m, b.r, a.b, b.m, a.k FROM u a JOIN u b ON b.k = a.k AND b.m > a.m WHERE a.id <= 40`,
		`SELECT count(*), sum(b.r), group_concat(b.s), max(b.b) FROM u a JOIN u b ON b.k = a.k AND a.s < b.s WHERE a.id <= 60`,
		`SELECT a.m, b.m, b.k, a.r FROM u a, u b WHERE a.id <= 30 AND b.id <= 300 AND a.m = b.m`,
		// One cursor, on its segments where it has them.
		`SELECT k, m, s, b, r, m FROM u WHERE r > 100`,
		`SELECT m, b, k, s, b FROM u WHERE k <> 2`,
		`SELECT s, k, r, s FROM u WHERE r < 700 AND k > 0`,
		`SELECT m, b, s, k FROM u WHERE m IS NOT NULL AND b IS NOT NULL`,
	}
	const fill = `INSERT INTO u SELECT value, value %% 5, 's-' || value, value * 0.25,
		CASE value %% 4 WHEN 0 THEN value WHEN 1 THEN 'm' || value WHEN 2 THEN NULL ELSE value * 1.5 END,
		CASE WHEN value %% 3 = 0 THEN x'00ff' || value ELSE NULL END FROM generate_series(%d, %d)`
	for _, layout := range []string{"delta", "segments", "both"} {
		t.Run(layout, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "u.musq")
			db, err := Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { db.Close() }()
			for _, q := range []string{`CREATE TABLE u(id INTEGER PRIMARY KEY, k INT, s TEXT, r REAL, m, b BLOB)`, fmt.Sprintf(fill, 1, 3000)} {
				if err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if layout == "segments" || layout == "both" {
				// Closing folds the rows into segments.
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if db, err = OpenWrite(path); err != nil {
					t.Fatal(err)
				}
			}
			if layout == "both" {
				if err := db.Exec(fmt.Sprintf(fill, 3001, 3100)); err != nil {
					t.Fatal(err)
				}
			}
			run := func(on bool) []string {
				old := vmJITEnabled
				vmJITEnabled = on
				defer func() { vmJITEnabled = old }()
				var out []string
				for _, q := range queries {
					// The JIT's run and the VDBE's compile apart.
					_, rows, err := db.Query(fmt.Sprintf("%s -- %v", q, on), nil)
					if err != nil {
						t.Fatalf("%s: %v", q, err)
					}
					out = append(out, fmt.Sprint(rows))
				}
				return out
			}
			want := run(false)
			jitPointerModes(t, func(t *testing.T, mode string) {
				before := vmJITCursorSyncs.Load()
				got := run(true)
				if vmJITCursorSyncs.Load() == before {
					t.Fatalf("%s: no entry advanced a cursor natively: the test checks the VDBE against itself", mode)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("%s: %s\nvdbe %.300s\njit  %.300s", mode, queries[i], want[i], got[i])
					}
				}
			})
		})
	}
}
