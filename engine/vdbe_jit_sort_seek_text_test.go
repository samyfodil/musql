package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestVMJITSortSeekTextConst: TPC-DS q27's probe of store, ANALYZEd: each
// outer row seeks the inner table's sorted rows on one TEXT constant. Native
// code serves that seek with the run the VDBE's own search found for the
// constant, rather than leaving for the VDBE at each probe. Every answer is
// the VDBE's: under a NOCASE column, for a constant no row holds, one a
// numeric column takes as an INTEGER, a bound parameter, a LEFT JOIN and a
// GROUP BY.
func TestVMJITSortSeekTextConst(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path,
		`CREATE TABLE cd(k INTEGER, g TEXT)`,
		`CREATE TABLE st(sk INTEGER, state TEXT, nc TEXT COLLATE NOCASE, num INTEGER)`,
		`CREATE TABLE f(cd INTEGER, s INTEGER, q INTEGER)`,
		`INSERT INTO cd SELECT value, CASE value % 3 WHEN 0 THEN 'M' ELSE 'F' END FROM generate_series(1, 3000)`,
		`INSERT INTO st SELECT value, CASE value % 4 WHEN 0 THEN 'TN' WHEN 1 THEN NULL WHEN 2 THEN 'tn' ELSE 'GA' END,
			CASE value % 4 WHEN 0 THEN 'TN' WHEN 1 THEN NULL WHEN 2 THEN 'tn' ELSE 'GA' END, value % 5 FROM generate_series(1, 40)`,
		`INSERT INTO f SELECT value % 3001, value % 41, value % 13 FROM generate_series(1, 30000)`,
		`ANALYZE`)
	execDB(t, path, `VACUUM`)
	const q = `SELECT count(*), sum(q) FROM f, cd, st WHERE f.cd = cd.k AND f.s = st.sk AND cd.g = 'M' AND %s`
	queries := []string{
		fmt.Sprintf(q, `st.state = 'TN'`),
		fmt.Sprintf(q, `st.nc = 'TN'`),
		fmt.Sprintf(q, `st.state = 'XX'`),
		fmt.Sprintf(q, `st.num = '3'`),
		fmt.Sprintf(q, `st.num = 3`),
		fmt.Sprintf(q, `st.state = ?`),
		`SELECT count(*), count(st.sk), sum(q) FROM f, cd LEFT JOIN st ON f.s = st.sk AND st.state = 'TN' WHERE f.cd = cd.k AND cd.g = 'M'`,
		`SELECT st.state, count(*) FROM cd, st WHERE cd.g = 'M' AND st.state = 'GA' AND cd.k % 7 = st.sk % 7 GROUP BY 1`,
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
			_, rows, err := p.QueryArgs(fmt.Sprintf("%s -- %v", q, on), []Value{{Typ: Text, S: []byte("TN")}})
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
	// 1,000 outer rows, each a probe of st: 1,469 entries when each leaves.
	for _, i := range []int{0, 5} {
		if segCursorsHere() && enters[i] > 700 {
			t.Errorf("%s: %d entries into native code: it leaves for the VDBE at each probe", queries[i], enters[i])
		}
	}
}
