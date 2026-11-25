package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestVMJITRecursiveCTEStepsInTheVDBE: a recursive CTE's step runs once per
// queued row, and the outer query reads the CTE's rows from a co-routine, one
// advance each. Native code cannot loop in either, so entering it per row
// handed it a cursor and took it back for a few instructions' work, which
// made "WITH RECURSIVE t AS (SELECT 1 UNION ALL SELECT a+1 FROM t WHERE
// a<1000000) SELECT sum(a) FROM t" 1.6x slower than the VDBE alone. Every
// answer is the VDBE's: sum, count, a filter, a join and ORDER BY over the
// CTE, and a CTE whose step reads a table.
func TestVMJITRecursiveCTEStepsInTheVDBE(t *testing.T) {
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	path := filepath.Join(t.TempDir(), "r.musq")
	buildDB(t, path, `CREATE TABLE e(a INTEGER, b INTEGER)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 300) INSERT INTO e SELECT v, v + 1 FROM g`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const n = 20000
	queries := []string{
		fmt.Sprintf(`WITH RECURSIVE t AS (SELECT 1 a UNION ALL SELECT a+1 FROM t WHERE a<%d) SELECT sum(a) FROM t`, n),
		fmt.Sprintf(`WITH RECURSIVE t(a, s) AS (SELECT 1, 1 UNION ALL SELECT a+1, s+a*2 FROM t WHERE a<%d) SELECT count(*), max(s), sum(a %% 7) FROM t WHERE a %% 3 = 0`, n),
		`WITH RECURSIVE t(a) AS (SELECT 1 UNION ALL SELECT b FROM t JOIN e ON e.a = t.a) SELECT count(*), sum(a) FROM t`,
		`WITH RECURSIVE t(a) AS (SELECT 1 UNION ALL SELECT a+1 FROM t WHERE a<500) SELECT t.a, e.b FROM t, e WHERE e.a = t.a AND t.a % 97 = 0 ORDER BY 1 DESC`,
	}
	run := func(on bool) (out []string, enters int64) {
		old := vmJITEnabled
		vmJITEnabled = on
		defer func() { vmJITEnabled = old }()
		vmJITCountCursorEnters = true
		defer func() { vmJITCountCursorEnters = false }()
		vmJITCursorEnters.Store(0)
		for _, q := range queries {
			_, rows, err := p.QueryArgs(fmt.Sprintf("%s -- %v", q, on), nil)
			out = append(out, fmt.Sprint(rows, err))
		}
		return out, vmJITCursorEnters.Load()
	}
	want, _ := run(false)
	got, enters := run(true)
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s:\n vdbe %.300s\n jit  %.300s", queries[i], want[i], got[i])
		}
	}
	// On main: one per row of the first two CTEs, their steps' and the
	// outer queries' alike.
	if enters > n/10 {
		t.Errorf("%d entries into native code that hand it a cursor, for CTEs of %d rows", enters, n)
	}
}
