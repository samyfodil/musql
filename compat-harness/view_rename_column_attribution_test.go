package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestViewRenameColumnAttribution tests ALTER TABLE RENAME COLUMN where views
// reference the column and disambiguation is needed.
func TestViewRenameColumnAttribution(t *testing.T) {
	declines := map[int]bool{9: true}
	for ci, c := range [][]string{
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE VIEW v AS SELECT a FROM t`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE VIEW v AS SELECT a FROM o`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE VIEW a AS SELECT 1 AS z`, `CREATE VIEW v AS SELECT a FROM t`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT a, b, a+1 FROM t WHERE a>0 ORDER BY a`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT t.a FROM t`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE VIEW v AS SELECT t.a FROM t JOIN o ON t.a=o.a`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(z)`, `CREATE VIEW v AS SELECT a FROM t JOIN o ON t.b=o.z`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE VIEW v AS SELECT o.a FROM t JOIN o ON t.b=o.a`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE VIEW v AS SELECT x.a FROM t AS x JOIN o ON x.b=o.a`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE VIEW v AS SELECT a FROM t JOIN o ON t.b=o.a`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT count(a) FROM t GROUP BY a`, `ALTER TABLE t RENAME COLUMN a TO q`},
	} {
		ci, c := ci, c
		t.Run(fmt.Sprintf("%02d", ci), func(t *testing.T) {
			var out [2]string
			var failed [2]bool
			for i, drv := range []string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(drv, p)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range c {
					if _, e := db.Exec(s); e != nil {
						failed[i] = true
					}
				}
				out[i] = renderQuery(db, `SELECT sql FROM sqlite_schema WHERE type='view'`)
				db.Close()
			}
			if declines[ci] {
				if !failed[1] {
					t.Errorf("%v: expected a DECLINE (more than one FROM table needs real resolution), got %s", c, out[1])
				}
				return
			}
			if failed[1] {
				t.Errorf("%v: declined, but the oracle rewrote to %s", c, out[0])
			}
			if out[0] != out[1] {
				t.Errorf("%v\n  cgo: %s\n  mus: %s", c, out[0], out[1])
			}
		})
	}
}
