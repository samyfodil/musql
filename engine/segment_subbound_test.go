package engine

import (
	"fmt"
	"testing"
)

// TestSubqueryBoundMatchesTheLoop: a filtered count or sum whose bound is an
// uncorrelated scalar subquery resolves it once, as OpSubquery would, and runs
// the column kernel -- and must answer as the loop does, served where claimed.
func TestSubqueryBoundMatchesTheLoop(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`,
		`CREATE TABLE u (id INTEGER PRIMARY KEY, w INTEGER, name TEXT)`,
	}
	for i := 1; i <= 3000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d, 's%d')`, i, (i*37)%1000-300, i%50))
	}
	for i := 1; i <= 40; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO u VALUES (%d, %d, 's%d')`, i, i*11, i))
	}
	p := newSegPair(t, stmts...)
	cases := []struct {
		sql    string
		served bool
	}{
		{`SELECT count(*) FROM t WHERE v > (SELECT avg(v) FROM t)`, true}, // a REAL bound on an INTEGER column
		{`SELECT count(*) FROM t WHERE v <= (SELECT max(w) FROM u WHERE w < 200)`, true},
		{`SELECT sum(v) FROM t WHERE v > (SELECT min(w) FROM u)`, true},
		{`SELECT count(*) FROM t WHERE s < (SELECT name FROM u WHERE id = 7)`, true}, // TEXT
		{`SELECT count(*) FROM t WHERE v > (SELECT w FROM u WHERE id > 30)`, true},   // several rows: the first
		{`SELECT count(*) FROM t WHERE v > (SELECT w FROM u WHERE id > 99)`, false},  // no row: NULL declines
		{`SELECT count(*) FROM t WHERE v > (SELECT avg(w) FROM u WHERE u.id = t.id)`, false},
		{`SELECT count(*) FROM t WHERE v > (SELECT avg(v) FROM t) AND v < (SELECT max(w) FROM u)`, true},
	}
	for _, c := range cases {
		_, want, err := p.plain(c.sql)
		if err != nil {
			t.Fatalf("%s plain: %v", c.sql, err)
		}
		ResetSegFilterCountersForTest()
		_, got, err := p.fast(c.sql)
		if err != nil {
			t.Fatalf("%s fast: %v", c.sql, err)
		}
		n, _ := SegFilterCountersForTest()
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s: kernel %s, loop %s", c.sql, g, w)
		}
		if JITEnabled() && (n > 0) != c.served {
			t.Errorf("%s: served=%d, want %v", c.sql, n, c.served)
		}
	}
}
