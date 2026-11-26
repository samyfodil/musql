package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestVMJITTextCastConst: #393's d_date BETWEEN '2002-02-01' AND
// CAST('2002-04-01' AS varchar). A TEXT cast leaves a TEXT literal's bytes as
// they are, which native code now does too, where it left the cast to the
// VDBE at every row. Every answer is the VDBE's: other spellings of TEXT
// affinity, a cast whose operand a CASE chooses, a number spelled as TEXT,
// and the cast on either side.
func TestVMJITTextCastConst(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	path := filepath.Join(t.TempDir(), "c.musq")
	buildDB(t, path,
		`CREATE TABLE dd(sk INTEGER, d varchar, n INTEGER)`,
		`INSERT INTO dd SELECT value, printf('%04d-%02d-%02d', 1998 + value % 8, 1 + value % 12, 1 + value % 28), value % 50
			FROM generate_series(1, 20000)`,
		`INSERT INTO dd VALUES (20001, 12, 1), (20002, NULL, 2), (20003, X'32303032', 3), (20004, '2002', 7)`)
	execDB(t, path, `VACUUM`)
	queries := []string{
		`SELECT count(*), sum(sk) FROM dd WHERE d BETWEEN '2002-02-01' AND CAST('2002-04-01' AS varchar)`,
		`SELECT count(*) FROM dd WHERE d <= CAST('2002-04-01' AS TEXT) AND d > CAST('2000' AS CLOB)`,
		`SELECT count(*) FROM dd WHERE CAST('2001-06-15' AS char(10)) < d`,
		`SELECT count(*) FROM dd WHERE d = CAST('12' AS TEXT) OR n = CAST('7' AS TEXT)`,
		`SELECT count(*) FROM dd WHERE d < CAST(CASE WHEN sk % 2 = 0 THEN '2000-01-01' ELSE '2003-01-01' END AS TEXT)`,
		`SELECT sk, d, d < CAST('2002' AS TEXT), CAST('x' AS TEXT) || d FROM dd WHERE sk % 1999 = 0 OR sk > 20000 ORDER BY sk`,
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
