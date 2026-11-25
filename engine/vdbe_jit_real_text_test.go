package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestVMJITRealText: TPC-DS q04's year_total, arithmetic over VARCHAR prices
// ('12.34'). Native code reads a TEXT that spells a REAL plainly as that REAL,
// rather than leaving for the VDBE at each row. Every answer is the VDBE's:
// INTEGER spellings, '-0.00', spaces, exponents, more digits than a double
// holds, NULLs, a column of no type, and a divide by a zero spelled as TEXT.
func TestVMJITRealText(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path,
		`CREATE TABLE ss(c INTEGER, lp VARCHAR, wc VARCHAR, dc VARCHAR, sp VARCHAR, mix)`,
		`INSERT INTO ss SELECT value % 50, printf('%d.%02d', value % 300, value % 100), printf('%d.%02d', value % 40, value % 97),
			printf('%d.%02d', value % 7, value % 13), printf('%d.%02d', value % 90, value % 11),
			CASE value % 6 WHEN 0 THEN 2 WHEN 1 THEN 2.5 WHEN 2 THEN '3.25' ELSE NULL END FROM generate_series(1, 20000)`,
		`INSERT INTO ss VALUES (1, '12', '3', '1', '2', '7'), (2, '-0.00', '0.00', '-0.0', '0.', '-0.00'),
			(3, ' 1.5', '1.5e2', '1.000000000000000000000001', '9007199254740993.5', 'x'),
			(4, NULL, '1.5', NULL, '2.5', NULL), (5, '.5', '-.25', '1.', '-3.', '.5')`,
		`ANALYZE`)
	execDB(t, path, `VACUUM`)
	queries := []string{
		`SELECT c, sum(((lp - wc) - dc) + sp / 2), count(*) FROM ss GROUP BY c ORDER BY c`,
		`SELECT sum(lp * wc), sum(lp - mix), sum(mix * 2), sum(wc / dc) FROM ss`,
		`SELECT c, lp - wc, lp + 0, 0 - lp, lp / sp, typeof(lp - wc) FROM ss WHERE c < 6 AND rowid > 19990 OR rowid > 20000 ORDER BY rowid`,
		`SELECT sum(CASE WHEN lp - wc > 100 THEN 1 ELSE 0 END), max(lp - wc), min(dc * sp) FROM ss`,
		`SELECT 1 / (lp - lp), lp / '0.00' FROM ss WHERE rowid > 20000`,
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
	// 20,000 rows. On main native code is never entered: each row would
	// leave it at the first subtract.
	if segCursorsHere() && (enters[0] == 0 || enters[0] > 2000) {
		t.Errorf("%s: %d entries into native code, want some and not one per row", queries[0], enters[0])
	}
}
