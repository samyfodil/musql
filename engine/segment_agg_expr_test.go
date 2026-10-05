package engine

import (
	"fmt"
	"testing"
)

// TestSegmentAggregateInAnExpression verifies aggregates in expressions.
func TestSegmentAggregateInAnExpression(t *testing.T) {
	stmts := []string{`CREATE TABLE nums(x INTEGER)`}
	for i := 1; i <= 5; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO nums VALUES(%d)`, i))
	}
	p := newSegPair(t, stmts...)

	for _, q := range []string{
		`SELECT count(*) FROM nums`,
		`SELECT sum(x) FROM nums`,
		`SELECT min(x), max(x) FROM nums`,
		`SELECT count(*) FROM nums WHERE x > 2`,
		`SELECT count(*) + 1 FROM nums`,
		`SELECT 1 + count(*) FROM nums`,
		`SELECT count(*) * 2 FROM nums`,
		`SELECT -count(*) FROM nums`,
		`SELECT -count(*) * -31 FROM nums`,
		`SELECT sum(x) + 1 FROM nums`,
		`SELECT max(x) + 1 FROM nums`,
		`SELECT count(*) - count(*) FROM nums`,
		`SELECT abs(-count(*)) FROM nums`,
		`SELECT count(*) + 1 FROM nums WHERE x > 2`,
		`SELECT min(x) + max(x) FROM nums`,
	} {
		want := p.mustPlain(q)
		got := p.mustFast(q)
		render := func(rows [][]Value) string {
			out := ""
			for _, r := range rows {
				for _, c := range r {
					out += fmt.Sprintf("%d:%d|", c.Typ, c.I)
				}
				out += ";"
			}
			return out
		}
		if render(got) != render(want) {
			t.Errorf("%s\n fast:  %s\n plain: %s", q, render(got), render(want))
		}
	}
}
