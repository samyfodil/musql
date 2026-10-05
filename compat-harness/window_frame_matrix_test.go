package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestWindowFrameMatrix tests window functions across all frame shapes:
// ROWS, RANGE, and GROUPS with various bounds, EXCLUDE clauses, collations,
// and ordering options.
func TestWindowFrameMatrix(t *testing.T) {
	setup := []string{
		`CREATE TABLE w(g INT, k INT, v TEXT, n TEXT COLLATE NOCASE)`,
		`INSERT INTO w VALUES(1,1,'a','a'),(1,2,'B','B'),(1,3,'a','a'),(2,1,'c','c'),(2,2,'C','C'),(2,3,NULL,NULL)`,
	}
	fns := []string{
		"row_number()", "rank()", "dense_rank()", "percent_rank()", "cume_dist()",
		"ntile(2)", "lag(v)", "lead(v)", "lag(v,2,'z')", "first_value(v)",
		"last_value(v)", "nth_value(v,2)", "count(*)", "sum(k)", "avg(k)",
		"min(v)", "max(v)", "group_concat(v)", "total(k)", "min(n)", "max(n)",
	}
	frames := []string{
		"",
		"ORDER BY k",
		"PARTITION BY g ORDER BY k",
		"PARTITION BY g ORDER BY k ROWS BETWEEN 1 PRECEDING AND CURRENT ROW",
		"PARTITION BY g ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING",
		"PARTITION BY g ORDER BY k RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING",
		"PARTITION BY g ORDER BY k GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW",
		"PARTITION BY g ORDER BY k ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING",
		"PARTITION BY g ORDER BY k ROWS BETWEEN 1 FOLLOWING AND 2 FOLLOWING",
		"PARTITION BY g ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW",
		"PARTITION BY g ORDER BY k RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE TIES",
		"PARTITION BY g ORDER BY k GROUPS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE GROUP",
		"ORDER BY v COLLATE NOCASE",
		"PARTITION BY n ORDER BY k",
		"ORDER BY k DESC NULLS FIRST",
	}
	for fi, fn := range fns {
		for wi, fr := range frames {
			fn, fr, fi, wi := fn, fr, fi, wi
			t.Run(fmt.Sprintf("%02d/%02d", fi, wi), func(t *testing.T) {
				q := fmt.Sprintf("SELECT g,k,v, %s OVER (%s) FROM w ORDER BY g,k", fn, fr)
				var out [2]string
				for i, drv := range []string{"sqlite3", "sqlite"} {
					p := filepath.Join(t.TempDir(), "x.db")
					db, err := sql.Open(drv, p)
					if err != nil {
						t.Fatal(err)
					}
					db.SetMaxOpenConns(1)
					for _, s := range setup {
						if _, err := db.Exec(s); err != nil {
							t.Fatalf("%s: %v", s, err)
						}
					}
					out[i] = renderQuery(db, q)
					db.Close()
				}
				if out[0] != out[1] {
					t.Errorf("%s\n  cgo: %s\n  mus: %s", q, out[0], out[1])
				}
			})
		}
	}
}
