package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestCTEBodyRunsTheProbe: a CTE body materialized at run time runs the
// program the correlation probe compiled for it, when execSelect would have
// compiled the same one, and answers as it does compiled again. A body with
// an INDEXED BY, a LIMIT expression, a compound, a parenthesized join, no
// FROM or a transparent derived table is compiled again.
func TestCTEBodyRunsTheProbe(t *testing.T) {
	setup := []string{
		`CREATE TABLE f(id INTEGER PRIMARY KEY, k INT, g INT, v REAL)`,
		`CREATE TABLE d(k INTEGER PRIMARY KEY, w INT, name TEXT)`,
		`CREATE INDEX f_k ON f(k)`,
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
	queries := []struct {
		sql    string
		served bool
	}{
		{`WITH a AS (SELECT k, sum(v) s FROM f GROUP BY k), b AS (SELECT k, count(*) n FROM f WHERE g < 3 GROUP BY k)
			SELECT a.k, a.s, b.n FROM a, b WHERE a.k = b.k ORDER BY a.k`, true},
		{`WITH a AS (SELECT k, sum(v) s FROM f WHERE g IN (SELECT w FROM d WHERE name < 'n2') GROUP BY k)
			SELECT d.name, a.s FROM a, d, a a2 WHERE a.k = d.k AND a2.k = d.k + 1 ORDER BY 1`, true},
		{`WITH a AS (SELECT k, v FROM f ORDER BY v DESC LIMIT 20)
			SELECT d.name, a.v FROM a, d, a a2 WHERE a.k = d.k AND a2.k = a.k ORDER BY 2, 1`, true},
		{`WITH a AS (SELECT k, sum(v) s FROM f INDEXED BY f_k GROUP BY k)
			SELECT d.name, a.s FROM a, d, a a2 WHERE a.k = d.k AND a2.k = d.k ORDER BY 1`, false},
		{`WITH a AS (SELECT k, v FROM f ORDER BY v LIMIT 10+5)
			SELECT d.name, a.v FROM a, d, a a2 WHERE a.k = d.k AND a2.k = a.k ORDER BY 2, 1`, false},
		{`WITH a AS (SELECT k FROM f WHERE g = 1 UNION ALL SELECT k FROM d)
			SELECT count(*) FROM a, d, a a2 WHERE a.k = d.k AND a2.k = a.k`, false},
		{`WITH a AS (SELECT f.k, d.name FROM f LEFT JOIN (d JOIN f f2 ON f2.id = d.k) ON f.k = d.w WHERE f.g = 2)
			SELECT count(*), max(a.name) FROM a, d, a a2 WHERE a.k = d.k AND a2.k = a.k`, false},
		{`WITH a AS (SELECT 3 k, 'x' t)
			SELECT d.name, a.t FROM a, d, a a2 WHERE a.k = d.k AND a2.k = a.k`, false},
		{`WITH a AS (SELECT k, v FROM (SELECT k, v, g FROM f) WHERE g = 4)
			SELECT count(*), sum(a.v) FROM a, d, a a2 WHERE a.k = d.k AND a2.k = a.k`, false},
	}
	results := func(off bool) ([]string, []int64) {
		cteProbeRunOffForTest = off
		defer func() { cteProbeRunOffForTest = false }()
		// Its own connection, so no program is taken from the other's plan
		// cache.
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		var out []string
		var runs []int64
		for _, q := range queries {
			before := cteProbeRuns.Load()
			_, rows, err := p.QueryArgs(q.sql, nil)
			if err != nil {
				t.Fatalf("%s: %v", q.sql, err)
			}
			out = append(out, fmt.Sprint(typedRows(rows)))
			runs = append(runs, cteProbeRuns.Load()-before)
		}
		return out, runs
	}
	want, _ := results(true)
	got, runs := results(false)
	for i, q := range queries {
		if got[i] != want[i] {
			t.Errorf("%s:\n got  %.400s\n want %.400s", q.sql, got[i], want[i])
		}
		if (runs[i] > 0) != q.served {
			t.Errorf("%s: %d runs of the probe, want served=%v", q.sql, runs[i], q.served)
		}
	}
}
