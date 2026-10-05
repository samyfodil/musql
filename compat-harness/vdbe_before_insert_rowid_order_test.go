// This file is the differential gate for WHEN an auto rowid is allocated: not
// until the BEFORE INSERT program has run. NEW.rowid therefore reads -1 inside
// a BEFORE trigger, and a BEFORE trigger that inserts into the SAME table gets
// the lower rowid (autoinc.test 3928.1).
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestBeforeInsertRowidOrderParity(t *testing.T) {
	got := map[string]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, _ := sql.Open(drv, dsn)
		db.SetMaxOpenConns(1)
		for _, q := range []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`CREATE TABLE log(what, v)`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('new.a', new.a); INSERT INTO log VALUES('new.rowid', new.rowid); END`,
			`INSERT INTO t(b) VALUES('x')`,
			`INSERT INTO t(b) VALUES('y')`,
			// The autoinc.test 3928 shape: BEFORE and AFTER triggers that both
			// insert into their own table, recursively.
			`CREATE TABLE t2(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
			`CREATE TRIGGER t2r1 BEFORE INSERT ON t2 BEGIN
			   INSERT INTO t2(b) VALUES('before1');
			   INSERT INTO t2(b) VALUES('before2');
			 END`,
			`CREATE TRIGGER t2r2 AFTER INSERT ON t2 BEGIN
			   INSERT INTO t2(b) VALUES('after1');
			   INSERT INTO t2(b) VALUES('after2');
			 END`,
			`INSERT INTO t2(b) VALUES('test')`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Logf("%-8s %-30.30q %v", drv, q, err)
			}
		}
		for _, probe := range []string{
			`SELECT what, v FROM log`,
			`SELECT a, b FROM t ORDER BY a`,
			`SELECT a, b FROM t2 ORDER BY a`,
			`SELECT seq FROM sqlite_sequence WHERE name='t2'`,
		} {
			out := queryString(t, db, probe)
			if drv == "sqlite" {
				got[probe] = out
				continue
			}
			if got[probe] != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", probe, got[probe], out)
			}
		}
		db.Close()
	}
}
