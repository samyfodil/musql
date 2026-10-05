package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestMinMaxSubqueryAnchor tests min()/max() inside select-list subqueries in GROUP BY queries.
func TestMinMaxSubqueryAnchor(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(2,'z')`,
		`CREATE TABLE u(c)`, `INSERT INTO u VALUES(1),(2)`,
		`CREATE TABLE m(k COLLATE NOCASE, v)`, `INSERT INTO m VALUES('A',1),('a',2),('B',3),('b',4)`,
	}
	declines := map[string]bool{
		`SELECT a, (SELECT max(t.b) FROM u WHERE u.c=t.a) FROM t GROUP BY a`: true,
		`SELECT a, b, (SELECT max(t.b) FROM u) FROM t GROUP BY a`:            true,
		`SELECT a, (SELECT max(t.b) FROM u) FROM t GROUP BY a ORDER BY b`:    true,
	}
	for qi, q := range []string{
		`SELECT a, (SELECT max(t.b) FROM u) FROM t GROUP BY a`,
		`SELECT a, (SELECT max(t.b)+1 FROM u) FROM t GROUP BY a`,
		`SELECT (SELECT max(t.b) FROM u) FROM t GROUP BY a`,
		`SELECT a, max(b), (SELECT max(t.b) FROM u) FROM t GROUP BY a`,
		`SELECT a, (SELECT min(t.b) FROM u) FROM t GROUP BY a`,
		`SELECT a, (SELECT max(t.b) FROM u WHERE u.c=t.a) FROM t GROUP BY a`,
		`SELECT a, b, (SELECT max(t.b) FROM u) FROM t GROUP BY a`,
		`SELECT k, max(v) FROM m GROUP BY k`,
		`SELECT k, min(v) FROM m GROUP BY k`,
		`SELECT k, v FROM m GROUP BY k`,
		`SELECT a, (SELECT sum(t.a) FROM u) FROM t GROUP BY a`,
		`SELECT a, (SELECT count(t.b) FROM u) FROM t GROUP BY a`,
		`SELECT a, (SELECT max(t.b) FROM u) FROM t GROUP BY a HAVING count(*)>0`,
		`SELECT a, (SELECT max(t.b) FROM u) FROM t GROUP BY a ORDER BY b`,
	} {
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
			if declines[q] {
				if out[1] != "ERR" {
					t.Errorf("%s: expected a DECLINE (the anchor row is observable here), got %s", q, out[1])
				}
				return
			}
			if out[0] != out[1] {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", q, out[0], out[1])
			}
		})
	}
}
