package engine

import (
	"fmt"
	"testing"
)

// TestSegSortedSeekMatchesScan: a join level's sorted seek served from the
// segments answers as the same join with the seek defeated by a CAST, which
// keeps the column's affinity -- for INTEGER, REAL and TEXT probes and NULL
// keys. Counts grouped by the outer table reach it (no automatic index sorts
// the level first). Over a REAL column, with rows in the log, or with the
// session's own writes, the built rows serve it instead, with the same answer.
// Row queries, ordered fully, answer as the scan does too.
func TestSegSortedSeekMatchesScan(t *testing.T) {
	stmts := []string{
		`CREATE TABLE c (id INTEGER PRIMARY KEY, kr REAL, kt TEXT, kn)`,
		`CREATE TABLE o (id INTEGER PRIMARY KEY, cid INTEGER, rid REAL, v TEXT)`,
	}
	probes := []string{"%d", "%d.0", "%d.5", "'%d'", "NULL", "'x%d'"}
	for i := 0; i < 120; i++ {
		k := probes[i%len(probes)]
		if k != "NULL" {
			k = fmt.Sprintf(k, i%40)
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO c VALUES (%d, %d.0, '%d', %s)`, i, i%40, i%40, k))
	}
	for i := 0; i < 900; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO o VALUES (%d, %d, %d, 'v%d')`, 3000-i, (i*13)%45, (i*13)%45, i))
	}
	p := newSegPair(t, stmts...)
	type pair struct{ seek, scan string }
	q := func(on, on0 string) pair {
		const sel = `SELECT c.id, count(o.id), count(*) FROM c LEFT JOIN o ON `
		return pair{sel + on + ` GROUP BY c.id`, sel + on0 + ` GROUP BY c.id`}
	}
	served := []pair{
		q(`o.cid = c.kr`, `CAST(o.cid AS INTEGER) = c.kr`),
		q(`o.cid = c.kt`, `CAST(o.cid AS INTEGER) = c.kt`),
		q(`o.cid = c.kn`, `CAST(o.cid AS INTEGER) = c.kn`),
	}
	built := []pair{q(`o.rid = c.id`, `CAST(o.rid AS REAL) = c.id`)}
	const rowSel = `SELECT c.id, o.id, o.v FROM c JOIN o ON `
	// On the rowid, the segment join (SegJoinGroups) answers before any seek.
	rowsQ := []pair{
		q(`o.cid = c.id`, `CAST(o.cid AS INTEGER) = c.id`),
		{rowSel + `o.cid = c.kt ORDER BY c.id, o.id`, rowSel + `CAST(o.cid AS INTEGER) = c.kt ORDER BY c.id, o.id`},
		{`SELECT c.id, o.id FROM c LEFT JOIN o ON o.cid = c.kr ORDER BY 1, 2`, `SELECT c.id, o.id FROM c LEFT JOIN o ON CAST(o.cid AS INTEGER) = c.kr ORDER BY 1, 2`},
	}
	// served, when given, is whether the segments must have served the seeks.
	check := func(stage string, ps []pair, served ...bool) {
		t.Helper()
		for _, pq := range ps {
			segSortedSeeksServed.Store(0)
			got := p.mustFast(pq.seek)
			if n := segSortedSeeksServed.Load(); len(served) > 0 && (n > 0) != served[0] {
				t.Errorf("%s %s: %d seeks served by the segments, want served=%v", stage, pq.seek, n, served[0])
			}
			want := p.mustFast(pq.scan)
			if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
				t.Errorf("%s %s:\n seek %.300s\n scan %.300s", stage, pq.seek, g, w)
			}
		}
	}
	check("segments", served, true)
	check("segments", built, false)
	check("segments", rowsQ)
	p.delta(`INSERT INTO o VALUES (5, 7, 7, 'log')`, `DELETE FROM o WHERE id = 2999`)
	check("with log", served, false)
	check("with log", rowsQ)
	n, err := OpenWrite(p.path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if err := n.Exec(`INSERT INTO o VALUES (6, 9, 9, 'session')`); err != nil {
		t.Fatal(err)
	}
	for _, pq := range append(served, rowsQ...) {
		_, got, gerr := n.Query(pq.seek, nil)
		_, want, werr := n.Query(pq.scan, nil)
		if gerr != nil || werr != nil {
			t.Fatal(gerr, werr)
		}
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("session %s:\n seek %.300s\n scan %.300s", pq.seek, g, w)
		}
	}
}
