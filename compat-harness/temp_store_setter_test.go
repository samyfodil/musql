package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Tests PRAGMA temp_store setter through the driver. The setter routes to
// the write path to allow discarding the temp database. Tests various temp_store
// modes and verifies that temp tables and ordinary operations work correctly
// in all modes.
func TestTempStoreSetterThroughDriver(t *testing.T) {
	for si, seq := range [][]string{
		{`PRAGMA temp_store`},
		{`PRAGMA temp_store=MEMORY`, `PRAGMA temp_store`},
		{`PRAGMA temp_store=2`, `PRAGMA temp_store`},
		{`PRAGMA temp_store=FILE`, `PRAGMA temp_store`},
		{`PRAGMA temp_store=1`, `PRAGMA temp_store`},
		{`PRAGMA temp_store=0`, `PRAGMA temp_store`},
		{`PRAGMA temp_store=DEFAULT`, `PRAGMA temp_store`},
		{`PRAGMA temp_store=junk`, `PRAGMA temp_store`},
		{`CREATE TEMP TABLE tt(x)`, `PRAGMA temp_store=MEMORY`, `PRAGMA temp_store`},
		{`PRAGMA temp_store=MEMORY`, `CREATE TEMP TABLE tt(x)`, `INSERT INTO tt VALUES(1)`, `SELECT * FROM tt`},
		{`PRAGMA temp_store=MEMORY`, `CREATE TABLE t2(a)`, `INSERT INTO t2 VALUES(1)`, `SELECT count(*) FROM t2`},
		{`PRAGMA temp_store=MEMORY`, `SELECT 1 ORDER BY 1`},
		{`PRAGMA temp_store=MEMORY`, `CREATE TEMP TABLE tt(x)`, `PRAGMA database_list`},
		{`PRAGMA temp_store=FILE`, `CREATE TEMP TABLE tt(x)`, `PRAGMA database_list`},
	} {
		si, seq := si, seq
		t.Run(fmt.Sprintf("%02d", si), func(t *testing.T) {
			var out [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, q := range seq {
					// database_list's "file" column is the database's own path,
					// which differs by construction between two connections on
					// their own copies -- compare only the temp row's shape.
					got := renderQuery(db, q)
					if q == `PRAGMA database_list` {
						if k := strings.Index(got, "[1 temp"); k >= 0 {
							got = got[k:]
						}
					}
					out[i] += " | " + got
				}
				db.Close()
			}
			if out[0] != out[1] {
				t.Errorf("%v\n  cgo:%s\n  mus:%s", seq, out[0], out[1])
			}
		})
	}
}
