package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// Parenthesized join group with USING coalesces correctly.
func TestParenJoinUsingGroup(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE u(a,c)`, `INSERT INTO u VALUES(1,'p'),(2,'q')`,
		`CREATE TABLE w(a,d)`, `INSERT INTO w VALUES(1,'m')`,
	}
	qs := []string{
		`SELECT * FROM u JOIN (t JOIN w USING(a))`,
		`SELECT * FROM u, (t JOIN w USING(a))`,
		`SELECT * FROM u CROSS JOIN (t JOIN w USING(a))`,
		`SELECT * FROM u JOIN (t NATURAL JOIN w)`,
		`SELECT * FROM u JOIN (w JOIN t USING(a))`,
		`SELECT * FROM u JOIN (t JOIN w USING(a)) USING(a)`,
		`SELECT * FROM u JOIN (t LEFT JOIN w USING(a))`,
		`SELECT * FROM (t JOIN w USING(a)) JOIN u`,
		`SELECT * FROM (t JOIN w USING(a))`,
		`SELECT * FROM u JOIN (t JOIN w USING(a,a))`,
		`SELECT * FROM u JOIN (t JOIN w USING(a)) WHERE u.a=2`,
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
