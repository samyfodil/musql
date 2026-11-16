package engine

import (
	"fmt"
	"testing"
)

// TestCTEBodyReusedAcrossRuns: a statement run again runs its CTE bodies from
// the programs its first run compiled, and they read the database and the
// parameters as they are at each run. Each answer is checked against the same
// query with the CTE written out as derived tables, compiled on its own.
func TestCTEBodyReusedAcrossRuns(t *testing.T) {
	db := memdbTestDB(t)
	for _, q := range []string{`CREATE TABLE t(a INTEGER, b TEXT)`, `INSERT INTO t VALUES(1, 'x'), (2, 'y'), (3, 'x'), (5, 'y')`} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	pairs := [][2]string{
		{`WITH c AS (SELECT b, sum(a) s FROM t WHERE a > ? GROUP BY b) SELECT c.b, c.s, d.s FROM c, c AS d WHERE c.b = d.b ORDER BY 1`,
			`SELECT c.b, c.s, d.s FROM (SELECT b, sum(a) s FROM t WHERE a > ?1 GROUP BY b) c, (SELECT b, sum(a) s FROM t WHERE a > ?1 GROUP BY b) d WHERE c.b = d.b ORDER BY 1`},
		{`WITH c AS (SELECT a, b, rank() OVER (PARTITION BY b ORDER BY a) r FROM t WHERE a > ?) SELECT c.a, d.a FROM c, c AS d WHERE c.b = d.b AND c.r = d.r + 1 ORDER BY 1`,
			`SELECT c.a, d.a FROM (SELECT a, b, rank() OVER (PARTITION BY b ORDER BY a) r FROM t WHERE a > ?1) c, (SELECT a, b, rank() OVER (PARTITION BY b ORDER BY a) r FROM t WHERE a > ?1) d WHERE c.b = d.b AND c.r = d.r + 1 ORDER BY 1`},
		{`WITH c AS (SELECT b, avg(a) m FROM t WHERE a > ? GROUP BY b) SELECT a FROM t WHERE a > (SELECT m FROM c WHERE c.b = t.b) ORDER BY 1`,
			`SELECT a FROM t WHERE a > (SELECT m FROM (SELECT b, avg(a) m FROM t WHERE a > ?1 GROUP BY b) c WHERE c.b = t.b) ORDER BY 1`},
	}
	run := func(q string, min int64) string {
		t.Helper()
		p, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		_, rows, err := p.QueryArgs(q, []Value{{Typ: Int, I: min}})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	before := cteBodyReuses.Load()
	for _, step := range []struct {
		min    int64
		insert string
	}{{0, ""}, {0, ""}, {1, ""}, {0, `INSERT INTO t VALUES(4, 'x'), (9, 'y')`}, {2, ""}, {0, `DELETE FROM t WHERE a = 3`}} {
		if step.insert != "" {
			if err := db.Exec(step.insert); err != nil {
				t.Fatal(err)
			}
		}
		for _, pr := range pairs {
			if got, want := run(pr[0], step.min), run(pr[1], step.min); got != want {
				t.Errorf("a > %d after %q: %s\n got %s, want %s", step.min, step.insert, pr[0], got, want)
			}
		}
	}
	if cteBodyReuses.Load() == before {
		t.Error("no run reused a compiled CTE body")
	}
}
