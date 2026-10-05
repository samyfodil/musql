package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestNestedGroupDupColName verifies ":N" disambiguation of duplicate column
// names from non-leading parenthesized join groups with USING/NATURAL joins.
func TestNestedGroupDupColName(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x')`,
		`CREATE TABLE u(a,c)`, `INSERT INTO u VALUES(1,'p')`,
		`CREATE TABLE w(a,d)`, `INSERT INTO w VALUES(1,'m')`,
		`CREATE TABLE y(a,e)`, `INSERT INTO y VALUES(1,'n')`,
		`CREATE TABLE k(a INTEGER PRIMARY KEY, b)`, `INSERT INTO k VALUES(1,'K')`,
	}
	for qi, q := range []string{
		`SELECT * FROM u JOIN (t JOIN w ON t.a=w.a)`,
		`SELECT * FROM u JOIN (t JOIN w ON t.a=w.a) JOIN (u AS u2 JOIN y ON u2.a=y.a)`,
		`SELECT * FROM u JOIN (t JOIN w ON t.a=w.a), y`,
		`SELECT * FROM u JOIN (t JOIN (w JOIN y ON w.a=y.a) ON t.a=w.a)`,
		`SELECT * FROM (t JOIN w ON t.a=w.a) JOIN u`,
		// A hand-written reference into the rebuilt group, in every shape the
		// name list has to be built for.
		`SELECT t.a FROM u JOIN (t JOIN w USING(a))`,
		`SELECT w.a FROM u JOIN (t JOIN w USING(a))`,
		`SELECT t.a, t.b, w.d FROM u JOIN (t JOIN w USING(a))`,
		`SELECT t.a, w.a, t.b, w.d FROM u JOIN (t JOIN w ON t.a=w.a)`,
		`SELECT t.b FROM u JOIN (t JOIN w USING(a))`,
		`SELECT t.a FROM u JOIN (t NATURAL JOIN w)`,
		`SELECT t.a, w.a, y.a FROM u JOIN (t JOIN w JOIN y ON t.a=w.a AND w.a=y.a)`,
		`SELECT t.a FROM u JOIN (t JOIN (w JOIN y USING(a)) USING(a))`,
		`SELECT t.a FROM u JOIN (t JOIN w USING(a)) AS g`,
		`SELECT g.a FROM u JOIN (t JOIN w USING(a)) AS g`,
		`SELECT k.a, k.b FROM u JOIN (k JOIN w ON k.a=w.a)`,
		`SELECT k.rowid FROM u JOIN (k JOIN w ON k.a=w.a)`,
		`SELECT t.a FROM u JOIN u AS u3 JOIN (t JOIN w USING(a))`,
		`SELECT t.a FROM u LEFT JOIN (t JOIN w USING(a))`,
		`SELECT t.a AS q FROM u JOIN (t JOIN w USING(a))`,
		`SELECT t.a FROM u JOIN (t JOIN w USING(a)) ORDER BY t.a`,
		`SELECT t.a, count(*) FROM u JOIN (t JOIN w USING(a)) GROUP BY t.a`,
		// ...and a QUALIFIED star, which names from the same list.
		`SELECT t.* FROM u JOIN (t JOIN w USING(a))`,
		`SELECT w.* FROM u JOIN (t JOIN w USING(a))`,
		`SELECT t.* FROM u JOIN (t JOIN w ON t.a=w.a)`,
		`SELECT t.* FROM (t JOIN w USING(a)) JOIN u`,
		// A LEADING unaliased group is never rebuilt: declared names.
		`SELECT t.a FROM (t JOIN w USING(a)) JOIN u`,
		`SELECT t.a FROM (t JOIN w ON t.a=w.a) JOIN u`,
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
			if out[0] != out[1] {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", q, out[0], out[1])
			}
		})
	}
}
