// This file is the differential gate for a SCALAR SUBQUERY's affinity in a
// comparison: it carries the affinity of the column it selects, so a
// BLOB-affinity (i.e. no-affinity) column reached through a subquery blocks
// the TEXT coercion exactly as a direct column reference would, while a
// COMPUTED result column (y+0) does not (rowvalue9.test 3.5).
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestScalarSubqueryAffinityParity(t *testing.T) {
	counts := map[string]int{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, _ := sql.Open(drv, dsn)
		db.SetMaxOpenConns(1)
		for _, q := range []string{
			`CREATE TABLE c1(a INTEGER, b TEXT)`,
			`INSERT INTO c1 VALUES(1, 1)`,
			`CREATE TABLE c2(x BLOB, y BLOB)`,
			`INSERT INTO c2 VALUES(1, 1)`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", drv, err)
			}
		}
		for _, q := range []string{
			`SELECT c1.rowid FROM c1 WHERE b = (SELECT y FROM c2)`,
			`SELECT c1.rowid FROM c1 WHERE b = (SELECT y+0 FROM c2)`,
			`SELECT c1.rowid FROM c1, c2 WHERE c1.b = c2.y`,
			`SELECT c1.rowid FROM c1 WHERE b = 1`,
		} {
			rows, err := db.Query(q)
			n := 0
			if err == nil {
				for rows.Next() {
					n++
				}
				rows.Close()
			}
			if err != nil {
				t.Fatalf("%s: %s: %v", drv, q, err)
			}
			if drv == "sqlite" {
				counts[q] = n
				continue
			}
			if counts[q] != n {
				t.Errorf("%s\n  engine: %d row(s)\n  cgo:    %d row(s)", q, counts[q], n)
			}
		}
		db.Close()
	}
}
