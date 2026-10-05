// This file is the differential gate for two compound-SELECT behaviors that
// were each observable as a wrong ANSWER: a deduplicating compound (UNION,
// INTERSECT, EXCEPT) emits its rows in whole-row order, because SQLite dedups
// through an ephemeral b-tree keyed on the row -- while UNION ALL, a plain
// concatenation, stays in arm order -- and a recursive CTE joined with plain
// UNION dedups its INITIAL arm's rows too (with1.test 26.2).
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestCompoundRowOrderParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE u(a)`,
		`INSERT INTO u VALUES(3),(1),(2)`,
		`CREATE TABLE t(label TEXT, step INTEGER)`,
		`INSERT INTO t VALUES('a',1),('a',1),('b',1)`,
	}
	probes := []string{
		`SELECT 5 UNION SELECT 0 UNION SELECT 3`,
		`SELECT a FROM u UNION SELECT 9`,
		`SELECT a FROM u UNION ALL SELECT 9`,
		`SELECT a FROM u INTERSECT SELECT a FROM u`,
		`SELECT a FROM u EXCEPT SELECT 2`,
		`SELECT 'b' UNION SELECT 'a'`,
		`SELECT 2,1 UNION SELECT 1,9`,
		// The order is observable through a bare column under an aggregate.
		`WITH c(i) AS (VALUES(5) UNION SELECT 0) SELECT min(1)-i FROM c`,
		// A recursive UNION dedups the seed rows; UNION ALL keeps them.
		`WITH RECURSIVE cte(label, step) AS (SELECT * FROM t UNION SELECT label, step+1 FROM cte WHERE step<3) SELECT * FROM cte ORDER BY +label, +step`,
		`WITH RECURSIVE cte(label, step) AS (SELECT DISTINCT * FROM t UNION ALL SELECT label, step+1 FROM cte WHERE step<3) SELECT * FROM cte ORDER BY +label, +step`,
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
