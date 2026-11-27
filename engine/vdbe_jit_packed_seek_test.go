package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestVMJITPackedSortedSeek: a sorted seek keyed on two INTEGER columns
// (TPC-DS q75's store_sales probe, ANALYZEd) whose first column's runs are
// long is made natively through an index of packed keys, each probe finding
// exactly its stretch, rather than leaving native code for the VDBE on every
// probe. Every answer is the VDBE's: under an automatic index order and with
// none, NULL further keys, keys no row holds or wider than any row's, a
// further key that is a REAL, and a LEFT JOIN.
func TestVMJITPackedSortedSeek(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	path := filepath.Join(t.TempDir(), "p.musq")
	buildDB(t, path,
		`CREATE TABLE dd(dsk INTEGER, yr INTEGER)`,
		`CREATE TABLE it(isk INTEGER, cat TEXT)`,
		`CREATE TABLE ss(dsk INTEGER, isk INTEGER, tick INTEGER, q INTEGER)`,
		`CREATE TABLE sr(tick INTEGER, isk INTEGER, rq INTEGER)`,
		`INSERT INTO dd SELECT value, 1998 + value / 365 FROM generate_series(1, 3000)`,
		`INSERT INTO it SELECT value, CASE WHEN value % 5 = 0 THEN 'Books' ELSE 'x' END FROM generate_series(1, 500)`,
		`INSERT INTO ss SELECT 1000 + value % 200, CASE WHEN value % 97 = 0 THEN NULL ELSE (value * 7) % 600 END, value, value % 13
		   FROM generate_series(1, 60000)`,
		`INSERT INTO sr SELECT value * 3, (value * 21) % 600, value % 4 FROM generate_series(1, 5000)`,
		`ANALYZE`)
	execDB(t, path, `VACUUM`)
	queries := []string{
		`SELECT count(*), sum(q) FROM ss JOIN it ON it.isk = ss.isk JOIN dd ON dd.dsk = ss.dsk WHERE it.cat = 'Books'`,
		`SELECT count(*) FROM ss JOIN it ON it.isk = ss.isk JOIN dd ON dd.dsk = ss.dsk WHERE it.cat = 'Books'`,
		`SELECT dd.yr, count(*), sum(q - coalesce(sr.rq, 0)) FROM ss JOIN it ON it.isk = ss.isk JOIN dd ON dd.dsk = ss.dsk
		   LEFT JOIN sr ON sr.tick = ss.tick AND sr.isk = ss.isk WHERE it.cat = 'Books' GROUP BY dd.yr`,
		`SELECT count(*), sum(q) FROM ss JOIN it ON it.isk * 1.5 = ss.isk JOIN dd ON dd.dsk = ss.dsk WHERE it.cat = 'Books'`,
		`SELECT count(*), sum(q) FROM ss JOIN it ON it.isk + 100000 = ss.isk JOIN dd ON dd.dsk = ss.dsk WHERE it.cat = 'Books'`,
		`SELECT count(*), sum(q) FROM ss JOIN it ON CASE WHEN it.isk % 3 = 0 THEN NULL ELSE it.isk END = ss.isk
		   JOIN dd ON dd.dsk = ss.dsk WHERE it.cat = 'Books'`,
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	run := func(on bool) (out []string, enters []int64) {
		old := vmJITEnabled
		vmJITEnabled = on
		defer func() { vmJITEnabled = old }()
		for _, q := range queries {
			before := vmJITCursorEnters.Load()
			_, rows, err := p.QueryArgs(fmt.Sprintf("%s -- %v", q, on), nil)
			out, enters = append(out, fmt.Sprint(rows, err)), append(enters, vmJITCursorEnters.Load()-before)
		}
		return out, enters
	}
	want, _ := run(false)
	// With no GC cycle running, whose write barrier sends every seek back
	// to the VDBE while it lasts.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	vmJITCountCursorEnters = true
	defer func() { vmJITCountCursorEnters = false }()
	got, enters := run(true)
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s:\n vdbe %.300s\n jit  %.300s", queries[i], want[i], got[i])
		}
		// 3,000 dates by 100 Books items is 300,000 probes per query. A
		// further key that is a REAL takes the VDBE's conversions, at
		// each probe.
		if segCursorsHere() && i != 3 && enters[i] > 20000 {
			t.Errorf("%s: %d entries into native code: it leaves for the VDBE at each probe", queries[i], enters[i])
		}
	}
}

// TestFilterShiftNarrowsACrowdedPrefix: a packed index's filter blocks follow
// the key's prefix, all but its last column, unless one prefix holds so many
// keys that its block would fill; then the prefix takes the last column's top
// bits too, until no run is longer than a block holds.
func TestFilterShiftNarrowsACrowdedPrefix(t *testing.T) {
	keys := func(prefixes int64, crowded int) []int64 {
		var out []int64
		for p := range prefixes {
			n := 4
			if p == 2 {
				n = crowded
			}
			for i := range int64(n) {
				out = append(out, p<<20|i)
			}
		}
		return out
	}
	if got := filterShift(keys(6, 4), 20); got != 20 {
		t.Errorf("even prefixes: shift %d, want the last column's width 20", got)
	}
	if got := filterShift(keys(6, 1000), 20); got != 5 {
		t.Errorf("one prefix of 1000 keys: shift %d, want 5, runs of 32", got)
	}
	if got := filterShift(keys(200, 40), 20); got != 20 {
		t.Errorf("one prefix of 40 keys among 840: shift %d, want 20", got)
	}
}
