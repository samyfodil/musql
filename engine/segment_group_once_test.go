package engine

import (
	"fmt"
	"testing"
)

// TestSegGroupOnceBoundsHoisted: a columnar GROUP BY whose WHERE compares a
// column with date/time calls on literals, which the loop computes once
// (a once-block), takes the walk with those blocks run ahead of it, and
// answers as the loop does, with rows in the log too.
func TestSegGroupOnceBoundsHoisted(t *testing.T) {
	stmts := []string{`CREATE TABLE l (id INTEGER PRIMARY KEY, pk INTEGER, sk INTEGER, q INTEGER, d TEXT)`}
	for i := 1; i <= 3000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO l VALUES (%d, %d, %d, %d, '199%d-%02d-%02d')`, i, i%97, i%7, i%13, 3+i%4, 1+i%12, 1+i%28))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT pk, sk, 0.5 * sum(q) FROM l WHERE d >= date('1994-01-01') AND d < date('1994-01-01', '1 year') GROUP BY pk, sk`,
		`SELECT pk, count(*), sum(q) FROM l WHERE d < date('1995-06-30') GROUP BY pk`,
		`SELECT sk, sum(q) FROM l WHERE d >= date('2001-01-01') GROUP BY sk`,
	}
	check := func(stage string) {
		t.Helper()
		for _, q := range queries {
			before := segFilterServed.Load()
			got := p.mustFast(q)
			if segFilterServed.Load() == before {
				t.Errorf("%s %s: the walk did not serve it", stage, q)
			}
			want := p.mustPlain(q)
			if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
				t.Errorf("%s %s:\n walk %.300s\n loop %.300s", stage, q, g, w)
			}
		}
	}
	check("segments")
	p.delta(`INSERT INTO l VALUES (5000, 1, 1, 100, '1994-05-05')`, `DELETE FROM l WHERE id = 10`)
	check("with log")
}
