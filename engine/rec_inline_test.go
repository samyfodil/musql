package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestRecInlineMatchesTheSubProgram: a recursive step inlined into the queue
// program (recInlinePeephole) answers exactly what the step's own sub-program
// did, for every queue discipline, and a runaway recursion still stops at the
// same cap.
func TestRecInlineMatchesTheSubProgram(t *testing.T) {
	p := newSegPair(t, `CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, payload TEXT)`,
		`CREATE TABLE link(aa INT, bb INT)`,
		`INSERT INTO link VALUES(1,3),(5,3),(5,6),(6,7),(3,10)`,
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(1),(2)`,
		`CREATE VIEW v AS SELECT * FROM t1`)
	queries := []string{
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 500) SELECT count(*), sum(i), max(i) FROM c`,
		`WITH RECURSIVE c(i, s) AS (SELECT 1, 'a' UNION ALL SELECT i + 1, s || i FROM c WHERE i < 40) SELECT i, s FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c LIMIT 25) SELECT group_concat(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c LIMIT 10 OFFSET 5) SELECT group_concat(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION SELECT (i * 7) % 31 FROM c) SELECT count(*), sum(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i * 2 FROM c WHERE i < 1000 UNION ALL SELECT i * 3 FROM c WHERE i < 1000) SELECT count(*), sum(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 5 UNION ALL SELECT i - 1 FROM c WHERE i > 0 ORDER BY 1 DESC) SELECT group_concat(i) FROM c`,
		`WITH RECURSIVE c(i, f) AS (SELECT 1, 1.5 UNION ALL SELECT i + 1, f * 2 / 3 FROM c WHERE i < 30) SELECT sum(f), max(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < ?) SELECT count(*) FROM c`,
		`WITH RECURSIVE c(i, n) AS (SELECT 1, NULL UNION ALL SELECT i + 1, CASE WHEN i % 3 = 0 THEN NULL ELSE i END FROM c WHERE i < 50) SELECT count(n), sum(n) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c) SELECT count(*) FROM c`, // runaway: the cap
		// A step that joins: its self cursor and a table scan on one pooled
		// machine (with5.test, with2.test).
		`WITH RECURSIVE closure(x) AS (VALUES(1) UNION SELECT aa FROM link, closure WHERE link.bb=closure.x UNION SELECT bb FROM closure, link WHERE link.aa=closure.x) SELECT group_concat(x) FROM (SELECT x FROM closure ORDER BY x)`,
		`WITH q(a) AS (SELECT 1 UNION ALL SELECT a+1 FROM q, v WHERE a<5) SELECT count(*), sum(a) FROM q`,
		// A step that joins a CTE and tests a correlated NOT EXISTS over a
		// recursive one, inlined as a subroutine (issue #74's sudoku).
		`WITH RECURSIVE input(sud) AS (VALUES('53..7....6..195....98....6.8...6...34..8.3..17...2...6.6....28....419..5....8..79')),
		  digits(z, lp) AS (VALUES('1', 1) UNION ALL SELECT CAST(lp+1 AS TEXT), lp+1 FROM digits WHERE lp<9),
		  x(s, ind) AS (SELECT sud, instr(sud, '.') FROM input UNION ALL
		    SELECT substr(s, 1, ind-1) || z || substr(s, ind+1), instr(substr(s, 1, ind-1) || z || substr(s, ind+1), '.')
		    FROM x, digits AS z WHERE ind>0 AND NOT EXISTS (SELECT 1 FROM digits AS lp
		      WHERE z.z = substr(s, ((ind-1)/9)*9 + lp, 1) OR z.z = substr(s, ((ind-1)%9) + (lp-1)*9 + 1, 1)
		         OR z.z = substr(s, (((ind-1)/3) % 3) * 3 + ((ind-1)/27) * 27 + lp + ((lp-1) / 3) * 6, 1)))
		SELECT s FROM x WHERE ind=0`,
		`WITH RECURSIVE d(z, lp) AS (VALUES('1', 1) UNION ALL SELECT CAST(lp+1 AS TEXT), lp+1 FROM d WHERE lp<5),
		  e(w) AS (VALUES(2),(4)),
		  x(s, k) AS (SELECT '', 1 UNION ALL SELECT s || z, k + 1 FROM x, d WHERE k < 4
		    AND NOT EXISTS (SELECT 1 FROM d AS f, e WHERE f.z = substr(s, k - 1, 1) OR f.lp * e.w = k + d.lp))
		SELECT count(*), max(s), min(s) FROM x`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 20 AND EXISTS (SELECT 1 FROM link WHERE aa + bb > i)) SELECT group_concat(i) FROM c`,
	}
	ask := func(q string, inline bool) string {
		recInlineOffForTest, segPeepholesOffForTest = !inline, !inline
		defer func() { recInlineOffForTest, segPeepholesOffForTest = false, false }()
		n, err := OpenWrite(p.path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		var args []Value
		if strings.Contains(q, "?") {
			args = []Value{{Typ: Int, I: 77}}
		}
		_, rows, err := n.Query(q, args)
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprint(typedRows(rows))
	}
	for _, q := range queries {
		want, got := ask(q, false), ask(q, true)
		if strings.HasPrefix(want, "error: ") && !strings.Contains(q, "runaway") && !strings.Contains(q, "FROM c) SELECT count(*)") {
			t.Errorf("%s: %s", q, want)
		}
		if got != want {
			t.Errorf("%s:\n inlined     %s\n sub-program %s", q, got, want)
		}
	}
	// The recursive INSERT the browser race loads its table with.
	load := func(inline bool) string {
		recInlineOffForTest = !inline
		defer func() { recInlineOffForTest = false }()
		n, err := OpenWrite(p.path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		if err := n.Exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 2000) INSERT INTO t SELECT i, (i * 7919) % 1000, 'row-' || i || '-payload' FROM c`); err != nil {
			t.Fatal(err)
		}
		_, rows, err := n.Query(`SELECT count(*), sum(id), sum(sec), max(payload), min(payload) FROM t`, nil)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	if want, got := load(false), load(true); got != want {
		t.Errorf("recursive INSERT:\n inlined     %s\n sub-program %s", got, want)
	}
}
