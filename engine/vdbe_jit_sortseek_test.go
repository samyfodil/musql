package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestVMJITSortSeek: an automatic index's seek of a CTE's rows (OpSortedSeek
// over a row list) is made natively once the VDBE has sorted them
// (jit.VSortSeek), and so is the OpIfEmpty a miss reaches (jit.VSortEmpty).
// Each statement answers as the VDBE does: runs of one key and of several,
// misses, keys that are not all INTEGERs, probes that are not INTEGERs, an
// empty CTE, a LEFT JOIN's NULL row.
func TestVMJITSortSeek(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	countSyncs(t)
	path := filepath.Join(t.TempDir(), "ss.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if err := db.Exec(`CREATE TABLE u(a INT, s TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO u SELECT value, value % 9 FROM generate_series(1, 300)`); err != nil {
		t.Fatal(err)
	}
	fifth := `select i, i*i*i*i*i i5 from (select value i from generate_series(1, 40))`
	join := `select a.i, b.i, c.i, d.i from t a, t b, t c, t d where a.i < b.i and b.i < c.i and a.i5 + b.i5 + c.i5 + %s = d.i5`
	queries := []string{
		fmt.Sprintf(`with t as materialized (%s) `+join, fifth, "0"),
		fmt.Sprintf(`with t as not materialized (%s) `+join, fifth, "0"),
		fmt.Sprintf(`with t as materialized (%s) `+join, fifth, "1"),
		`with c as materialized (select value % 7 k, value v from generate_series(1, 200))
		 select a.v, count(*), sum(b.v), min(b.v), max(b.v) from c a, c b where b.k = a.v % 9 group by a.v`,
		`with c as materialized (select value % 7 k, value v from generate_series(1, 200))
		 select a.v, b.v from c a, c b where b.k = a.v - 150`,
		`with c as materialized (select value % 7 k, value v from generate_series(1, 40))
		 select a.v, b.v from c a, c b where b.k = a.v % 9`,
		`with c as materialized (select case when value % 5 = 0 then null else value % 7 end k, value v from generate_series(1, 100))
		 select a.v, count(*), sum(b.v), min(b.v), max(b.v) from c a, c b where b.k = a.v % 9 group by a.v`,
		`with c as materialized (select case when value % 5 = 0 then '3' else value % 7 end k, value v from generate_series(1, 100))
		 select a.v, count(*), sum(b.v), min(b.v), max(b.v) from c a, c b where b.k = a.v % 9 group by a.v`,
		`with c as materialized (select value % 7 k, value v, (value % 4) || '' s from generate_series(1, 100))
		 select a.v, count(*), sum(b.v), min(b.v), max(b.v) from c a, c b where b.k = a.s group by a.v`,
		`with c as materialized (select value % 7 k, value v from generate_series(1, 100))
		 select a.v, count(*), sum(b.v), min(b.v), max(b.v) from c a, c b where b.k = a.v * 0.5 group by a.v`,
		`with c as materialized (select value * -3000000000 k, value v from generate_series(1, 100))
		 select a.v, b.v from c a, c b where b.k = a.v * -6000000000`,
		`with c as materialized (select value k from generate_series(1, 0)), d as materialized (select value k from generate_series(1, 50))
		 select x.k, y.k, c.k from d x, d y, c where c.k = x.k + y.k`,
		`with c as materialized (select value % 7 k, value v from generate_series(1, 60))
		 select a.v, b.v from c a left join c b on b.k = a.v % 11 where a.v < 30`,
		`with c as materialized (select a % 7 k, a v from u)
		 select u.a, count(*), sum(c.v), min(c.v) from u, c where c.k = u.s group by u.a`,
	}
	run := func(on bool) []string {
		old := vmJITEnabled
		vmJITEnabled = on
		defer func() { vmJITEnabled = old }()
		var out []string
		for _, q := range queries {
			_, rows, err := db.Query(fmt.Sprintf("%s -- %v", q, on), nil)
			out = append(out, fmt.Sprint(rows, err))
		}
		return out
	}
	// About 10,000 seeks, nearly all misses: a handful of entries, not one a
	// seek. Counted first: on js/wasm a kernel reads the write-barrier flag
	// at the address it was compiled with, which jitPointerModes forces on.
	// And with no GC cycle running, whose write barrier sends every seek back
	// to the VDBE while it lasts.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	CountVMJITCursorEntersForTest(true)
	VMJITCursorEntersForTest()
	if _, _, err := db.Query(fmt.Sprintf(`with t as materialized (%s) `+join+` -- enters`, fifth, "0"), nil); err != nil {
		t.Fatal(err)
	}
	if n := VMJITCursorEntersForTest(); n > 200 {
		t.Errorf("%d native entries", n)
	}
	CountVMJITCursorEntersForTest(false)
	want := run(false)
	jitPointerModes(t, func(t *testing.T, mode string) {
		got := run(true)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: %s\nvdbe %.300s\njit  %.300s", mode, queries[i], want[i], got[i])
			}
		}
	})
}
