// This file is the differential gate for UPDATE ... FROM over a JOIN: the
// target table is joined with the FROM clause's RESULT, so an outer join
// inside FROM never null-extends the target, and the FROM's own ON clauses
// cannot reference the target at all (upfrom4.test).
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestUpdateFromJoinParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t5(a INTEGER PRIMARY KEY, b TEXT, c TEXT)`,
		`CREATE TABLE m1(x INTEGER PRIMARY KEY, y TEXT)`,
		`CREATE TABLE m2(u INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t5 VALUES(1,'one','ONE'),(2,'two','TWO'),(3,'three','THREE'),(4,'four','FOUR')`,
		`INSERT INTO m1 VALUES(1,'i'),(2,'ii'),(3,'iii')`,
		`INSERT INTO m2 VALUES(1,'I'),(3,'II'),(4,'III')`,
	}
	for _, upd := range []string{
		`UPDATE t5 SET b=y, c=v FROM m1 LEFT JOIN m2 ON (x=u) WHERE x=a`,
		`UPDATE t5 SET b=y, c=v FROM m2 RIGHT JOIN m1 ON (x=u) WHERE x=a`,
		`UPDATE t5 SET b=y, c=v FROM m1 LEFT JOIN m2 ON (u=t5.a) WHERE x=a`,
		`UPDATE t5 SET b=y, c=v FROM m1 FULL JOIN m2 ON (x=u) WHERE x=a`,
		`UPDATE t5 SET b=y FROM m1 WHERE x=a`,
	} {
		got := ""
		for _, drv := range []string{"sqlite", "sqlite3"} {
			dsn := ":memory:"
			if drv == "sqlite" {
				dsn = filepath.Join(t.TempDir(), "e.sqlite")
			}
			db, _ := sql.Open(drv, dsn)
			db.SetMaxOpenConns(1)
			for _, q := range setup {
				db.Exec(q)
			}
			out := ""
			if _, err := db.Exec(upd); err != nil {
				out = "ERR " + err.Error()
			} else {
				out = queryString(t, db, `SELECT * FROM t5 ORDER BY a`)
			}
			db.Close()
			if drv == "sqlite" {
				got = out
				continue
			}
			// Both engines must agree on the resulting ROWS; where both
			// REJECT, the wording is allowed to differ (this engine reports
			// the unresolvable target as a missing table, C SQLite as a
			// missing column) -- the rejection is the behavior under test.
			if strings.HasPrefix(got, "ERR") && strings.HasPrefix(out, "ERR") {
				continue
			}
			if got != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", upd, got, out)
			}
		}
	}
}
