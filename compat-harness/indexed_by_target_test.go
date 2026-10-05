package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestIndexedByTarget verifies INDEXED BY works with all FROM items.
//
// Ephemeral items (CTEs, derived tables, table-valued functions) have no index
// chain and always error. "NOT INDEXED" is a no-op.
func TestIndexedByTarget(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`, `CREATE INDEX ti ON t(a)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE VIEW v AS SELECT a,b FROM t`,
		`CREATE VIRTUAL TABLE f USING fts4(a)`,
	}
	for qi, q := range []string{
		`WITH q AS (SELECT * FROM t) SELECT * FROM q INDEXED BY ti`,
		`WITH q AS (SELECT * FROM t) SELECT * FROM q INDEXED BY nosuch`,
		`WITH q AS (SELECT * FROM t) SELECT * FROM q NOT INDEXED`,
		`SELECT * FROM (SELECT * FROM t) INDEXED BY ti`,
		`SELECT * FROM v INDEXED BY ti`,
		`SELECT * FROM v NOT INDEXED`,
		`SELECT * FROM f INDEXED BY ti`,
		`SELECT * FROM t INDEXED BY nosuch`,
		`SELECT * FROM t INDEXED BY ti`,
		`SELECT * FROM sqlite_schema INDEXED BY ti`,
		`SELECT * FROM pragma_table_info('t') INDEXED BY ti`,
		`DELETE FROM t INDEXED BY ti WHERE a=1`,
		`UPDATE t INDEXED BY ti SET b='z' WHERE a=1`,
		`WITH q AS (SELECT * FROM t) DELETE FROM t WHERE a IN (SELECT a FROM q INDEXED BY ti)`,
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
					db.Exec(s)
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
