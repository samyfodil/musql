package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestAggregateOverJoinGroup tests whole-table aggregates over materialized
// join groups against C SQLite.
func TestAggregateOverJoinGroup(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE u(a,c)`, `INSERT INTO u VALUES(1,'p'),(2,'q')`,
		`CREATE TABLE w(a,d)`, `INSERT INTO w VALUES(1,'m'),(2,'n')`,
	}
	declines := map[string]bool{
		`SELECT group_concat(w.d) FROM u JOIN (t JOIN w USING(a))`: true,
	}
	for qi, q := range []string{
		`SELECT count(*) FROM u JOIN (t JOIN w USING(a))`,
		`SELECT count(*) FROM u JOIN (t JOIN w ON t.a=w.a)`,
		`SELECT sum(t.a) FROM u JOIN (t JOIN w USING(a))`,
		`SELECT sum(u.a), sum(t.a), sum(w.a) FROM u JOIN (t JOIN w USING(a))`,
		`SELECT max(w.d) FROM u JOIN (t JOIN w USING(a))`,
		`SELECT u.a, count(*) FROM u JOIN (t JOIN w USING(a)) GROUP BY u.a ORDER BY u.a`,
		`SELECT t.a, count(*) FROM u JOIN (t JOIN w USING(a)) GROUP BY t.a ORDER BY t.a`,
		`SELECT group_concat(w.d) FROM u JOIN (t JOIN w USING(a))`,
		`SELECT count(*) FROM u, (t JOIN w USING(a))`,
		`SELECT count(*) FROM (t JOIN w USING(a)) JOIN u`,
		`SELECT count(*) FROM u JOIN (t LEFT JOIN w USING(a))`,
		`SELECT t.b, w.d FROM u JOIN (t JOIN w USING(a)) ORDER BY t.b`,
		// GROUP BY over the group, in every spelling the offset rebase has to
		// carry: each member's own key, a key from the source PRECEDING the
		// group (the one that was wrong), several aggregates at once, HAVING,
		// two keys, a leading group, a LEFT join inside it, a comma join, an
		// outward ON, a nested group, a window beside the aggregate, a
		// DISTINCT aggregate, an expression key, and a WHERE.
		`SELECT u.c, count(*) FROM u JOIN (t JOIN w USING(a)) GROUP BY u.c ORDER BY u.c`,
		`SELECT w.d, count(*) FROM u JOIN (t JOIN w USING(a)) GROUP BY w.d ORDER BY w.d`,
		`SELECT u.c, sum(t.a), max(w.d), min(t.b) FROM u JOIN (t JOIN w USING(a)) GROUP BY u.c ORDER BY u.c`,
		`SELECT u.c, count(*) FROM u JOIN (t JOIN w USING(a)) GROUP BY u.c HAVING count(*)>1 ORDER BY u.c`,
		`SELECT u.c, t.b FROM u JOIN (t JOIN w USING(a)) GROUP BY u.c, t.b ORDER BY u.c, t.b`,
		`SELECT t.b, count(*) FROM (t JOIN w USING(a)) JOIN u GROUP BY t.b ORDER BY t.b`,
		`SELECT u.c, count(*) FROM u JOIN (t LEFT JOIN w USING(a)) GROUP BY u.c ORDER BY u.c`,
		`SELECT u.c, count(*) FROM u, (t JOIN w USING(a)) GROUP BY u.c ORDER BY u.c`,
		`SELECT u.c, count(DISTINCT t.b) FROM u JOIN (t JOIN w USING(a)) GROUP BY u.c ORDER BY u.c`,
		`SELECT u.c, count(*), row_number() OVER (ORDER BY u.c) FROM u JOIN (t JOIN w USING(a)) GROUP BY u.c ORDER BY u.c`,
		`SELECT sum(u.a), count(*), t.b FROM u JOIN (t JOIN w USING(a)) GROUP BY t.b ORDER BY t.b`,
		`SELECT u.c, max(t.b) FROM u JOIN (t JOIN w USING(a)) GROUP BY u.c ORDER BY max(t.b)`,
		`SELECT u.a+t.a, count(*) FROM u JOIN (t JOIN w USING(a)) GROUP BY u.a+t.a ORDER BY 1`,
		`SELECT u.c, count(*) FROM u JOIN (t JOIN w USING(a)) WHERE u.a=t.a GROUP BY u.c ORDER BY u.c`,
		`SELECT u.c FROM u JOIN (t JOIN w USING(a)) GROUP BY u.c HAVING max(t.b)='y' ORDER BY u.c`,
		`SELECT DISTINCT u.c, t.b FROM u JOIN (t JOIN w USING(a)) ORDER BY u.c, t.b`,
		`SELECT u.c, count(*) FROM u JOIN (t JOIN w ON t.a=w.a) GROUP BY u.c ORDER BY u.c`,
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
					t.Errorf("%s: expected a DECLINE (see this test's doc comment), got %s -- if it is right, move it out of declines", q, out[1])
				}
				return
			}
			if out[0] != out[1] {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", q, out[0], out[1])
			}
		})
	}
}
