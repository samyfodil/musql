package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestCTEProbeCountsReplayed: a CTE body that references another CTE or a
// view counts those references each time it is compiled, so a later probe
// of it adds the counts its first probe made instead of compiling it again.
// The counts decide which CTEs are materialized (nUse), so each statement's
// plan and rows must be the same with every probe compiled.
func TestCTEProbeCountsReplayed(t *testing.T) {
	setup := []string{
		`CREATE TABLE f(id INTEGER PRIMARY KEY, k INT, g INT, v REAL)`,
		`CREATE TABLE d(k INTEGER PRIMARY KEY, w INT, name TEXT)`,
		`CREATE INDEX f_k ON f(k)`,
		`CREATE VIEW fv AS SELECT k, g, v FROM f WHERE g < 5`,
		`CREATE VIEW fvv AS SELECT a.k, a.v FROM fv a, fv b WHERE a.k = b.k AND b.g = 1`,
	}
	for i := range 400 {
		setup = append(setup, fmt.Sprintf(`INSERT INTO f VALUES(%d, %d, %d, %d.5)`, i, i%30, i%7, i))
	}
	for i := range 30 {
		setup = append(setup, fmt.Sprintf(`INSERT INTO d VALUES(%d, %d, 'n%d')`, i, i%4, i))
	}
	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path, setup...)
	queries := []struct {
		sql    string
		replay bool
	}{
		{`WITH a AS (SELECT DISTINCT k FROM f WHERE g = 2),
		      b AS (SELECT a.k, sum(f.v) s FROM a, f WHERE f.k = a.k GROUP BY a.k),
		      c AS (SELECT k, s FROM b WHERE s > 10)
			SELECT d.name, c.s FROM c, d WHERE c.k = d.k ORDER BY 1`, true},
		{`WITH b AS (SELECT fv.k, sum(fv.v) s FROM fv, d WHERE fv.k = d.k GROUP BY fv.k),
		      c AS (SELECT b.k, b.s FROM b, d WHERE b.k = d.w)
			SELECT count(*), sum(c.s) FROM c, d, c c2 WHERE c.k = d.k AND c2.k = c.k`, true},
		{`WITH b AS (SELECT k, v FROM fvv),
		      c AS (SELECT b.k, count(*) n FROM b, d WHERE b.k = d.k GROUP BY b.k)
			SELECT d.name, c.n FROM c, d WHERE c.k = d.k ORDER BY 1`, true},
		{`WITH a AS MATERIALIZED (SELECT k, g FROM f WHERE v > 20),
		      b AS (SELECT a.k, count(*) n FROM a, d WHERE a.k = d.k GROUP BY a.k),
		      c AS NOT MATERIALIZED (SELECT b.k, b.n FROM b, a WHERE b.k = a.g)
			SELECT count(*), sum(c.n) FROM c, d, c c2 WHERE c.k = d.k AND c2.k = c.k`, true},
		{`WITH a AS (SELECT k, v FROM f WHERE g = 3)
			SELECT count(*) FROM a, d WHERE a.k = d.k`, false},
	}
	results := func(off bool) ([]string, []int64) {
		cteProbeCountsOffForTest = off
		defer func() { cteProbeCountsOffForTest = false }()
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		var out []string
		var replays []int64
		for _, q := range queries {
			before := cteProbeCountReplays.Load()
			_, plan, err := p.QueryArgs("EXPLAIN QUERY PLAN "+q.sql, nil)
			if err != nil {
				t.Fatalf("plan %s: %v", q.sql, err)
			}
			_, rows, err := p.QueryArgs(q.sql, nil)
			if err != nil {
				t.Fatalf("%s: %v", q.sql, err)
			}
			out = append(out, fmt.Sprint(typedRows(plan), typedRows(rows), p.viewRefCount))
			replays = append(replays, cteProbeCountReplays.Load()-before)
		}
		return out, replays
	}
	want, _ := results(true)
	got, replays := results(false)
	for i, q := range queries {
		if got[i] != want[i] {
			t.Errorf("%s:\n got  %.600s\n want %.600s", q.sql, got[i], want[i])
		}
		if (replays[i] > 0) != q.replay {
			t.Errorf("%s: %d probes counted again, want replay=%v", q.sql, replays[i], q.replay)
		}
	}
}
