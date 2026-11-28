package engine

import (
	"fmt"
	"path/filepath"
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
		// Bit operators, unary minus and NOT, and a join with a CTE, which
		// tests the CTE for rows first (OpIfEmpty) -- issue #203's shape.
		`WITH RECURSIVE n(m) AS (VALUES(1),(2),(4),(8),(16)), b(k, m, r) AS (SELECT 1, m, ~m FROM n UNION ALL SELECT k + 1, b.m | n.m, (b.r & ~n.m) FROM b, n WHERE n.m > b.m AND (b.m & n.m) = 0 AND (n.m << 1) >> 1 = n.m) SELECT k, count(*), sum(m), sum(r), sum(-m) FROM b GROUP BY k`,
		`WITH RECURSIVE e(m) AS (SELECT 1 WHERE 0), b(k) AS (SELECT 1 UNION ALL SELECT k + 1 FROM b, e WHERE k < 5) SELECT count(*) FROM b`,
		`WITH RECURSIVE n(m) AS (VALUES(1),(2)), e(m) AS (SELECT x FROM t1 WHERE x > 100), b(k) AS (SELECT 1 UNION ALL SELECT k + 1 FROM b CROSS JOIN n CROSS JOIN e WHERE k < 5 UNION ALL SELECT k + 2 FROM b CROSS JOIN n WHERE k < 9) SELECT group_concat(k) FROM b`,
		`WITH RECURSIVE b(k, f) AS (SELECT 1, 0 UNION ALL SELECT k + 1, NOT f FROM b WHERE k < 9) SELECT group_concat(f) FROM b`,
		// A function computed once per statement keeps a flag register that is
		// read before it is written: its load is not a constant to hoist.
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 3 AND date('2000-01-01') = '2000-01-01') SELECT group_concat(i) FROM c`,
		`WITH RECURSIVE c(i, d) AS (SELECT 1, '' UNION ALL SELECT i + 1, date('2000-01-01', '+' || i || ' days') || upper('x') FROM c WHERE i < 4) SELECT group_concat(d) FROM c`,
		// The self row read straight from the queue: no subtype, no affinity.
		`WITH RECURSIVE c(i, j) AS (SELECT 1, json('[1]') UNION ALL SELECT i + 1, json_array(j, json(j)) FROM c WHERE i < 4) SELECT group_concat(j, ' | ') FROM c`,
		`WITH RECURSIVE c(i, s) AS (SELECT 1, '007' UNION ALL SELECT i + 1, s || '' FROM c WHERE i < 3) SELECT group_concat(typeof(s) || s) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c, t1 WHERE i < 6) SELECT count(*), sum(i) FROM c`,
		`WITH RECURSIVE c(i, s, r) AS (SELECT 1, '1', 1.0 UNION ALL SELECT i + 1, CAST(i + 1 AS TEXT), CAST(i AS REAL) / 2 FROM c WHERE i < 6) SELECT group_concat(typeof(s) || s || typeof(r) || r) FROM c`,
		// A step that seeks a CTE by the current row (OpSortedSeek), on one
		// column and on two -- issue #298's shape.
		`WITH RECURSIVE t(i) AS (SELECT value FROM generate_series(1, 12)),
		  s(s) AS (SELECT i*i FROM t WHERE i*i < 24),
		  a(a, b) AS (SELECT t.i, t1.i FROM t, t t1 WHERE t.i <> t1.i AND t.i + t1.i IN (SELECT s FROM s)),
		  c(lv, r, bmp, path) AS (SELECT 1, a.b, 1 + (1 << (a.b - 1)), '01' || printf('%02d', a.b) FROM a WHERE a.a = 1
		    UNION ALL SELECT lv + 1, a.b, bmp + (1 << (a.b - 1)), path || printf('%02d', a.b) FROM c, a
		    WHERE a.a = c.r AND bmp & (1 << (a.b - 1)) = 0 AND lv < 12)
		SELECT lv, count(*), max(path), min(path) FROM c GROUP BY lv`,
		`WITH RECURSIVE e(x, y, z) AS (SELECT value % 5, value % 3, value FROM generate_series(1, 40)),
		  c(k, x, y, z) AS (SELECT 1, 1, 1, 0 UNION ALL SELECT k + 1, e.y + 1, (e.z + k) % 3, e.z FROM c, e WHERE e.x = c.x AND e.y = c.y AND k < 6)
		SELECT k, count(*), sum(z) FROM c GROUP BY k`,
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

// TestRecInlineReadsTheSelfRowDirectly: an inlined step that reads its self
// reference only by column opens no one-row cursor per popped row; the
// sub-program it replaces opened one per row.
func TestRecInlineReadsTheSelfRowDirectly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	buildDB(t, path, `CREATE TABLE t1(x)`, `INSERT INTO t1 VALUES(1),(2)`)
	opens := func(q string, inline bool) int64 {
		recInlineOffForTest = !inline
		defer func() { recInlineOffForTest = false }()
		n, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		before := recSelfOpens.Load()
		if _, _, err := n.Query(q, nil); err != nil {
			t.Fatal(err)
		}
		return recSelfOpens.Load() - before
	}
	for _, q := range []string{
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 500) SELECT count(*) FROM c`,
		`WITH RECURSIVE n(m) AS (VALUES(1)), c(i) AS (SELECT 1 UNION ALL SELECT i + m FROM c, n WHERE i < 50) SELECT count(*) FROM c`,
	} {
		if got := opens(q, false); got == 0 {
			t.Errorf("%s: the sub-program opened no self cursor: the counter is not reached", q)
		}
		if got := opens(q, true); got != 0 {
			t.Errorf("%s: the inlined step opened its self cursor %d times", q, got)
		}
	}
}

// TestRecInlineWalksATable: a step that joins a table, by a scan, an index
// seek or a rowid seek -- a graph or tree walk -- is inlined like any other,
// with the same answers as its sub-program.
func TestRecInlineWalksATable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.musq")
	buildDB(t, path,
		`CREATE TABLE edge(a INTEGER, b INTEGER, w REAL)`,
		`CREATE INDEX edge_a ON edge(a)`,
		`WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM g WHERE i < 300) INSERT INTO edge SELECT i, i * 2, i / 7.0 FROM g`,
		`INSERT INTO edge SELECT a, a * 2 + 1, w FROM edge WHERE a % 3 = 0`,
		`CREATE TABLE plain(a INTEGER, b INTEGER)`,
		`INSERT INTO plain SELECT a, b FROM edge`,
		`CREATE TABLE emp(id INTEGER PRIMARY KEY, boss INTEGER, name TEXT)`,
		`WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM g WHERE i < 400) INSERT INTO emp SELECT i, CASE WHEN i = 1 THEN NULL ELSE i / 2 END, 'e' || i FROM g`)
	queries := []string{
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r WHERE n < (SELECT count(*) FROM plain WHERE a < 50)) SELECT count(*), sum(n) FROM r`,
		// Down an index on edge(a).
		`WITH RECURSIVE r(n, d) AS (SELECT 1, 0 UNION SELECT edge.b, d + 1 FROM r JOIN edge ON edge.a = r.n WHERE d < 12) SELECT count(*), sum(n), max(d) FROM r`,
		`WITH RECURSIVE r(n, p) AS (SELECT 3, '3' UNION ALL SELECT edge.b, p || '>' || edge.b FROM r, edge WHERE edge.a = r.n AND edge.w < 30) SELECT count(*), max(p), min(p) FROM r`,
		// The same walk over a table with no index: a scan per step.
		`WITH RECURSIVE r(n, d) AS (SELECT 1, 0 UNION SELECT plain.b, d + 1 FROM r JOIN plain ON plain.a = r.n WHERE d < 8) SELECT count(*), sum(n), max(d) FROM r`,
		// Up a tree by rowid.
		`WITH RECURSIVE up(id, boss, depth) AS (SELECT id, boss, 0 FROM emp WHERE id = 397 UNION ALL SELECT emp.id, emp.boss, depth + 1 FROM up JOIN emp ON emp.id = up.boss) SELECT group_concat(id), max(depth) FROM up`,
		// Down the same tree, through its boss column with no index.
		`WITH RECURSIVE dn(id, depth) AS (SELECT 1, 0 UNION ALL SELECT emp.id, depth + 1 FROM dn JOIN emp ON emp.boss = dn.id WHERE depth < 4) SELECT count(*), sum(id), max(depth) FROM dn`,
		// The rowid read back, and a table column carried along.
		`WITH RECURSIVE up(id, names) AS (SELECT 250, '' UNION ALL SELECT emp.boss, names || emp.name || ',' FROM up, emp WHERE emp.rowid = up.id AND emp.boss IS NOT NULL) SELECT names FROM up ORDER BY length(names) DESC LIMIT 1`,
	}
	// steps is how many recursive steps of q's program still run as
	// sub-programs ("RecQueueFill 1"), which EXPLAIN shows after inlining.
	run := func(q string, inline bool) (rows string, steps int) {
		recInlineOffForTest = !inline
		defer func() { recInlineOffForTest = false }()
		n, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		_, res, err := n.Query(q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		_, ops, err := n.Query("EXPLAIN "+q, nil)
		if err != nil {
			t.Fatalf("EXPLAIN %s: %v", q, err)
		}
		for _, r := range ops {
			if string(r[1].S) == "RecQueueFill" && r[2].I == 1 {
				steps++
			}
		}
		return fmt.Sprint(typedRows(res)), steps
	}
	for _, q := range queries {
		want, subSteps := run(q, false)
		got, steps := run(q, true)
		if got != want {
			t.Errorf("%s:\n inlined     %s\n sub-program %s", q, got, want)
		}
		if subSteps == 0 {
			t.Errorf("%s: EXPLAIN shows no step run as a sub-program even with inlining off", q)
		}
		if steps != 0 {
			t.Errorf("%s: %d steps not inlined", q, steps)
		}
	}
}
