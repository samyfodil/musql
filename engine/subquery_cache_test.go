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
	// Not kept: an aggregate query binding the enclosing row in its
	// expressions (bindAggOuterRefs), which may resolve at run time; a
	// virtual table, whose arguments are coded as it opens; and a window,
	// whose projection compiles off the chain.
	for _, q := range []struct{ q, plain string }{
		{`SELECT a.id, (SELECT sum(b.x * a.v) FROM b WHERE b.k = a.k) FROM a`,
			`SELECT a.id, (SELECT sum(b.x * a.v) FROM b WHERE b.k = a.k AND random() IS NOT NULL) FROM a`},
		{`SELECT a.id, (SELECT value FROM json_each((SELECT json_array(a.v) WHERE a.k IS NOT NULL)) WHERE a.k = 1) FROM a`,
			`SELECT a.id, (SELECT value FROM json_each((SELECT json_array(a.v) WHERE a.k IS NOT NULL)) WHERE a.k = 1 AND random() IS NOT NULL) FROM a`},
		{`SELECT a.id, (SELECT a.v + sum(x) OVER () FROM b WHERE b.k = a.k) FROM a`,
			`SELECT a.id, (SELECT a.v + sum(x) OVER () FROM b WHERE b.k = a.k AND random() IS NOT NULL) FROM a`},
	} {
		subKeepHits.Store(0)
		got := p.mustPlain(q.q)
		if n := subKeepHits.Load(); n != 0 {
			t.Errorf("%s: %d runs kept", q.q, n)
		}
		want := p.mustPlain(q.plain)
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n kept %.400s\n run  %.400s", q.q, g, w)
		}
	}
	// A function that is not constant in a window's projection runs for
	// every enclosing row.
	// So does one in the body of a CTE each run reads afresh.
	for _, q := range []string{
		`SELECT count(DISTINCT (SELECT random() + sum(x) OVER () FROM b WHERE b.k = a.k LIMIT 1)) > 100 FROM a`,
		`WITH r AS NOT MATERIALIZED (SELECT random() + sum(x) OVER () AS v FROM b) SELECT count(DISTINCT (SELECT v FROM r WHERE a.k = 1)) > 30 FROM a`,
	} {
		if got := fmt.Sprint(typedRows(p.mustPlain(q))); got != "[1:1|]" {
			t.Errorf("%s: %s", q, got)
		}
	}
	// A CTE every reference reads one run of is kept.
	subKeepHits.Store(0)
	q := `WITH r AS (SELECT k, sum(x) s FROM b GROUP BY k) SELECT a.id FROM a, r r1 WHERE r1.k = a.k AND r1.s > (SELECT avg(s) FROM r r2 WHERE r2.k IS NOT a.k)`
	p.mustPlain(q)
	if subKeepHits.Load() == 0 {
		t.Errorf("%s: no run kept", q)
	}
}
