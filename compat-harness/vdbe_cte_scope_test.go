// This file is the differential gate for CTE scoping: a CTE's body resolves in
// the scope where it was DEFINED, so an inner WITH that redefines a name the
// body references cannot reach it (with3.test 2.0).
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestCTELexicalScopeParity(t *testing.T) {
	got := map[string]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, _ := sql.Open(drv, dsn)
		db.SetMaxOpenConns(1)
		for _, q := range []string{
			`WITH x1 AS (SELECT 10), x2 AS (SELECT 11),
			   x3 AS (SELECT * FROM x1 UNION ALL SELECT * FROM x2),
			   x4 AS (WITH x1 AS (SELECT 12), x2 AS (SELECT 13) SELECT * FROM x3)
			 SELECT * FROM x4`,
			`WITH x1 AS (SELECT 10), x2 AS (WITH x1 AS (SELECT 12) SELECT * FROM x1) SELECT * FROM x2`,
			`WITH x1 AS (SELECT 10), x2 AS (SELECT * FROM x1) SELECT * FROM x2`,
			`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<4) SELECT * FROM c`,
		} {
			out := queryString(t, db, q)
			if drv == "sqlite" {
				got[q] = out
				continue
			}
			// Compare the ROWS; the auto-generated column NAME of a bare
			// "SELECT <literal>" reached through nested CTEs is a separate,
			// still-open gap (this engine names it from the outer binding).
			if rowsOf(got[q]) != rowsOf(out) {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", q, got[q], out)
			}
		}
		db.Close()
	}
}

// rowsOf strips queryString's leading "cols=[...]" so only the row data is
// compared.
func rowsOf(s string) string {
	if i := strings.Index(s, "] "); i >= 0 {
		return s[i+2:]
	}
	return s
}
