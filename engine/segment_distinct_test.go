package engine

import (
	"fmt"
	"testing"
)

// TestSegmentDistinctMatchesTheLoop: DISTINCT over a clean int64 block comes
// off the block in first-seen order; it must return the loop's rows in the
// loop's order, served where claimed and declined otherwise.
func TestSegmentDistinctMatchesTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t (id INTEGER PRIMARY KEY, k INTEGER, n INTEGER, s TEXT, m)`}
	for i := 1; i <= 3000; i++ {
		n := fmt.Sprint(i % 7)
		if i%11 == 0 {
			n = "NULL"
		}
		m := fmt.Sprint((i * 7) % 13)
		if i%5 == 0 {
			m = "'x'"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d, %s, 's%d', %s)`, i, (i*7919)%97-40, n, i%9, m))
	}
	p := newSegPair(t, stmts...)
	cases := []struct {
		sql    string
		served bool
	}{
		{`SELECT DISTINCT k FROM t`, true},
		{`SELECT DISTINCT id FROM t`, true},
		{`SELECT count(*) FROM (SELECT DISTINCT k FROM t)`, true},
		{`SELECT sum(k) FROM (SELECT DISTINCT k FROM t)`, true},
		{`SELECT DISTINCT n FROM t`, false},             // NULLs
		{`SELECT DISTINCT s FROM t`, false},             // TEXT
		{`SELECT DISTINCT m FROM t`, false},             // mixed classes
		{`SELECT DISTINCT k FROM t WHERE k > 0`, false}, // a WHERE
		{`SELECT DISTINCT k, n FROM t`, false},          // two columns
		{`SELECT DISTINCT k FROM t ORDER BY k`, false},
	}
	check := func(stage string) {
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
				t.Errorf("%s %s:\n block %.200s\n loop  %.200s", stage, c.sql, g, w)
			}
			if stage == "clean" && JITEnabled() && (n > 0) != c.served {
				t.Errorf("%s: served=%d, want %v", c.sql, n, c.served)
			}
		}
	}
	check("clean")
	// A log to merge: the guard declines and the loop answers.
	p.delta(`INSERT INTO t VALUES (5000, 1000, 1, 's', 1)`, `DELETE FROM t WHERE id = 3`)
	check("delta")
}
