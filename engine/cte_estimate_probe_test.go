package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestCTEEstimateFromProbe: a CTE body the correlation probe compiled with no
// enclosing query is the program cteBodyProgram would compile for its row
// estimate, so the estimate is taken from it: each statement plans and
// answers as it does with the body compiled again. Bodies with a GROUP BY, a
// LIMIT, a compound, an ORDER BY and a subquery, joined in either order, at
// the top and inside a subquery, where the probe ran under an enclosing query
// and is not taken.
func TestCTEEstimateFromProbe(t *testing.T) {
	setup := []string{
		`CREATE TABLE f(id INTEGER PRIMARY KEY, k INT, g INT, v REAL)`,
		`CREATE TABLE d(k INTEGER PRIMARY KEY, w INT, name TEXT)`,
		`CREATE INDEX d_w ON d(w)`,
	}
	for i := range 600 {
		setup = append(setup, fmt.Sprintf(`INSERT INTO f VALUES(%d, %d, %d, %d.25)`, i, i%40, i%7, i))
	}
	for i := range 40 {
		setup = append(setup, fmt.Sprintf(`INSERT INTO d VALUES(%d, %d, 'n%d')`, i, i%5, i))
	}
	setup = append(setup, `ANALYZE`)
	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path, setup...)
	queries := []string{
		`WITH a AS (SELECT k, sum(v) s FROM f GROUP BY k), b AS (SELECT k, count(*) n FROM f WHERE g < 3 GROUP BY k)
			SELECT a.k, a.s, b.n FROM a, b WHERE a.k = b.k ORDER BY a.k`,
		`WITH a AS (SELECT k, sum(v) s FROM f GROUP BY k), b AS (SELECT k, w FROM d WHERE w IN (SELECT w FROM d WHERE name = 'n3'))
			SELECT b.k, a.s FROM b, a WHERE a.k = b.k ORDER BY 1`,
		`WITH a AS (SELECT k FROM f WHERE v > 100 LIMIT 50), b AS (SELECT k, g FROM f WHERE id < 30)
			SELECT count(*), sum(b.g) FROM a, b, d WHERE a.k = b.k AND d.k = a.k`,
		`WITH a AS (SELECT k FROM f WHERE g = 1 UNION ALL SELECT k FROM d), b AS (SELECT k, max(v) m FROM f GROUP BY k)
			SELECT a.k, b.m FROM a, b WHERE a.k = b.k ORDER BY 1, 2`,
		`WITH a AS (SELECT k FROM f WHERE g = 1 UNION SELECT w FROM d), b AS (SELECT k, max(v) m FROM f GROUP BY k)
			SELECT a.k, b.m FROM b, a WHERE a.k = b.k ORDER BY 1, 2`,
		`WITH a AS (SELECT k, v FROM f ORDER BY v DESC LIMIT 20), b AS (SELECT * FROM d)
			SELECT b.name, a.v FROM a, b WHERE a.k = b.k ORDER BY 2`,
		`WITH a AS (SELECT k, sum(v) s FROM f GROUP BY k), b AS (SELECT k FROM d WHERE w = 2)
			SELECT name, (SELECT max(a.s) FROM a, b WHERE a.k = b.k AND a.k < d.k) FROM d ORDER BY k`,
		`SELECT k, (WITH a AS (SELECT k, count(*) n FROM f GROUP BY k), b AS (SELECT k FROM d WHERE w = 1)
			SELECT sum(n) FROM a, b WHERE a.k = b.k) FROM d WHERE k < 5`,
	}
	run := func(p *ReadOnlyPager, q string) string {
		_, rows, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	results := func(off bool) []string {
		cteEstFromProbeOffForTest = off
		defer func() { cteEstFromProbeOffForTest = false }()
		// Its own connection, so no program is taken from the other's plan
		// cache.
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		var out []string
		for _, q := range queries {
			out = append(out, run(p, "EXPLAIN QUERY PLAN "+q), run(p, q))
		}
		return out
	}
	want := results(true)
	before := cteEstFromProbe.Load()
	got := results(false)
	if cteEstFromProbe.Load() == before {
		t.Error("no estimate was taken from a probe")
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s:\n got  %.400s\n want %.400s", queries[i/2], got[i], want[i])
		}
	}
}
