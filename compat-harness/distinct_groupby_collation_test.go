package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// SELECT DISTINCT over a GROUP BY with various collation combinations.
func TestDistinctGroupByCollation(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a TEXT, n TEXT COLLATE NOCASE, r TEXT COLLATE RTRIM, k INT)`,
		`INSERT INTO t VALUES('A','A','x ',1),('a','a','x',1),('b','b','y ',2),('B','B','y',2)`,
	}
	qs := []string{
		`SELECT DISTINCT a FROM t GROUP BY a COLLATE NOCASE`,
		`SELECT DISTINCT a FROM t GROUP BY a COLLATE RTRIM`,
		`SELECT DISTINCT a COLLATE NOCASE FROM t GROUP BY a`,
		`SELECT DISTINCT a COLLATE RTRIM FROM t GROUP BY a`,
		`SELECT DISTINCT a FROM t GROUP BY a`,
		`SELECT DISTINCT n FROM t GROUP BY n`,
		`SELECT DISTINCT n FROM t GROUP BY n COLLATE NOCASE`,
		`SELECT DISTINCT n FROM t GROUP BY n COLLATE BINARY`,
		`SELECT DISTINCT n COLLATE BINARY FROM t GROUP BY n`,
		`SELECT DISTINCT r FROM t GROUP BY r`,
		`SELECT DISTINCT r FROM t GROUP BY k`,
		`SELECT DISTINCT a, n FROM t GROUP BY k`,
		`SELECT DISTINCT n, a FROM t GROUP BY k`,
		`SELECT DISTINCT a, count(*) FROM t GROUP BY a COLLATE NOCASE`,
		`SELECT DISTINCT upper(a) FROM t GROUP BY a COLLATE NOCASE`,
		`SELECT DISTINCT max(n) FROM t GROUP BY k`,
		`SELECT DISTINCT n FROM t GROUP BY n HAVING count(*)>0`,
		`SELECT DISTINCT n FROM t GROUP BY n ORDER BY n`,
		`SELECT DISTINCT n FROM t GROUP BY n ORDER BY n COLLATE BINARY`,
		`SELECT DISTINCT n FROM t GROUP BY n LIMIT 1`,
	}
	for qi, q := range qs {
		qi, q := qi, q
		t.Run(fmt.Sprintf("%02d", qi), func(t *testing.T) {
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
