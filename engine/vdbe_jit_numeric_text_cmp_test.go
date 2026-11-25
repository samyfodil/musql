package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestVMJITNumericTextCmp: TPC-DS q16's d_date <= CAST('2002-04-02' AS date),
// a TEXT date against the INTEGER 2002 under NUMERIC affinity. A TEXT the
// affinity leaves a TEXT is greater than any number, which native code now
// answers rather than leaving for the VDBE at each row. Every answer is the
// VDBE's: TEXTs that are numbers ('12', ' 7 ', '1e3', '-.5'), REALs, NULLs,
// BLOBs, every comparison, and the number on either side.
func TestVMJITNumericTextCmp(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path,
		`CREATE TABLE dd(sk INTEGER, d date, txt TEXT, num INTEGER, r REAL)`,
		`INSERT INTO dd SELECT value, printf('%04d-%02d-%02d', 1998 + value % 8, 1 + value % 12, 1 + value % 28),
			CASE value % 9 WHEN 0 THEN '12' WHEN 1 THEN ' 7 ' WHEN 2 THEN '1e3' WHEN 3 THEN '-.5' WHEN 4 THEN 'abc'
				WHEN 5 THEN NULL WHEN 6 THEN '2002-03-01' WHEN 7 THEN X'3132' ELSE '' END,
			CASE value % 7 WHEN 0 THEN NULL ELSE value % 2005 END, value / 3.0 FROM generate_series(1, 20000)`,
		`INSERT INTO dd VALUES (20001, 2002, '2002', 2002, 2002.0), (20002, NULL, NULL, NULL, NULL), (20003, X'00', '9 9', 1, -0.0)`,
		`ANALYZE`)
	execDB(t, path, `VACUUM`)
	queries := []string{
		`SELECT count(*), sum(sk) FROM dd WHERE d BETWEEN '2002-02-01' AND CAST('2002-04-02' AS date)`,
		`SELECT count(*), sum(sk) FROM dd WHERE d <= CAST('2002-04-02' AS date)`,
		`SELECT count(*) FROM dd WHERE d > 2002`,
		`SELECT count(*) FROM dd WHERE 2002 < d`,
		`SELECT count(*) FROM dd WHERE d = 2002`,
		`SELECT count(*) FROM dd WHERE d <> 2002.5`,
		`SELECT count(*), sum(txt < num), sum(txt >= num), sum(num > txt), sum(txt = num), sum(txt <> r), sum(r <= txt) FROM dd`,
		`SELECT sk, txt < num, txt > r, d >= 2002 FROM dd WHERE sk % 997 = 0 OR sk > 20000 ORDER BY sk`,
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
	// With no GC cycle running, whose write barrier sends every row back to
	// the VDBE while it lasts.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	vmJITCountCursorEnters = true
	defer func() { vmJITCountCursorEnters = false }()
	got, enters := run(true)
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s:\n vdbe %.400s\n jit  %.400s", queries[i], want[i], got[i])
		}
	}
	t.Logf("entries %v", enters)
	if segCursorsHere() && (enters[0] == 0 || enters[0] > 2000) {
		t.Errorf("%s: %d entries into native code, want some and not one per row", queries[0], enters[0])
	}
}
