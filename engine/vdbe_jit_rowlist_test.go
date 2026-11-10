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
