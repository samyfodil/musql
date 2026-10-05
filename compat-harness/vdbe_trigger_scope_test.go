// Trigger body DML target qualification and rowid pseudo-column shadowing.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

var triggerQualifiedBodyCases = []string{
	`CREATE TRIGGER e1 AFTER INSERT ON tA BEGIN INSERT INTO main.tB VALUES(1,2); END`,
	`CREATE TRIGGER e2 AFTER INSERT ON tA BEGIN UPDATE main.tB SET x=1; END`,
	`CREATE TRIGGER e3 AFTER INSERT ON tA BEGIN DELETE FROM main.tB; END`,
	`CREATE TRIGGER e5 AFTER INSERT ON tA BEGIN INSERT INTO temp.tB VALUES(1,2); END`,
	`CREATE TRIGGER e4 AFTER INSERT ON tA BEGIN SELECT * FROM main.tB; END`,
	`CREATE TRIGGER ok1 AFTER INSERT ON tA BEGIN INSERT INTO tB VALUES(1,2); END`,
	`CREATE TRIGGER main.ok2 AFTER INSERT ON tA BEGIN INSERT INTO tB VALUES(3,4); END`,
}

func TestTriggerQualifiedBodyTargetParity(t *testing.T) {
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
	for _, s := range []string{`CREATE TABLE tA(a,b)`, `CREATE TABLE tB(x,y)`} {
		if err := edb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	for _, s := range triggerQualifiedBodyCases {
		eErr := edb.Exec(s)
		_, cErr := cdb.Exec(s)
		if (eErr == nil) != (cErr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eErr, cErr)
			continue
		}
		if eErr != nil {
			if got := strings.TrimPrefix(eErr.Error(), "engine: "); got != cErr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", s, got, cErr.Error())
			}
		}
	}
}

// rowidShadowTriggerStmts is triggerD.test 1.1's shape: a table whose
// ordinary columns are literally named rowid, oid and _rowid_, with triggers
// logging NEW./OLD. of each across INSERT, UPDATE and DELETE.
var rowidShadowTriggerStmts = []string{
	`CREATE TABLE t1(rowid, oid, _rowid_, x)`,
	`CREATE TABLE log(a,b,c,d,e)`,
	`CREATE TRIGGER r1 BEFORE INSERT ON t1 BEGIN INSERT INTO log VALUES('r1', new.rowid, new.oid, new._rowid_, new.x); END`,
	`CREATE TRIGGER r2 AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES('r2', new.rowid, new.oid, new._rowid_, new.x); END`,
	`CREATE TRIGGER r3 BEFORE UPDATE ON t1 BEGIN INSERT INTO log VALUES('r3.old', old.rowid, old.oid, old._rowid_, old.x); END`,
	`CREATE TRIGGER r4 AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('r4.new', new.rowid, new.oid, new._rowid_, new.x); END`,
	`CREATE TRIGGER r5 BEFORE DELETE ON t1 BEGIN INSERT INTO log VALUES('r5', old.rowid, old.oid, old._rowid_, old.x); END`,
	`INSERT INTO t1 VALUES(100,200,300,400)`,
	`UPDATE t1 SET x=401`,
	`DELETE FROM t1`,
}

func TestTriggerRowidShadowedByColumnParity(t *testing.T) {
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
	for _, s := range rowidShadowTriggerStmts {
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
	const q = `SELECT * FROM log`
	eCols, eVals, qErr := p.QueryArgs(q, nil)
	if qErr != nil {
		t.Fatal(qErr)
	}
	cCols, cRows, sErr := cgoSelect(t, cdb, q, nil)
	if sErr != nil {
		t.Fatal(sErr)
	}
	eRows := engineRowsToStrings(eVals)
	if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
		t.Errorf("trigger log DIVERGES: %s\n  engine: %v\n  cgo:    %v", reason, eRows, cRows)
	}
}

// TestTempTriggerQualifiedNameRejected: a TEMP trigger's name may not be
// schema-qualified at all, not even "main." Verified directly against real
// SQLite -- "temporary trigger may not have qualified name" (trigger7.test)
// -- while the same statement without TEMP is fine, and an unqualified TEMP
// trigger is fine.
func TestTempTriggerQualifiedNameRejected(t *testing.T) {
	for _, tc := range []struct {
		stmt      string
		wantError bool
	}{
		{`CREATE TEMP TRIGGER main.r1 AFTER INSERT ON t1 BEGIN SELECT 1; END`, true},
		{`CREATE TEMP TRIGGER r2 AFTER INSERT ON t1 BEGIN SELECT 1; END`, false},
		{`CREATE TRIGGER main.r3 AFTER INSERT ON t1 BEGIN SELECT 1; END`, false},
	} {
		edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		if err := edb.Exec(`CREATE TABLE t1(a,b)`); err != nil {
			t.Fatal(err)
		}
		if _, err := cdb.Exec(`CREATE TABLE t1(a,b)`); err != nil {
			t.Fatal(err)
		}
		eErr := edb.Exec(tc.stmt)
		_, cErr := cdb.Exec(tc.stmt)
		if (cErr != nil) != tc.wantError {
			t.Fatalf("[%s] oracle changed: cgo=%v", tc.stmt, cErr)
		}
		if (eErr == nil) != (cErr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", tc.stmt, eErr, cErr)
		} else if eErr != nil {
			if got := strings.TrimPrefix(eErr.Error(), "engine: "); got != cErr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", tc.stmt, got, cErr.Error())
			}
		}
		edb.Close()
		cdb.Close()
	}
}

// beforeTriggerSameRowCases cover SQLite's ticket e25d9ea771: a BEFORE
// trigger that UPDATEs the very row being updated. The outer UPDATE built its
// new row image BEFORE the trigger ran, so storing it verbatim wrote back the
// pre-trigger values and silently undid the trigger. Every column the outer
// statement does not itself assign has to be re-read after the BEFORE
// program. The neighbouring cases keep the merge honest: a trigger touching
// another table, and a BEFORE INSERT trigger (which never had the bug).
var beforeTriggerSameRowCases = []struct {
	name  string
	stmts []string
	query string
}{
	{"before update, same row via rowid", []string{
		`CREATE TABLE t(a,b,c)`, `INSERT INTO t VALUES(1,31,32)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN UPDATE t SET b=b+1 WHERE rowid=old.rowid; END`,
		`UPDATE t SET a=5`}, `SELECT * FROM t`},
	{"before update, whole table", []string{
		`CREATE TABLE t(a,b,c)`, `INSERT INTO t VALUES(1,31,32)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN UPDATE t SET b=b+1; END`,
		`UPDATE t SET a=5`}, `SELECT * FROM t`},
	{"before update OF column", []string{
		`CREATE TABLE t(a,b,c)`, `INSERT INTO t VALUES(1,31,32)`,
		`CREATE TRIGGER tb BEFORE UPDATE OF a ON t BEGIN UPDATE t SET b=b+1, c=c+1 WHERE rowid=old.rowid; END`,
		`UPDATE t SET a=5, c=99`}, `SELECT * FROM t`},
	{"before update touching another table", []string{
		`CREATE TABLE t(a,b,c)`, `CREATE TABLE u(z)`, `INSERT INTO t VALUES(1,31,32)`, `INSERT INTO u VALUES(0)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN UPDATE u SET z=z+1; END`,
		`UPDATE t SET a=5`}, `SELECT * FROM u`},
	{"before insert updating the same table", []string{
		`CREATE TABLE t(a,b,c)`, `INSERT INTO t VALUES(1,31,32)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN UPDATE t SET b=b+1; END`,
		`INSERT INTO t VALUES(9,9,9)`}, `SELECT * FROM t ORDER BY a`},
}

func TestBeforeTriggerUpdatingSameRowParity(t *testing.T) {
	for _, tc := range beforeTriggerSameRowCases {
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
			eCols, eVals, qErr := p.QueryArgs(tc.query, nil)
			if qErr != nil {
				t.Fatal(qErr)
			}
			cCols, cRows, sErr := cgoSelect(t, cdb, tc.query, nil)
			if sErr != nil {
				t.Fatal(sErr)
			}
			eRows := engineRowsToStrings(eVals)
			if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
				t.Errorf("DIVERGES: %s\n  engine: %v\n  cgo:    %v", reason, eRows, cRows)
			}
		})
	}
}
