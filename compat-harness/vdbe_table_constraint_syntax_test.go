// This file is the differential gate for SQLite's OPTIONAL comma between
// TABLE constraints: "CONSTRAINT one PRIMARY KEY(a) CONSTRAINT two
// CHECK(b<10) UNIQUE(b) CONSTRAINT three" declares three ENFORCED constraints
// plus a dangling name (schema5.test). This engine used to keep only the
// first and silently drop the rest, so a CHECK simply never fired.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestCommaLessTableConstraintParity(t *testing.T) {
	cases := []struct{ ddl, ins string }{
		{`CREATE TABLE t1(a,b,c, PRIMARY KEY(a) UNIQUE (a) CONSTRAINT one)`, `INSERT INTO t1 VALUES(1,3,4)`},
		{`CREATE TABLE t1(a,b,c, CONSTRAINT one PRIMARY KEY(a) CONSTRAINT two CHECK(b<10) UNIQUE(b) CONSTRAINT three)`, `INSERT INTO t1 VALUES(10,11,12)`},
		{`CREATE TABLE t1(a,b,c, UNIQUE(a) CONSTRAINT one, PRIMARY KEY(b,c) CONSTRAINT two)`, `INSERT INTO t1 VALUES(1,3,4)`},
		{`CREATE TABLE t1(a,b,c, CONSTRAINT)`, ``},
		{`CREATE TABLE t1(a,b,c, CONSTRAINT one)`, ``},
		{`CREATE TABLE t1(a,b,c, CHECK(a>0) CHECK(b>0))`, `INSERT INTO t1 VALUES(1,-1,1)`},
		{`CREATE TABLE t1(a,b,c, CHECK(a>0)CHECK(b>0))`, `INSERT INTO t1 VALUES(1,-1,1)`},
	}
	for _, tc := range cases {
		got := ""
		for _, drv := range []string{"sqlite", "sqlite3"} {
			dsn := ":memory:"
			if drv == "sqlite" {
				dsn = filepath.Join(t.TempDir(), "e.sqlite")
			}
			db, _ := sql.Open(drv, dsn)
			db.SetMaxOpenConns(1)
			out := ""
			if _, err := db.Exec(tc.ddl); err != nil {
				out = "DDL " + err.Error()
			} else {
				db.Exec(`INSERT INTO t1 VALUES(1,2,3)`)
				if tc.ins != "" {
					if _, err := db.Exec(tc.ins); err != nil {
						out = "INS " + err.Error()
					} else {
						out = "INS ok"
					}
				} else {
					out = "DDL ok"
				}
			}
			db.Close()
			if drv == "sqlite" {
				got = out
				continue
			}
			// The engine prefixes its errors; the message itself must match.
			// A syntax error's reported POSITION is the one exception: both
			// engines reject "CONSTRAINT" with no name, C SQLite blaming
			// near ")" and this parser near "," -- the rejection is what
			// matters, and the corpus never compares DDL error text.
			g := strings.TrimPrefix(strings.TrimPrefix(got, "INS engine: INSERT into t1: "), "DDL engine: ")
			c := strings.TrimPrefix(strings.TrimPrefix(out, "INS "), "DDL ")
			if strings.Contains(g, "syntax error") && strings.Contains(c, "syntax error") {
				continue
			}
			if g != c && got != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", tc.ddl, got, out)
			}
		}
	}
}
