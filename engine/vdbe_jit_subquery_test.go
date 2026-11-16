package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestVMJITSubqueryValue: a scalar subquery a run makes once is read by native
// code once it is made (jitSubs, jit.VGuardCopy): a loop comparing with it
// stays native rather than exiting at the subquery on every row, and answers
// as the VDBE does for every kind of value, none, the first of many, and a
// subquery no row reaches.
func TestVMJITSubqueryValue(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	lowEntryThreshold(t)
	countSyncs(t)
	path := filepath.Join(t.TempDir(), "sq.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INT, r REAL, s TEXT)`,
		`CREATE TABLE u(k INT, s TEXT)`,
		`INSERT INTO t SELECT value, value % 50, value * 0.5, 's' || (value % 7) FROM generate_series(1, 3000)`,
		`INSERT INTO u SELECT value, 's' || value FROM generate_series(3, 9)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	queries := []string{
		`SELECT count(*) FROM t WHERE k = (SELECT max(k) FROM u)`,
		`SELECT count(*), sum(id) FROM t WHERE r > (SELECT avg(r) FROM t)`,
		`SELECT count(*) FROM t WHERE s = (SELECT min(s) FROM u)`,
		`SELECT count(*) FROM t WHERE k = (SELECT k FROM u WHERE k > 100)`,
		`SELECT count(*) FROM t WHERE k = (SELECT k FROM u ORDER BY k DESC)`,
		`SELECT count(*) FROM t WHERE k < 0 AND k = (SELECT max(k) FROM u)`,
		`SELECT id, (SELECT max(k) FROM u) + k FROM t WHERE k < 3 ORDER BY id`,
		`SELECT count(*) FROM t WHERE k = (SELECT max(k) FROM u) AND r < (SELECT min(r) FROM t) + 100`,
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
	want := run(false)
	jitPointerModes(t, func(t *testing.T, mode string) {
		got := run(true)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: %s\nvdbe %.300s\njit  %.300s", mode, queries[i], want[i], got[i])
			}
		}
	})
	// The made value is read natively: the loop over 3,000 rows enters native
	// code a handful of times, not once a row.
	CountVMJITCursorEntersForTest(true)
	defer CountVMJITCursorEntersForTest(false)
	VMJITCursorEntersForTest()
	if _, _, err := db.Query(`SELECT id FROM t WHERE k = (SELECT max(k) FROM u) -- enters`, nil); err != nil {
		t.Fatal(err)
	}
	if n := VMJITCursorEntersForTest(); n > 100 {
		t.Errorf("%d native entries for one run over 3,000 rows", n)
	}
}
