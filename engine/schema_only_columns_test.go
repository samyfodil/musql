package engine

import (
	"fmt"
	"testing"
)

// TestSchemaOnlyResolveReadsNoRows is the schema-only resolve of an IN
// subquery's FROM (a CTE and a base table) giving their columns and none of
// their rows: compiling the comparison reads no table.
func TestSchemaOnlyResolveReadsNoRows(t *testing.T) {
	db := memdbTestDB(t)
	stmts := []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`}
	for i := range 50 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'b%d')`, i, i))
	}
	for _, q := range stmts {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	q := `SELECT a FROM t WHERE b IN (WITH c(x) AS (SELECT b FROM t) SELECT x FROM c, t WHERE x = t.b)`
	stmt, err := parseSelectStatement(`WITH c(x) AS (SELECT b FROM t) SELECT x FROM c, t WHERE x = t.b`)
	if err != nil {
		t.Fatal(err)
	}
	defer p.pushCTEScope(stmt.CTEs)()
	jts, err := p.resolveFromSchemaOnly(stmt.From)
	if err != nil {
		t.Fatal(err)
	}
	if len(jts) != 2 {
		t.Fatalf("%d tables, want 2", len(jts))
	}
	for _, jt := range jts {
		if len(jt.rows) != 0 || len(jt.tbl.cols) == 0 {
			t.Fatalf("%s: %d rows and %d columns, want its columns and no rows", jt.name, len(jt.rows), len(jt.tbl.cols))
		}
	}
	_, rows, err := p.QueryArgs(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 50 {
		t.Fatalf("%d rows, want 50", len(rows))
	}
}
