// This file is the differential gate for a VIEW's WITH-clause scope: a view's
// body sees its OWN WITH clause and nothing else. A CTE in the statement that
// QUERIES the view must not shadow a table the view names (view2.test), and
// the body's own CTEs must still resolve -- this engine had it backwards on
// both counts.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestViewCTEScopeParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(x,y)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE VIEW v2 AS SELECT * FROM t1`,
		`CREATE VIEW v3 AS WITH t1(p,q) AS (SELECT 9,9) SELECT * FROM t1`,
		`CREATE VIEW v4 AS WITH cc(p,q) AS (SELECT 9,9) SELECT * FROM cc`,
	}
	probes := []string{
		// The caller's CTE must NOT reach inside the view.
		`WITH t1(a, b) AS ( SELECT 3, 4 ) SELECT * FROM v2`,
		// The view's OWN CTE shadows the same-named table, inside the view only.
		`SELECT * FROM v3`,
		`SELECT * FROM v4`,
		// ... and does not leak out of it.
		`SELECT * FROM t1`,
	}
	got := map[string]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, err := sql.Open(drv, dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range setup {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", drv, q, err)
			}
		}
		for _, q := range probes {
			out := queryString(t, db, q)
			if drv == "sqlite" {
				got[q] = out
				continue
			}
			if got[q] != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", q, got[q], out)
			}
		}
		db.Close()
	}
}

// queryString renders a query's column names and rows (or its error) as one
// comparable string. A statement that fails only once rows are FETCHED counts
// as an error too (rows.Err below): C SQLite defers several of its own --
// "frame starting offset must be a non-negative integer", "argument of ntile
// must be a positive integer" -- past prepare, and treating those as a
// successful empty result would hide a genuine accept/reject divergence.
func queryString(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		return "ERR"
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := fmt.Sprintf("cols=%v", cols)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "SCANERR"
		}
		out += fmt.Sprintf(" %v", vals)
	}
	if rows.Err() != nil {
		return "ERR"
	}
	return out
}
