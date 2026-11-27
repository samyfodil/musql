package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestAggStepLeanMatchesAggStep: every answer, error included, is the generic
// step's.
func TestAggStepLeanMatchesAggStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.musq")
	buildDB(t, path, `CREATE TABLE t(g INTEGER, a, b REAL, c TEXT)`,
		`INSERT INTO t VALUES (1, 1, 1.5, '12abc'), (1, NULL, NULL, NULL), (2, 9223372036854775807, 2.0, '3'),
			(2, 1, -0.5, 'x'), (3, '7', 1e308, '1e308'), (3, 2.5, 1e308, ''), (1, -4, 0.25, '0x10'), (4, NULL, NULL, NULL)`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	queries := []string{
		`SELECT count(*), count(a), sum(b), total(c), avg(b) FROM t`,
		`SELECT sum(a) FROM t`,
		`SELECT sum(a) FROM t WHERE g <> 2`,
		`SELECT sum(c), avg(c), total(a) FROM t`,
		`SELECT g, count(*), sum(b), avg(a) FROM t WHERE g <> 2 GROUP BY g ORDER BY g`,
		`SELECT g, sum(b) FROM t GROUP BY g HAVING count(a) > 1 ORDER BY sum(b)`,
		`SELECT g, count(a) FROM t GROUP BY g ORDER BY avg(b) DESC, g`,
		`SELECT count(*), sum(DISTINCT g), max(a) FROM t`,
		`SELECT count(*) FILTER (WHERE g > 1), sum(g) FROM t`,
		`SELECT g, a, sum(b) FROM t GROUP BY g ORDER BY g`,
		`SELECT sum(b), count(*) FROM t`,
		`WITH RECURSIVE r(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM r WHERE x < 1000) SELECT sum(x), count(*), avg(x), total(x * 0.5) FROM r`,
		`WITH RECURSIVE r(x) AS (SELECT 9223372036854775000 UNION ALL SELECT x + 100 FROM r WHERE x < 9223372036854775700) SELECT sum(x) FROM r`,
	}
	run := func(off bool) []string {
		old := aggStepLeanOff
		aggStepLeanOff = off
		defer func() { aggStepLeanOff = old }()
		var out []string
		for _, q := range queries {
			_, rows, err := p.QueryArgs(fmt.Sprintf("%s -- %v", q, off), nil)
			out = append(out, fmt.Sprint(rows, err))
		}
		return out
	}
	want, got := run(true), run(false)
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s:\n aggStep %s\n lean    %s", queries[i], want[i], got[i])
		}
	}
}
