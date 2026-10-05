// This file is the differential gate for a trigger body naming another
// database: "trigger r5 cannot reference objects in database temp" applies
// whether the qualifier sits in a plain FROM or is reached only through an
// expression subquery in the select list, WHERE, or HAVING (attach.test
// 5.4-5.7). Only the FROM form used to be caught.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestTriggerForeignSchemaParity(t *testing.T) {
	bodies := []string{
		`SELECT 'no-op' FROM temp.t6`,
		`SELECT 'no-op' || (SELECT * FROM temp.t6)`,
		`SELECT 'no-op' FROM t5 WHERE x<(SELECT min(p) FROM temp.t6)`,
		`SELECT 'no-op' FROM t5 GROUP BY 1 HAVING x<(SELECT min(p) FROM temp.t6)`,
		`SELECT max(1,x,(SELECT min(p) FROM temp.t6)) FROM t5`,
		`INSERT INTO t5 VALUES((SELECT min(p) FROM temp.t6),5)`,
		`DELETE FROM t5 WHERE x<(SELECT min(p) FROM temp.t6)`,
		`UPDATE t5 SET y=(SELECT min(p) FROM temp.t6)`,
		`UPDATE t5 SET y=1 WHERE x<(SELECT min(p) FROM temp.t6)`,
		`SELECT 'no-op'`,
		`INSERT INTO t5 VALUES(1,2)`,
	}
	for _, body := range bodies {
		got := ""
		for _, drv := range []string{"sqlite", "sqlite3"} {
			dsn := ":memory:"
			if drv == "sqlite" {
				dsn = filepath.Join(t.TempDir(), "e.sqlite")
			}
			db, _ := sql.Open(drv, dsn)
			db.SetMaxOpenConns(1)
			db.Exec(`CREATE TABLE t5(x,y)`)
			db.Exec(`CREATE TEMP TABLE t6(p,q,r)`)
			_, err := db.Exec(`CREATE TRIGGER r5 AFTER INSERT ON t5 BEGIN ` + body + `; END`)
			db.Close()
			msg := ""
			if err != nil {
				msg = strings.TrimPrefix(err.Error(), "engine: ")
			}
			if drv == "sqlite" {
				got = msg
				continue
			}
			if got != msg {
				t.Errorf("%s\n  engine: %q\n  cgo:    %q", body, got, msg)
			}
		}
	}
}
