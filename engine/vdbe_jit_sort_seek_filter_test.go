package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestVMJITSortedSeekRowFilter: TPC-DS q99's probe of date_dim, a sorted seek
// whose rewind carries a row filter (segRowFilter) for a range on another
// column. The filter is a hint for scans, so native code seeks as it would
// without one, rather than leaving for the VDBE at each probe. Every answer is
// the VDBE's: ranges that keep some rows, none and all, NULL keys, and a LEFT
// JOIN.
func TestVMJITSortedSeekRowFilter(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path,
		`CREATE TABLE w(wk INTEGER, nm TEXT)`,
		`CREATE TABLE sm(sk INTEGER, ty TEXT)`,
		`CREATE TABLE cc(ck INTEGER, nm TEXT)`,
		`CREATE TABLE dd(dsk INTEGER, ms INTEGER)`,
		`CREATE TABLE f(dsk INTEGER, wk INTEGER, sk INTEGER, ck INTEGER, amt INTEGER)`,
		`INSERT INTO w SELECT value, 'w' || value FROM generate_series(1, 5)`,
		`INSERT INTO sm SELECT value, 't' || (value % 4) FROM generate_series(1, 20)`,
		`INSERT INTO cc SELECT value, 'c' || value FROM generate_series(1, 6)`,
		`INSERT INTO dd SELECT value, value / 30 FROM generate_series(1, 3000)`,
		`INSERT INTO f SELECT CASE WHEN value % 97 = 0 THEN NULL ELSE value % 3100 END, 1 + value % 5, 1 + value % 20, 1 + value % 6, value % 17
			FROM generate_series(1, 60000)`,
		`ANALYZE`)
	execDB(t, path, `VACUUM`)
	const q = `SELECT %s FROM f, w, sm, cc, dd WHERE dd.ms BETWEEN %s AND f.dsk = dd.dsk AND f.wk = w.wk AND f.sk = sm.sk AND f.ck = cc.ck%s`
	queries := []string{
		fmt.Sprintf(q, "count(*), sum(amt)", "10 AND 21", ""),
		fmt.Sprintf(q, "count(*), sum(amt)", "500 AND 600", ""),
		fmt.Sprintf(q, "count(*), sum(amt)", "0 AND 600", ""),
		fmt.Sprintf(q, "w.nm, sm.ty, count(*), sum(amt)", "10 AND 21", " GROUP BY 1, 2 ORDER BY 1, 2"),
		`SELECT count(*), count(dd.dsk), sum(amt) FROM f, w, sm, cc LEFT JOIN dd ON dd.ms BETWEEN 10 AND 21 AND f.dsk = dd.dsk
			WHERE f.wk = w.wk AND f.sk = sm.sk AND f.ck = cc.ck`,
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
	}
	// 60,000 fact rows, each a probe: one entry each when they leave.
	if segCursorsHere() && enters[0] > 2000 {
		t.Errorf("%s: %d entries into native code: it leaves for the VDBE at each probe", queries[0], enters[0])
	}
}
