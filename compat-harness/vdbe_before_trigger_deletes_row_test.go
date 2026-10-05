// BEFORE trigger that deletes the row should prevent the update and AFTER trigger.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

var beforeTriggerDeleteCases = []struct {
	name  string
	stmts []string
}{
	{"before-update deletes the row", []string{
		`CREATE TABLE t1(a,b)`, `CREATE TABLE log(x)`, `INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t1 BEGIN DELETE FROM t1; END`,
		`CREATE TRIGGER ta AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('fired!'); END`,
		`UPDATE t1 SET b=3`}},
	{"before-update deletes another row", []string{
		`CREATE TABLE t1(a,b)`, `CREATE TABLE log(x)`, `INSERT INTO t1 VALUES(1,2),(9,9)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t1 BEGIN DELETE FROM t1 WHERE a=9; END`,
		`CREATE TRIGGER ta AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('fired!'); END`,
		`UPDATE t1 SET b=3 WHERE a=1`}},
	{"before-delete deletes the row", []string{
		`CREATE TABLE t1(a,b)`, `CREATE TABLE log(x)`, `INSERT INTO t1 VALUES(1,2)`,
		`CREATE TRIGGER tb BEFORE DELETE ON t1 BEGIN DELETE FROM t1 WHERE a=1; END`,
		`CREATE TRIGGER ta AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES('fired!'); END`,
		`DELETE FROM t1 WHERE a=1`}},
	// BEFORE trigger removes one of several matched rows.
	{"before-update deletes one of several", []string{
		`CREATE TABLE t1(a,b)`, `CREATE TABLE log(x)`, `INSERT INTO t1 VALUES(1,1),(2,2),(3,3)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t1 BEGIN DELETE FROM t1 WHERE a=2; END`,
		`CREATE TRIGGER ta AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('fired!'); END`,
		`UPDATE t1 SET b=b+10`}},
}

func TestBeforeTriggerDeletesRowParity(t *testing.T) {
	for _, tc := range beforeTriggerDeleteCases {
		t.Run(tc.name, func(t *testing.T) {
			edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer edb.Close()
			cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()

			for _, s := range tc.stmts {
				eErr := edb.Exec(s)
				_, cErr := cdb.Exec(s)
				if (eErr == nil) != (cErr == nil) {
					t.Fatalf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eErr, cErr)
				}
			}

			p, err := edb.SnapshotPager()
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{`SELECT * FROM t1 ORDER BY a, b`, `SELECT * FROM log`} {
				eCols, eVals, qErr := p.QueryArgs(q, nil)
				if qErr != nil {
					t.Fatal(q, qErr)
				}
				cCols, cRows, sErr := cgoSelect(t, cdb, q, nil)
				if sErr != nil {
					t.Fatal(q, sErr)
				}
				eRows := engineRowsToStrings(eVals)
				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
					t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cRows)
				}
			}
		})
	}
}
