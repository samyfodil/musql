// Tests that derived tables and views propagate their column collations to
// outer queries, including aggregate columns.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var viewPropQ = []string{
	`SELECT * FROM t4 WHERE a = 'THIS'`,
	`SELECT * FROM (SELECT * FROM t4) WHERE a = 'THIS'`,
	`SELECT * FROM v11 WHERE a = 'THIS'`,
	`SELECT * FROM (SELECT a FROM t4) WHERE a = 'THIS'`,
	`SELECT * FROM (SELECT a AS z FROM t4) WHERE z = 'THIS'`,
	`SELECT c0<c1 FROM v0`,
	`SELECT c1<c0 FROM v0`,
	`SELECT c0<c1 FROM (SELECT t0.c0 AS c0, AVG(t0.c1) AS c1 FROM t0)`,
	`SELECT c0, c1, typeof(c0), typeof(c1) FROM v0`,
}

func TestDerivedCollationPropagationParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{
		`CREATE TABLE t4(a COLLATE nocase)`,
		`INSERT INTO t4 VALUES('This'),('this'),('THIS'),('other')`,
		`CREATE VIEW v11 AS SELECT * FROM t4`,
		`CREATE TABLE t0(c0 INT, c1 INT)`,
		`INSERT INTO t0 VALUES(0, 1)`,
		`CREATE VIEW v0 AS SELECT t0.c0 AS c0, AVG(t0.c1) AS c1 FROM t0`,
	} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	p, _ := edb.SnapshotPager()
	for _, q := range viewPropQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil || cerr != nil {
			t.Fatalf("[%s] engine=%v cgo=%v", q, eerr, cerr)
		}
		eRows := engineRowsToStrings(ev)
		if len(eRows) != len(cr) {
			t.Errorf("[%s] row count %d vs %d\n  engine: %v\n  cgo:    %v", q, len(eRows), len(cr), eRows, cr)
			continue
		}
		if len(eRows) == 0 {
			continue
		}
		cols := make([]string, len(eRows[0]))
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
