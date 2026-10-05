package driver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestExplainTakesTheWrappedStatementsParameters: "EXPLAIN <stmt>" PREPARES
// <stmt> and describes its program, so it takes exactly the parameters <stmt>
// takes -- C SQLite binds into one like any other statement (verified:
// "EXPLAIN SELECT a FROM t WHERE a=?" prepared with one argument answers its
// listing). NumInput comes from engine.ParseParamInfo, which saw only the
// unparseable "EXPLAIN ..." text and reported 0, so database/sql refused the
// call with "expected 0 arguments, got 1" before the driver ever ran.
func TestExplainTakesTheWrappedStatementsParameters(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, s := range []string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, tc := range []struct {
		sql  string
		args []any
	}{
		{`EXPLAIN SELECT a FROM t WHERE a=?`, []any{1}},
		{`EXPLAIN QUERY PLAN SELECT a FROM t WHERE a=?`, []any{1}},
		{`EXPLAIN SELECT a FROM t WHERE a=? AND b=?`, []any{1, "x"}},
		{`EXPLAIN SELECT a FROM t WHERE a=:one`, []any{sql.Named("one", 1)}},
	} {
		rows, err := db.Query(tc.sql, tc.args...)
		if err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Errorf("%s: %v", tc.sql, err)
		}
		rows.Close()
		if n == 0 {
			t.Errorf("%s answered no rows", tc.sql)
		}
	}
}
