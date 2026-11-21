package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestCTESchemaFromCountedProbe: a CTE body that references another CTE or a
// view has its column names read off the program its correlation probe
// compiled, and the references that probe counted are counted again, as the
// compile they replace would count them. The counts decide which CTEs are
// materialized (nUse), so each statement's plan and rows must be the same
// with the body compiled again.
func TestCTESchemaFromCountedProbe(t *testing.T) {
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
		sql       string
		fromProbe bool
	}{
		{`WITH a AS (SELECT k, sum(v) s FROM f GROUP BY k),
		      b AS (SELECT a.k, a.s FROM a, d WHERE a.k = d.k AND a.s > (SELECT avg(s) FROM a))
			SELECT d.name, b.s FROM b, d WHERE b.k = d.w ORDER BY 1, 2`, true},
		{`WITH a AS (SELECT DISTINCT k FROM f WHERE g = 2),
		      b AS (SELECT a.k, count(*) n FROM a, f WHERE f.k = a.k GROUP BY a.k)
			SELECT count(*), sum(b.n) FROM b, d, b b2 WHERE b.k = d.k AND b2.k = b.k`, true},
		{`WITH b AS (SELECT fvv.k, sum(fvv.v) s FROM fvv, d WHERE fvv.k = d.k GROUP BY fvv.k)
			SELECT d.name, b.s FROM b, d WHERE b.k = d.k ORDER BY 1`, true},
		{`WITH a AS MATERIALIZED (SELECT k, g FROM f WHERE v > 20),
		      b AS NOT MATERIALIZED (SELECT a.k, count(*) n FROM a, d WHERE a.k = d.k GROUP BY a.k)
			SELECT count(*), sum(b.n) FROM b, d, b b2, a WHERE b.k = d.k AND b2.k = b.k AND a.k = b.k`, true},
		{`WITH a AS (SELECT k, v FROM f WHERE g = 3)
			SELECT count(*) FROM a, d WHERE a.k = d.k`, false},
	}
	results := func(off bool) ([]string, []int64) {
		cteSchemaFromProbeOffForTest = off
		defer func() { cteSchemaFromProbeOffForTest = false }()
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		var out []string
		var taken []int64
		for _, q := range queries {
			before := cteSchemaFromProbe.Load()
			_, plan, err := p.QueryArgs("EXPLAIN QUERY PLAN "+q.sql, nil)
			if err != nil {
				t.Fatalf("plan %s: %v", q.sql, err)
			}
			_, rows, err := p.QueryArgs(q.sql, nil)
			if err != nil {
				t.Fatalf("%s: %v", q.sql, err)
			}
			out = append(out, fmt.Sprint(typedRows(plan), typedRows(rows), p.viewRefCount))
			taken = append(taken, cteSchemaFromProbe.Load()-before)
		}
		return out, taken
	}
	want, _ := results(true)
	got, taken := results(false)
	for i, q := range queries {
		if got[i] != want[i] {
			t.Errorf("%s:\n got  %.600s\n want %.600s", q.sql, got[i], want[i])
		}
		if (taken[i] > 0) != q.fromProbe {
			t.Errorf("%s: %d column lists read off a probe, want %v", q.sql, taken[i], q.fromProbe)
		}
	}
}
