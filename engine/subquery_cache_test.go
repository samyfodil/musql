package engine

import (
	"fmt"
	"testing"
)

// TestCorrelatedSubqueryKeepsRows: a correlated scalar subquery runs once per
// distinct enclosing value it reads, and answers as when it runs for every
// row -- which a random() call in it, a function that is not constant, makes
// it do.
func TestCorrelatedSubqueryKeepsRows(t *testing.T) {
	stmts := []string{
		`CREATE TABLE a (id INTEGER PRIMARY KEY, k, v INT)`,
		`CREATE TABLE b (k, x REAL)`,
	}
	keys := []string{"1", "2", "'2'", "2.0", "NULL", "'x'", "X'01'"}
	for i := range 300 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO a(k, v) VALUES (%s, %d)`, keys[i%len(keys)], i%11))
	}
	for i := range 200 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO b VALUES (%s, %d.25)`, keys[(i*3)%len(keys)], i))
	}
	p := newSegPair(t, stmts...)
	qs := []struct{ kept, plain string }{
		{`SELECT a.id, (SELECT avg(x) FROM b WHERE b.k = a.k) FROM a`,
			`SELECT a.id, (SELECT avg(x) FROM b WHERE b.k = a.k AND random() IS NOT NULL) FROM a`},
		{`SELECT a.id FROM a WHERE a.v > (SELECT count(*) FROM b WHERE b.k = a.k) / 8`,
			`SELECT a.id FROM a WHERE a.v > (SELECT count(*) FROM b WHERE b.k = a.k AND random() IS NOT NULL) / 8`},
		{`SELECT a.id, (SELECT min(x) FROM b WHERE b.k = a.k AND b.x > a.v * 10) FROM a`,
			`SELECT a.id, (SELECT min(x) FROM b WHERE b.k = a.k AND b.x > a.v * 10 AND random() IS NOT NULL) FROM a`},
		{`SELECT a.id, sum(a.v), (SELECT count(*) FROM b WHERE b.x > sum(a.v)) FROM a GROUP BY a.id`,
			`SELECT a.id, sum(a.v), (SELECT count(*) FROM b WHERE b.x > sum(a.v) AND random() IS NOT NULL) FROM a GROUP BY a.id`},
	}
	for _, q := range qs {
		subKeepHits.Store(0)
		got := p.mustPlain(q.kept)
		if subKeepHits.Load() == 0 {
			t.Errorf("%s: no run kept", q.kept)
		}
		subKeepHits.Store(0)
		want := p.mustPlain(q.plain)
		if n := subKeepHits.Load(); n != 0 {
			t.Errorf("%s: %d runs kept", q.plain, n)
		}
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n kept %.400s\n run  %.400s", q.kept, g, w)
		}
	}
	// An aggregate query binding the enclosing row in its expressions
	// (bindAggOuterRefs) is not kept: such a binding may resolve at run time.
	subKeepHits.Store(0)
	got := p.mustPlain(`SELECT a.id, (SELECT sum(b.x * a.v) FROM b WHERE b.k = a.k) FROM a`)
	if n := subKeepHits.Load(); n != 0 {
		t.Errorf("an aggregate reading the enclosing row: %d runs kept", n)
	}
	want := p.mustPlain(`SELECT a.id, (SELECT sum(b.x * a.v) FROM b WHERE b.k = a.k AND random() IS NOT NULL) FROM a`)
	if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
		t.Errorf("an aggregate reading the enclosing row:\n kept %.400s\n run  %.400s", g, w)
	}
}
