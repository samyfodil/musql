package engine

import (
	"fmt"
	"testing"
)

// TestSubTermDecidedByNames: a WHERE term whose subqueries name nothing
// outside them is pushed down without a trial compile of the subquery, and
// answers as the same query with the term written where the join ends.
func TestSubTermDecidedByNames(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t (a INTEGER, b INTEGER)`,
		`CREATE TABLE u (c INTEGER, d INTEGER)`,
		`CREATE TABLE w (e INTEGER)`,
	}
	for i := 0; i < 60; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d)`, i, i%7), fmt.Sprintf(`INSERT INTO u VALUES (%d, %d)`, i%40, i%5))
	}
	for i := 0; i < 9; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO w VALUES (%d)`, i))
	}
	p := newSegPair(t, stmts...)
	for _, tc := range []struct {
		q, want string
		decided bool
	}{
		{`WITH c(x) AS (SELECT e FROM w WHERE e % 2 = 0) SELECT t.a, u.d FROM t, u WHERE t.a = u.c AND t.b IN (SELECT x FROM c) ORDER BY 1, 2`,
			`SELECT t.a, u.d FROM t, u WHERE t.a = u.c AND t.b IN (0, 2, 4, 6, 8) ORDER BY 1, 2`, true},
		{`SELECT t.a, u.d FROM t, u WHERE t.a = u.c AND t.b IN (SELECT e FROM w WHERE e > 2) AND u.d < (SELECT max(e) - 5 FROM w) ORDER BY 1, 2`,
			`SELECT t.a, u.d FROM t, u WHERE t.a = u.c AND t.b IN (3, 4, 5, 6, 7, 8) AND u.d < 3 ORDER BY 1, 2`, true},
		{`SELECT t.a, u.d FROM t, u WHERE t.a = u.c AND NOT EXISTS (SELECT 1 FROM w WHERE e = 30) AND t.b > 4 ORDER BY 1, 2`,
			`SELECT t.a, u.d FROM t, u WHERE t.a = u.c AND t.b > 4 ORDER BY 1, 2`, true},
		{`SELECT t.a, u.d FROM t, u WHERE t.a = u.c AND t.b IN (SELECT e FROM w WHERE e > u.d + 3) ORDER BY 1, 2`,
			`SELECT t.a, u.d FROM t, u WHERE t.a = u.c AND t.b > u.d + 3 AND t.b <= 8 ORDER BY 1, 2`, false},
	} {
		was := subNamesDecided.Load()
		got := p.mustFast(tc.q)
		if decided := subNamesDecided.Load() > was; decided != tc.decided {
			t.Errorf("%s: decided by names %v, want %v", tc.q, decided, tc.decided)
		}
		want := p.mustFast(tc.want)
		if len(want) == 0 {
			t.Fatalf("%s: no rows", tc.want)
		}
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n got  %.300s\n want %.300s", tc.q, g, w)
		}
	}
}
