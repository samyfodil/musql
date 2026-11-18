package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// TestOrOfBoundParameters holds an OR of "column = ?" terms to C's case 1
// (exprAnalyzeOrTerm's chngToIN), which turns it into "column IN (?, ...)" as
// it does an OR of literals: a parameter has no affinity. One group_concat
// cell shows the order the rows arrived in.
func TestOrOfBoundParameters(t *testing.T) {
	dir := t.TempDir()
	cPath := filepath.Join(dir, "or.sqlite")
	refDB, err := sql.Open("sqlite3", cPath)
	if err != nil {
		t.Fatal(err)
	}
	defer refDB.Close()
	stmts := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, val INTEGER, w TEXT COLLATE NOCASE, k INTEGER)`,
		`CREATE INDEX t_val ON t(val)`,
		`CREATE INDEX t_w ON t(w)`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, x TEXT)`,
	}
	for i := 1; i <= 60; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'n%d', %d, '%c', %d)`, i, i, (i*7)%5*10, "aBcAb"[i%5], i%4))
	}
	for i := range 4 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO u VALUES(%d, 'u%d')`, i, i))
	}
	for _, s := range stmts {
		if _, err := refDB.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	dsn := filepath.Join(dir, "or.musq")
	if err := sqliteconv.Import(cPath, dsn, sqliteconv.ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sql  string
		args []any
	}{
		{`SELECT group_concat(name) FROM t WHERE val = ? OR val = ?`, []any{int64(30), int64(10)}},
		{`SELECT group_concat(name) FROM t WHERE val = ?1 OR val = ?2`, []any{int64(10), int64(30)}},
		{`SELECT group_concat(name) FROM t WHERE val = ? OR val = ? OR val = ?`, []any{int64(30), int64(10), int64(30)}},
		{`SELECT group_concat(name) FROM t WHERE val = ? OR val = ?`, []any{int64(20), int64(20)}},
		{`SELECT group_concat(name) FROM t WHERE val = 10 OR val = ?`, []any{int64(40)}},
		{`SELECT group_concat(name) FROM t WHERE val = ? OR val = ?`, []any{"30", "10"}},
		{`SELECT group_concat(name) FROM t WHERE val = ? OR val = ?`, []any{nil, int64(10)}},
		{`SELECT group_concat(name) FROM t WHERE w = ? OR w = ?`, []any{"b", "A"}},
		{`SELECT group_concat(name) FROM t WHERE id = ? OR id = ?`, []any{int64(40), int64(3)}},
		{`SELECT group_concat(name) FROM t WHERE k = ? OR k = ?`, []any{int64(3), int64(1)}},
		{`SELECT count(*), group_concat(name) FROM t WHERE (val = ? OR val = ?) AND k > ?`, []any{int64(40), int64(0), int64(1)}},
		{`SELECT group_concat(t.name || u.x) FROM t JOIN u ON u.id = t.k WHERE t.val = ? OR t.val = ?`, []any{int64(30), int64(0)}},
		{`SELECT name FROM t WHERE val = ? OR val = ?`, []any{int64(30), int64(10)}},
	} {
		eargs := make([]engine.Value, len(c.args))
		for i, a := range c.args {
			switch x := a.(type) {
			case int64:
				eargs[i] = evInt(x)
			case string:
				eargs[i] = evText(x)
			default:
				eargs[i] = evNull
			}
		}
		compareSelectArgs(t, dsn, refDB, c.sql, eargs, c.args)
	}
}
