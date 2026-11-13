package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestCorrelatedSubqueryReadsCTEAsColumns: a correlated subquery opens its
// CTE again for every outer row, and every read sees the CTE's rows as a
// column read gives them -- a REAL column's integers as REALs, JSON without
// its subtype -- though the list was put that way once (slotFill.normalized).
func TestCorrelatedSubqueryReadsCTEAsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.musq")
	stmts := []string{`CREATE TABLE t(k INTEGER, v REAL, j TEXT)`, `CREATE TABLE o(k INTEGER)`}
	for i := range 12 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, '[%d]')`, i%4, i, i), fmt.Sprintf(`INSERT INTO o VALUES(%d)`, i%4))
	}
	buildDB(t, path, stmts...)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	q := `WITH c AS MATERIALIZED (SELECT k, v, json(j) AS j FROM t)
SELECT o.rowid, (SELECT group_concat(typeof(v) || ':' || json_quote(j), ' ') FROM c WHERE c.k = o.k) FROM o ORDER BY 1`
	_, rows, err := p.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 12 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, r := range rows {
		want := ""
		k := (r[0].I - 1) % 4
		for i := k; i < 12; i += 4 {
			if want != "" {
				want += " "
			}
			want += fmt.Sprintf(`real:"[%d]"`, i)
		}
		if got := string(r[1].S); got != want {
			t.Errorf("outer row %d: %s, want %s", r[0].I, got, want)
		}
	}
}
