package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestVMJITSortSeekMore: TPC-DS q71's probe of date_dim, ANALYZEd: the
// constant terms pin the cursor to a few rows, built once, and each fact row
// seeks them on its date and the same constants as further keys. Native code
// serves the run of the date alone, whose rows the loop tests, rather than
// leaving for the VDBE at each probe. Every answer is the VDBE's: with dates
// that repeat, NULL further keys, further keys no row holds, and a LEFT JOIN.
func TestVMJITSortSeekMore(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path,
		`CREATE TABLE dd(dsk INTEGER, moy INTEGER, yr INTEGER)`,
		`CREATE TABLE ws(dsk INTEGER, amt INTEGER)`,
		`INSERT INTO dd SELECT value, 1 + value % 12, 1998 + value / 365 FROM generate_series(1, 3000)`,
		`INSERT INTO dd SELECT value, 11, 1999 FROM generate_series(400, 420)`,
		`INSERT INTO ws SELECT CASE WHEN value % 89 = 0 THEN NULL ELSE value % 3100 END, value % 17 FROM generate_series(1, 60000)`,
		`ANALYZE`)
	execDB(t, path, `VACUUM`)
	queries := []string{
		`SELECT count(*), sum(amt) FROM ws, dd WHERE dd.dsk = ws.dsk AND moy = 11 AND yr = 1999`,
		`SELECT count(*), sum(amt) FROM ws, dd WHERE dd.dsk = ws.dsk AND moy = 13 AND yr = 1999`,
		`SELECT count(*), sum(amt) FROM ws, dd WHERE dd.dsk = ws.dsk AND moy = 11 AND yr = CASE WHEN amt > 20 THEN 1 END`,
		`SELECT yr, count(*), sum(amt) FROM ws, dd WHERE dd.dsk = ws.dsk AND moy = 2 GROUP BY yr`,
		`SELECT count(*), count(dd.dsk), sum(amt) FROM ws LEFT JOIN dd ON dd.dsk = ws.dsk AND moy = 11 AND yr = 1999`,
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
	// 60,000 fact rows, each a probe.
	if segCursorsHere() && enters[0] > 2000 {
		t.Errorf("%s: %d entries into native code: it leaves for the VDBE at each probe", queries[0], enters[0])
	}
}
