// Tests materialized expression and partial indexes, checking catalog parity
// and interchange with C SQLite
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// exprIndexSetup has expression and partial indexes on a test table
var exprIndexSetup = []string{
	`CREATE TABLE t(a INTEGER, b INTEGER, c TEXT)`,
	`INSERT INTO t VALUES(1,1,'v1'),(2,4,'V2'),(3,9,'v3'),(4,16,'V4'),(5,25,'v5')`,
	`INSERT INTO t VALUES(NULL,NULL,NULL)`,
	`CREATE INDEX ie ON t(a+b)`,
	`CREATE INDEX ip ON t(c) WHERE a > 3`,
	`CREATE INDEX im ON t(b, a*2) WHERE c IS NOT NULL`,
	`CREATE INDEX ic ON t(upper(c) COLLATE NOCASE)`,
}

// TestExprIndexCatalogAndQueryParity tests index catalog and query parity
func TestExprIndexCatalogAndQueryParity(t *testing.T) {
	probes := []string{
		`SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY name`,
		`PRAGMA index_list(t)`,
		`PRAGMA index_info(ie)`,
		`PRAGMA index_info(ip)`,
		`PRAGMA index_info(im)`,
		`SELECT a FROM t WHERE a+b = 20`,
		`SELECT a FROM t WHERE a > 3 AND c = 'v5'`,
		`SELECT a FROM t WHERE b = 25 AND a*2 = 10`,
		`SELECT a FROM t WHERE upper(c) = 'V3'`,
		`SELECT count(*) FROM t WHERE a+b IS NULL`,
	}
	renamed := []string{
		`ALTER TABLE t RENAME TO t1`,
		`SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY name`,
		`SELECT a FROM t1 WHERE a+b = 20`,
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
		for _, q := range exprIndexSetup {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", drv, q, err)
			}
		}
		// Run probes before and after rename
		for i, q := range append(append([]string(nil), probes...), renamed...) {
			out := ""
			if q == renamed[0] {
				_, err := db.Exec(q)
				out = fmt.Sprintf("err=%v", err != nil)
			} else {
				out = queryString(t, db, q)
			}
			key := fmt.Sprintf("#%d %s", i, q)
			if drv == "sqlite" {
				got[key] = out
				continue
			}
			if got[key] != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", key, got[key], out)
			}
		}
		db.Close()
	}
}

// TestExprIndexInterchange is the file-format half: C SQLite must accept
// the b-tree the pure engine wrote for an expression/partial index
// (integrity_check recomputes every key from the row) and actually use it.
// Page size 512 forces the index b-tree to split, so the interior-node
// separators are exercised too, not just a single leaf.
func TestExprIndexInterchange(t *testing.T) {
	for _, tc := range []struct{ name, ddl, query, idx string }{
		{"expr", `CREATE INDEX ie ON t(a+b)`, `SELECT c FROM t WHERE a+b = 40`, "ie"},
		{"func", `CREATE INDEX if1 ON t(upper(c))`, `SELECT a FROM t WHERE upper(c) = 'V57'`, "if1"},
		{"partial", `CREATE INDEX ip ON t(c) WHERE a > 3`, `SELECT a FROM t WHERE a > 3 AND c = 'v57'`, "ip"},
		{"collate", `CREATE INDEX ic ON t(upper(c) COLLATE NOCASE)`, `SELECT a FROM t WHERE upper(c) COLLATE NOCASE = 'v57'`, "ic"},
		{"mixed", `CREATE INDEX im ON t(b, a*2)`, `SELECT a FROM t WHERE b = 11 AND a*2 = 22`, "im"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "expr_idx.sqlite")
			edb, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			wvExec(t, edb, "CREATE TABLE t(a INTEGER, b INTEGER, c TEXT)")
			for i := 1; i <= 200; i++ {
				wvExec(t, edb, fmt.Sprintf("INSERT INTO t VALUES(%d,%d,%s)", i, i%23, wvString(fmt.Sprintf("v%d", i))))
			}
			wvExec(t, edb, "INSERT INTO t VALUES(NULL,NULL,NULL)")
			wvExec(t, edb, tc.ddl)
			if err := edb.Close(); err != nil {
				t.Fatalf("engine writer Close: %v", err)
			}

			db := openCgo(t, path)
			requireIntegrityOK(t, db)
			requireUsesIndex(t, db, tc.query, tc.idx)

			// The same file, reopened by the pure WRITE path (which re-derives
			// the index from its stored CREATE INDEX text), mutated, and closed
			// again, must still satisfy C SQLite -- the recovery path is
			// what a second session depends on.
			edb2, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("engine.OpenWrite: %v", err)
			}
			wvExec(t, edb2, "INSERT INTO t VALUES(999,7,'v999')")
			wvExec(t, edb2, "DELETE FROM t WHERE a = 1")
			if err := edb2.Close(); err != nil {
				t.Fatalf("engine writer reopen Close: %v", err)
			}
			db2 := openCgo(t, path)
			requireIntegrityOK(t, db2)
			requireUsesIndex(t, db2, tc.query, tc.idx)
		})
	}
}
