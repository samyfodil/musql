// TestR25CompoundRowidOrderParity tests ORDER BY rowid in compound SELECT
// statements against rowid and INTEGER PRIMARY KEY column equivalence.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var r25RowidOrderSchema = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
	`INSERT INTO t1 VALUES(3,'x'),(1,'y')`,
	`CREATE TABLE t2(c INTEGER PRIMARY KEY, d)`,
	`INSERT INTO t2 VALUES(2,'p')`,
	`CREATE TABLE w1(a INTEGER PRIMARY KEY,b,c,d,e,f,g)`,
	`INSERT INTO w1 VALUES(1,11,1001,1.001,100.1,'b','y'),(5,22,1001,2.0,100.1,'c','y'),(31,33,1001,3.0,100.1,'d','x'),(105,44,1,4.0,1.0,'e','w')`,
}

var r25RowidOrderQ = []string{
	`SELECT a FROM t1 UNION ALL SELECT c FROM t2 ORDER BY rowid`,
	`SELECT a FROM t1 UNION ALL SELECT c FROM t2 ORDER BY oid`,
	`SELECT a FROM t1 UNION ALL SELECT c FROM t2 ORDER BY _rowid_`,
	`SELECT a FROM t1 UNION ALL SELECT c FROM t2 ORDER BY t1.rowid`,
	`SELECT a FROM t1 UNION ALL SELECT c FROM t2 ORDER BY rowid DESC`,
	`SELECT b FROM t1 UNION ALL SELECT c FROM t2 ORDER BY rowid`,
	`SELECT a FROM t1 UNION ALL SELECT d FROM t2 ORDER BY rowid`,
	`SELECT rowid FROM t1 UNION ALL SELECT c FROM t2 ORDER BY a`,
	`SELECT a,b FROM t1 UNION ALL SELECT c,d FROM t2 ORDER BY rowid, 2`,
	`SELECT count(*) FROM w1 UNION ALL SELECT a FROM w1 WHERE a%100 IN (5,31,57,82,83,84,85,86,87) ORDER BY rowid`,
}

func TestR25CompoundRowidOrderParity(t *testing.T) {
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
	for _, s := range r25RowidOrderSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range r25RowidOrderQ {
		ec, ev, eerr := p.QueryArgs(q, nil)
		cc, cr, cerr := cgoSelect(t, cdb, q, nil)
		if cerr != nil {
			t.Errorf("[%s] the oracle rejected the statement this gate is built on: %v", q, cerr)
			continue
		}
		if eerr != nil {
			t.Errorf("[%s] engine declined a compound ORDER BY C SQLite answers: %v", q, eerr)
			continue
		}
		if len(ec) != len(cc) {
			t.Errorf("[%s] column count: engine %v, cgo %v", q, ec, cc)
			continue
		}
		for i := range ec {
			if ec[i] != cc[i] {
				t.Errorf("[%s] column %d named %q, C SQLite names it %q", q, i, ec[i], cc[i])
			}
		}
		eRows := engineRowsToStrings(ev)
		// The ORDER BY is the point: compare position-sensitively.
		if len(eRows) != len(cr) {
			t.Errorf("[%s] row count: engine %d, cgo %d", q, len(eRows), len(cr))
			continue
		}
		for i := range eRows {
			for j := range eRows[i] {
				if eRows[i][j] != cr[i][j] {
					t.Errorf("[%s] DIVERGES at row %d col %d: engine %q, cgo %q\n  engine: %v\n  cgo:    %v",
						q, i, j, eRows[i][j], cr[i][j], eRows, cr)
					break
				}
			}
		}
	}
}

// No arm's select list carries its own rowid, so C SQLite rejects the term.
// Widening the identity match must not have made it resolve.
func TestR25CompoundRowidOrderNoMatchDeclines(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	for _, s := range r25RowidOrderSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT b FROM t1 UNION ALL SELECT d FROM t2 ORDER BY rowid`,
		`SELECT b FROM t1 UNION ALL SELECT d FROM t2 ORDER BY oid`,
	} {
		if _, _, err := p.QueryArgs(q, nil); err == nil {
			t.Errorf("[%s] answered; C SQLite rejects this ORDER BY term", q)
		}
	}
}
