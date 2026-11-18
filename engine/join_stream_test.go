package engine

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// TestScanStreamsBelowOneRowSources: a table scan streams where its loop is
// rewound once a run, as the outermost one is -- and so does the first one
// below sources of at most one row each, an aggregate with no GROUP BY as a
// subquery or a CTE (TPC-DS q23's best_ss_customer). One below a source of
// more rows keeps its materialized rows, which its rewinds read again.
func TestScanStreamsBelowOneRowSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path, `CREATE TABLE t(id INTEGER PRIMARY KEY, a INT)`, `CREATE TABLE u(id INTEGER PRIMARY KEY, a INT)`)
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	for i := 1; i <= 50; i++ {
		for _, q := range []string{fmt.Sprintf(`INSERT INTO t VALUES(%d, %d)`, i, i%9), fmt.Sprintf(`INSERT INTO u VALUES(%d, %d)`, i, i%5)} {
			if err := n.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := n.Commit(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, c := range []struct {
		sql  string
		want []string // the tables whose opens stream, in program order
	}{
		{`SELECT t.id FROM t WHERE a > 3`, []string{"t"}},
		{`SELECT t.id FROM (SELECT max(a) AS m FROM u) s, t WHERE t.a > s.m`, []string{"t"}},
		{`WITH m AS (SELECT max(a) AS mx FROM u) SELECT count(*), sum(t.a) FROM m, t WHERE t.a < m.mx`, []string{"t"}},
		{`SELECT t.id FROM (SELECT a FROM u LIMIT 2) s, t WHERE t.a > s.a`, nil},
		{`SELECT t.id, u.id FROM (SELECT max(a) AS m FROM u) s, t, u WHERE t.a > s.m AND u.a < t.a`, []string{"t"}},
	} {
		stmt, err := ParseSelect(c.sql)
		if err != nil {
			t.Fatal(err)
		}
		pop := p.pushCTEScopeOf(stmt)
		prog, err := compileSelectScanRow(p, stmt, nil, nil)
		pop()
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		var got []string
		for _, in := range prog.Insns {
			if rt, ok := in.P4.(*resolvedTable); ok && in.Op == OpOpenRead && in.P5&1 != 0 {
				got = append(got, rt.name)
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: streaming opens %v, want %v", c.sql, got, c.want)
		}
		if _, rows, err := p.QueryArgs(c.sql, nil); err != nil || len(rows) == 0 {
			t.Errorf("%s: %d rows, %v", c.sql, len(rows), err)
		}
	}
}
