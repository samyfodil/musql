// Derived table and view column affinity in comparisons.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var affQ = []string{
	`SELECT c0, c1, typeof(c0), typeof(c1) FROM v0`,
	`SELECT c0<c1 FROM v0`,
	`SELECT c1<c0 FROM v0`,
	`SELECT 1 FROM v0 WHERE c1<c0`,
	`SELECT 1 FROM v0 WHERE c0<c1`,
	`SELECT c0<c1 FROM (SELECT t0.c0 AS c0, AVG(t0.c1) AS c1 FROM t0)`,
	`SELECT c1<c0 FROM (SELECT t0.c0 AS c0, AVG(t0.c1) AS c1 FROM t0)`,
	`SELECT c0<c1 FROM (SELECT t0.c0 AS c0, t0.c1 AS c1 FROM t0)`,
	`SELECT c0<c1 FROM t0`,
}

func TestDerivedComputedAffinityParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{
		`CREATE TABLE t0(c0 TEXT, c1)`,
		`INSERT INTO t0(c0, c1) VALUES (-1, 0)`,
		`CREATE VIEW v0(c0, c1) AS SELECT t0.c0, AVG(t0.c1) FROM t0`,
	} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	p, _ := edb.SnapshotPager()
	for _, q := range affQ {
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
