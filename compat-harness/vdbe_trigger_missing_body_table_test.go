// This file is the differential gate for a trigger body naming a table that
// does not exist: SQLite compiles the whole trigger program when it PREPARES
// the triggering statement, so the failure does not depend on how many rows
// that statement matches -- an empty target still errors (fts3conf.test).
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestTriggerMissingBodyTableParity(t *testing.T) {
	got := map[string]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, _ := sql.Open(drv, dsn)
		db.SetMaxOpenConns(1)
		for _, q := range []string{
			`CREATE TABLE A(ID INTEGER PRIMARY KEY, AnotherID INTEGER)`,
			`CREATE TRIGGER A_del AFTER DELETE ON A FOR EACH ROW BEGIN DELETE FROM AFTS WHERE rowid=OLD.ID; END`,
			`CREATE TRIGGER A_ins AFTER INSERT ON A FOR EACH ROW BEGIN INSERT INTO NOSUCH VALUES(1); END`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Logf("%-8s setup %-40.40q %v", drv, q, err)
			}
		}
		for _, q := range []string{
			`DELETE FROM A WHERE AnotherID=1`,
			`INSERT INTO A VALUES(1,1)`,
			`UPDATE A SET AnotherID=2`,
		} {
			_, err := db.Exec(q)
			msg := ""
			if err != nil {
				msg = strings.TrimPrefix(err.Error(), "engine: ")
			}
			if drv == "sqlite" {
				got[q] = msg
				continue
			}
			if got[q] != msg {
				t.Errorf("%s\n  engine: %q\n  cgo:    %q", q, got[q], msg)
			}
		}
		db.Close()
	}
}
