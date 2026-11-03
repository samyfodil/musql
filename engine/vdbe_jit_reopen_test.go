package engine

import (
	"fmt"
	"testing"
)

// TestVMJITReopenDerived: a CTE closed and opened again for every candidate of
// a recursive step, which native code does itself (VOpen, VClose), answers as
// the VDBE alone answers it: the sudoku, EXISTS and NOT EXISTS over a source
// the step also reads, over another, an empty one and a VALUES list, NULLs
// and TEXT, and inner loops that exit to the VDBE mid-walk.
func TestVMJITReopenDerived(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("no VM JIT on this platform")
	}
	countSyncs(t)
	vmJITReopens.Store(0)
	setup := []string{
		`CREATE TABLE t(a INTEGER, s TEXT)`,
		`CREATE TABLE u(b INTEGER, w TEXT)`,
		`CREATE TABLE e(c INTEGER)`,
	}
	for i := range 40 {
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, '%c%c')", i%13, 'a'+i%5, 'a'+i%3))
	}
	for i := range 9 {
		b := fmt.Sprint(i * 2)
		if i == 4 {
			b = "NULL"
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO u VALUES(%s, '%c')", b, 'a'+i%4))
	}
	sudoku := func(digits string) string {
		return `WITH RECURSIVE
  input(sud) AS (VALUES('53..7....6..195....98....6.8...6...34..8.3..17...2...6.6....28....419..5....8..79')),
  digits(z, lp) AS (` + digits + `),
  x(s, ind) AS (
    SELECT sud, instr(sud, '.') FROM input
    UNION ALL
    SELECT substr(s, 1, ind-1) || z || substr(s, ind+1), instr(substr(s, 1, ind-1) || z || substr(s, ind+1), '.')
     FROM x, digits AS z
    WHERE ind>0 AND NOT EXISTS (SELECT 1 FROM digits AS lp
             WHERE z.z = substr(s, ((ind-1)/9)*9 + lp, 1)
                OR z.z = substr(s, ((ind-1)%9) + (lp-1)*9 + 1, 1)
                OR z.z = substr(s, (((ind-1)/3) % 3) * 3 + ((ind-1)/27) * 27 + lp + ((lp-1) / 3) * 6, 1)))
SELECT s FROM x WHERE ind=0`
	}
	// rec is a recursive step over src that runs body as its NOT EXISTS (or
	// EXISTS) for every candidate, the shape the sudoku has.
	rec := func(with, step string) string {
		return "WITH RECURSIVE " + with + ", r(i, k, s) AS (VALUES(0, 0, '') UNION ALL " + step + ") SELECT count(*), sum(k), max(s), min(k) FROM r"
	}
	queries := []string{
		sudoku(`VALUES('1', 1) UNION ALL SELECT CAST(lp+1 AS TEXT), lp+1 FROM digits WHERE lp<9`),
		sudoku(`VALUES ('1',1),('2',2),('3',3),('4',4),('5',5),('6',6),('7',7),('8',8),('9',9)`),
		rec("d(v, w) AS MATERIALIZED (SELECT b, w FROM u)",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 4 AND NOT EXISTS (SELECT 1 FROM d AS d2 WHERE d2.v = (r.k + d1.v) % 11)"),
		rec("d(v, w) AS MATERIALIZED (SELECT b, w FROM u)",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 4 AND EXISTS (SELECT 1 FROM d AS d2 WHERE d2.v = (r.k + d1.v) % 11 AND d2.w = d1.w)"),
		rec("d(v, w) AS MATERIALIZED (SELECT b, w FROM u)",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 4 AND NOT EXISTS (SELECT 1 FROM d AS d2 WHERE d2.v = r.k OR substr(d2.w || r.s, 2, 2) = 'ab')"),
		rec("d(v, w) AS MATERIALIZED (SELECT b, w FROM u)",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 3 AND NOT EXISTS (SELECT 1 FROM d AS d2 WHERE d2.v IS NULL AND d1.v > 10)"),
		rec("d(v, w) AS MATERIALIZED (SELECT b, w FROM u), z(c) AS MATERIALIZED (SELECT c FROM e)",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 3 AND NOT EXISTS (SELECT 1 FROM z WHERE z.c = d1.v)"),
		rec("d(v, w) AS MATERIALIZED (SELECT b, w FROM u WHERE b > 6)",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 4 AND NOT EXISTS (SELECT 1 FROM d AS d2 WHERE d2.v = d1.v + r.k % 3 * 2)"),
		rec("d(v, w) AS MATERIALIZED (SELECT value, char(64 + value) FROM generate_series(1, 6))",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 4 AND NOT EXISTS (SELECT 1 FROM d AS d2 WHERE d2.w = substr(r.s || 'ABC', d1.v, 1))"),
		rec("d(v, w) AS (SELECT b, w FROM u), n(j) AS (VALUES(1),(3),(5))",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 4 AND NOT EXISTS (SELECT 1 FROM n WHERE n.j = (r.k + d1.v) % 7) AND EXISTS (SELECT 1 FROM d AS d2 WHERE d2.w = d1.w)"),
		rec("d(v, w) AS MATERIALIZED (SELECT b, w FROM u)",
			"SELECT i+1, k + d1.v, s || d1.w FROM r, d AS d1 WHERE i < 4 AND NOT EXISTS (SELECT 1 FROM (SELECT b * 2 AS v FROM u ORDER BY w LIMIT 7) AS d2 WHERE d2.v = (r.k + d1.v) % 13)"),
	}
	vmJITOnOff(t, setup, queries)
	if n := vmJITReopens.Load(); n == 0 {
		t.Fatalf("no entry reopened a derived cursor natively: the test checks little")
	} else {
		t.Logf("%d entries reopened a derived cursor natively", n)
	}
}
