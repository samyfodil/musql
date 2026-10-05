// IN subqueries in aggregate GROUP BY HAVING and ORDER BY expressions.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var inSubSetup = []string{
	`CREATE TABLE t1(a INTEGER, b INTEGER, c TEXT)`,
	`INSERT INTO t1 VALUES(1,2,'x'),(1,9,'y'),(1,4,'z'),(2,5,'p'),(2,7,'q')`,
}

var inSubQ = []string{
	`SELECT a, count(*) FROM t1 GROUP BY a HAVING a IN (SELECT 2)`,
	`SELECT a, count(*) FROM t1 GROUP BY a HAVING a NOT IN (SELECT 2)`,
	`SELECT a, count(*) FROM t1 GROUP BY a HAVING a IN (SELECT a FROM t1 WHERE b>6)`,
	`SELECT a, count(*) FROM t1 GROUP BY a HAVING max(b) IN (SELECT 9)`,
	`SELECT a, count(*) FROM t1 GROUP BY a ORDER BY a IN (SELECT 1), a`,
}

func TestGroupItemInSubqueryParity(t *testing.T) {
	dir := t.TempDir()
	edb, err := engine.Create(filepath.Join(dir, "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(dir, "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	for _, s := range inSubSetup {
		if _, _, err := edb.ExecArgs(s, nil); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	for _, q := range inSubQ {
		ecols, ev, eerr := p.QueryArgs(q, nil)
		ccols, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			t.Errorf("[%s] engine declined a shape this gate covers: %v", q, eerr)
			continue
		}
		if cerr != nil {
			t.Errorf("[%s] cgo errored where the engine did not: %v", q, cerr)
			continue
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch(ecols, eRows, ccols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
