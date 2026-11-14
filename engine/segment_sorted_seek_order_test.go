package engine

import (
	"fmt"
	"testing"
)

// TestSegSortedSeekTakesAutoIndexOrder: a join level whose automatic index
// would have built the table and sorted it is served from the segments, each
// run put in the index's order there -- the seek column holding NULLs, the key
// running on over a NOCASE TEXT column and a REAL one -- and an order-reading
// aggregate answers as with the rows built. The segment peepholes are off, or
// the segment join would answer the aggregates before any seek.
func TestSegSortedSeekTakesAutoIndexOrder(t *testing.T) {
	stmts := []string{
		`CREATE TABLE d (id INTEGER PRIMARY KEY, k INT, tag TEXT)`,
		`CREATE TABLE f (a INT, t TEXT COLLATE NOCASE, v REAL, w INT)`,
	}
	for i := range 30 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO d VALUES (%d, %d, 'g%d')`, i, i%12, i%3))
	}
	words := []string{"b", "A", "a", "C", "B", "c"}
	for i := range 1500 {
		a := fmt.Sprint((i * 7) % 13)
		if i%9 == 0 {
			a = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO f VALUES (%s, '%s', %d.5, %d)`, a, words[(i*5)%len(words)], (i*31)%17, i%4))
	}
	p := newSegPair(t, stmts...)
	qs := []string{
		`SELECT d.id, group_concat(f.t || f.v, ',') FROM d, f WHERE f.a = d.k GROUP BY d.id`,
		`SELECT d.tag, group_concat(f.v || '/' || f.w, ',') FROM d JOIN f ON f.a = d.k WHERE d.id > 3 GROUP BY d.tag`,
		`SELECT d.id, f.t, f.v FROM d LEFT JOIN f ON f.a = d.k WHERE d.id < 6`,
	}
	for _, q := range qs {
		segSortedSeeksServed.Store(0)
		got := p.mustPlain(q)
		if segSortedSeeksServed.Load() == 0 {
			t.Errorf("%s: no seek served by the segments", q)
		}
		restore := SetSegLazyRowsForTest(false)
		want := p.mustPlain(q)
		restore()
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n segments %.400s\n built    %.400s", q, g, w)
		}
		if g, w := fmt.Sprint(typedRows(p.mustFast(q))), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s (peepholes on):\n %.400s\n %.400s", q, g, w)
		}
	}
}

// TestSegSortedSeekOrdersRunOnIntegers: a run put in the automatic index's
// order is compared on the key's leading INTEGER columns read from their
// blocks, NULLs first, and only rows equal on those go on to the REAL and
// NOCASE TEXT columns after them.
func TestSegSortedSeekOrdersRunOnIntegers(t *testing.T) {
	stmts := []string{
		`CREATE TABLE d (id INTEGER PRIMARY KEY, k INT)`,
		`CREATE TABLE f (a INT, b INT, x REAL, t TEXT COLLATE NOCASE, w INT)`,
	}
	for i := range 20 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO d VALUES (%d, %d)`, i, i%7))
	}
	words := []string{"b", "A", "a", "C", "B", "c"}
	for i := range 2000 {
		b := fmt.Sprint((i * 11) % 5)
		if i%6 == 0 {
			b = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO f VALUES (%d, %s, %d, '%s', %d)`, (i*3)%7, b, (i*13)%3, words[(i*5)%len(words)], i))
	}
	p := newSegPair(t, stmts...)
	qs := []string{
		`SELECT d.id, group_concat(coalesce(f.b, 'n') || f.x || f.t || f.w, ',') FROM d, f WHERE f.a = d.k GROUP BY d.id`,
		`SELECT d.id, f.b, f.x, f.t, f.w FROM d JOIN f ON f.a = d.k WHERE d.id < 4`,
	}
	for _, q := range qs {
		segSortedSeeksServed.Store(0)
		got := p.mustPlain(q)
		if segSortedSeeksServed.Load() == 0 {
			t.Errorf("%s: no seek served by the segments", q)
		}
		restore := SetSegLazyRowsForTest(false)
		want := p.mustPlain(q)
		restore()
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n segments %.400s\n built    %.400s", q, g, w)
		}
	}
}
