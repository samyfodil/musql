package engine

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestVMJITResultRowStopKeepsCursors: while the collector marks, native code
// stops at each OP_ResultRow, whose row holds pointers, and the VDBE stores
// it. A run that gathers its rows goes on with the cursors native code kept,
// rather than taking each back and handing it over again a row at a time.
// The rows have to be the same with the JIT off, with the barrier held off,
// and with it forced on.
func TestVMJITResultRowStopKeepsCursors(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	countSyncs(t)
	p := newSegPair(t,
		`CREATE TABLE t(o INT, w INT, s TEXT, b BLOB)`,
		`INSERT INTO t SELECT value % 300, value % 7, 's' || (value % 13), CASE WHEN value % 5 = 0 THEN NULL ELSE x'0102' END FROM generate_series(1, 3000)`,
		`CREATE TABLE r(o INT, n INT)`,
		`INSERT INTO r SELECT value * 3 % 300, value FROM generate_series(1, 200)`,
		`CREATE TABLE k(o INT, n INT, s TEXT)`,
		`INSERT INTO k SELECT value % 150, value, 'k' || value FROM generate_series(1, 600)`,
		`CREATE INDEX ko ON k(o)`)
	queries := []string{
		`select count(*), sum(a.w) from t a where a.o in (select x.o from t x, t y where x.o = y.o and x.w <> y.w and x.o % 3 = 0)`,
		`select count(*), sum(a.w) from t a where a.o in (select r.o from r, t x, t y where r.o = x.o and x.o = y.o and x.w <> y.w)`,
		`select x.o, y.w, x.s, y.b from t x, t y where x.o = y.o and x.w <> y.w and x.o < 20 order by 1, 2, 3`,
		`select count(*), max(s), min(o) from (select x.o o, y.s s from t x, t y where x.o = y.o and x.w < y.w) where o > 5`,
		`select (select y.s from t x, t y where x.o = y.o and x.w <> y.w and x.o = 7 order by y.s limit 1)`,
		`select x.o, y.s from t x, t y where x.o = y.o and x.w <> y.w limit 25`,
		`select count(*), sum(a.n) from k a where a.o in (select k.o from t x, k where k.o = x.o and x.w = 1)`,
		`select x.o, k.n, k.s from t x, k where k.o = x.o and x.w = 2 order by 1, 2`,
		`select o, n from (select x.o o, k.n n from t x, k where k.o = x.o and x.w = 3) join r using (o) order by 1, 2`,
		`select (select k.s from t x, k where k.o = x.o and x.w = 4 and x.s = 's5')`,
		`select x.o, (select group_concat(k.n) from k where k.o = x.o) from t x where x.w = 5 order by 1, 2 limit 40`,
	}
	// The first two gather an IN subquery's rows, one a row per y.
	const gathering = 2
	var resumes int64
	run := func(mode string) []string {
		var out []string
		for i, q := range queries {
			vmJITResumes.Store(0)
			_, rows, err := p.fast(fmt.Sprintf("%s -- %s", q, mode))
			out = append(out, fmt.Sprint(typedRows(rows), err))
			if i < gathering {
				resumes += vmJITResumes.Load()
			}
		}
		return out
	}
	// Forced first: a statement's kernels are emitted once and kept, and the
	// flag a kernel reads is the one in force when it was emitted.
	restore := jit.ForceWriteBarrierOnForTest()
	forced := run("forced")
	restore()
	forcedResumes := resumes
	gc := debug.SetGCPercent(-1)
	runtime.GC()
	held := run("held")
	debug.SetGCPercent(gc)
	old := vmJITEnabled
	vmJITEnabled = false
	off := run("off")
	vmJITEnabled = old
	for i, q := range queries {
		if held[i] != off[i] {
			t.Errorf("%s:\n JIT off       %.300s\n barrier off   %.300s", q, off[i], held[i])
		}
		if forced[i] != off[i] {
			t.Errorf("%s:\n JIT off       %.300s\n barrier on    %.300s", q, off[i], forced[i])
		}
	}
	if forcedResumes < 1000 {
		t.Errorf("%d entries of the gathering runs went on with the cursors native code kept", forcedResumes)
	}
}
