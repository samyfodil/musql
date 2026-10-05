// This file tests column affinity in CHECK constraints.
// Comparisons inside CHECK must respect the column's declared affinity.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestCheckConstraintAffinityParity(t *testing.T) {
	for _, tc := range []struct{ name, ddl, ins string }{
		{"TEXT between +a", `CREATE TABLE t1(a TEXT, CHECK(a BETWEEN +a AND 999999))`, `INSERT INTO t1(a) VALUES(NULL),(5)`},
		{"TEXT not between", `CREATE TABLE t1(a TEXT, CHECK(a NOT BETWEEN +a AND 999999))`, `INSERT INTO t1(a) VALUES(NULL)`},
		{"TEXT cmp", `CREATE TABLE t1(a TEXT, CHECK(a <= 999999))`, `INSERT INTO t1(a) VALUES(5)`},
		{"TEXT ge", `CREATE TABLE t1(a TEXT, CHECK(a >= +a))`, `INSERT INTO t1(a) VALUES(5)`},
		{"between +a", `CREATE TABLE t1(a, CHECK(a BETWEEN +a AND 999999))`, `INSERT INTO t1(a) VALUES(NULL),(5)`},
		{"between plain", `CREATE TABLE t1(a, CHECK(a BETWEEN 0 AND 999999))`, `INSERT INTO t1(a) VALUES(NULL),(5)`},
		{"simple gt", `CREATE TABLE t1(a, CHECK(a>0))`, `INSERT INTO t1(a) VALUES(NULL),(5)`},
		{"unary plus", `CREATE TABLE t1(a, CHECK(+a>0))`, `INSERT INTO t1(a) VALUES(NULL),(5)`},
		{"is not null", `CREATE TABLE t1(a, CHECK(a IS NOT NULL))`, `INSERT INTO t1(a) VALUES(NULL),(5)`},
		{"false check", `CREATE TABLE t1(a, CHECK(a>100))`, `INSERT INTO t1(a) VALUES(5)`},
	} {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
		edb.Exec(tc.ddl)
		cdb.Exec(tc.ddl)
		eerr := edb.Exec(tc.ins)
		_, cerr := cdb.Exec(tc.ins)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] %s accept/reject disagrees\n  engine=%v\n  cgo=%v", tc.name, tc.ddl, eerr, cerr)
		}
		edb.Close()
		cdb.Close()
	}
}
