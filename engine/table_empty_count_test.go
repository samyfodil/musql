package engine

import (
	"fmt"
	"testing"
)

// TestTableEmptyFromCounts: a join's emptiness test of a later table
// (OpIfEmpty) reads the segments' row counts with the log's rows merged, and
// answers as the rows are: empty in its segments, emptied by the log, filled
// by it, and partly deleted.
func TestTableEmptyFromCounts(t *testing.T) {
	stmts := []string{`CREATE TABLE a(x)`, `CREATE TABLE b(y)`, `CREATE TABLE c(z)`, `CREATE TABLE d(w)`}
	for i := 0; i < 30; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO a VALUES(%d)`, i), fmt.Sprintf(`INSERT INTO b VALUES(%d)`, i))
	}
	for i := 0; i < 5; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO c VALUES(%d)`, i))
	}
	p := newSegPair(t, stmts...)
	count := func(q string) string {
		t.Helper()
		return fmt.Sprint(typedRows(p.mustFast(q)))
	}
	for _, tc := range []struct {
		delta []string
		c, d  string
	}{
		{nil, "[1:4500|]", "[1:0|]"},
		{[]string{`DELETE FROM c`}, "[1:0|]", "[1:0|]"},
		{[]string{`INSERT INTO c VALUES(7), (8)`, `INSERT INTO d VALUES(1)`}, "[1:1800|]", "[1:900|]"},
		{[]string{`DELETE FROM c WHERE z = 7`, `DELETE FROM d`}, "[1:900|]", "[1:0|]"},
	} {
		p.delta(tc.delta...)
		if got := count(`SELECT count(*) FROM a, b, c`); got != tc.c {
			t.Errorf("after %q: a, b, c counts %s, want %s", tc.delta, got, tc.c)
		}
		if got := count(`SELECT count(*) FROM a, b, d`); got != tc.d {
			t.Errorf("after %q: a, b, d counts %s, want %s", tc.delta, got, tc.d)
		}
	}
}
