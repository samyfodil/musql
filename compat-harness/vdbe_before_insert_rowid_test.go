// Tests the rowid that a BEFORE INSERT trigger sees. Auto rowids are visible
// as -1 in BEFORE, while explicit values are visible. Rowid-named columns are
// always visible as their own values.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestBeforeInsertRowidParity(t *testing.T) {
	cases := []struct{ name, ddl, ins string }{
		{"plain, auto rowid", `CREATE TABLE t1(w,x,y,z)`, `INSERT INTO t1 VALUES(1,2,3,4)`},
		{"explicit rowid", `CREATE TABLE t1(w,x,y,z)`, `INSERT INTO t1(rowid,w,x,y,z) VALUES(77,1,2,3,4)`},
		{"integer primary key", `CREATE TABLE t1(w INTEGER PRIMARY KEY,x,y,z)`, `INSERT INTO t1 VALUES(55,2,3,4)`},
		{"ipk null", `CREATE TABLE t1(w INTEGER PRIMARY KEY,x,y,z)`, `INSERT INTO t1 VALUES(NULL,2,3,4)`},
		{"rowid-named columns", `CREATE TABLE t1(rowid,oid,_rowid_,x)`, `INSERT INTO t1 VALUES(100,200,300,400)`},
	}
	for _, tc := range cases {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
		for _, s := range []string{
			`CREATE TABLE log(a,b,c,d)`, tc.ddl,
			`CREATE TRIGGER r1 BEFORE INSERT ON t1 BEGIN INSERT INTO log VALUES('r1', new.rowid, new.oid, new._rowid_); END`,
			`CREATE TRIGGER r2 AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES('r2', new.rowid, new.oid, new._rowid_); END`,
			tc.ins,
		} {
			eerr := edb.Exec(s)
			_, cerr := cdb.Exec(s)
			if (eerr == nil) != (cerr == nil) {
				t.Logf("  !! %s\n     eng=%v\n     cgo=%v", s, eerr, cerr)
			}
		}
		p, _ := edb.SnapshotPager()
		_, ev, _ := p.QueryArgs(`SELECT * FROM log`, nil)
		_, cr, _ := cgoSelect(t, cdb, `SELECT * FROM log`, nil)
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch([]string{"a", "b", "c", "d"}, eRows, []string{"a", "b", "c", "d"}, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", tc.name, reason, eRows, cr)
		}
		edb.Close()
		cdb.Close()
	}
}
