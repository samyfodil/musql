package compat

// Window function queries: row order without outer ORDER BY.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var winQueries = []string{
	`SELECT x, count(*) OVER (ORDER BY x) FROM t1`,
	`SELECT x FROM t1`,
	`SELECT x, count(*) OVER () FROM t1`,
	`SELECT x, row_number() OVER (ORDER BY x DESC) FROM t1`,
	`SELECT x, count(*) OVER (PARTITION BY y ORDER BY x) FROM t1`,
	`SELECT x, count(*) OVER (ORDER BY x) FROM t1 ORDER BY x DESC`,
	`SELECT x, sum(x) OVER (ORDER BY y) FROM t1`,
}

func TestWindowEmitOrderParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{`CREATE TABLE t1(x,y)`, `INSERT INTO t1 VALUES(7,1),(1,2),(5,1),(3,2)`} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	p, _ := edb.SnapshotPager()
	for _, q := range winQueries {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil || cerr != nil {
			t.Fatalf("[%s] engine=%v cgo=%v", q, eerr, cerr)
		}
		eRows := engineRowsToStrings(ev)
		cols := []string{"a", "b"}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("[%s] row ORDER/values DIVERGE: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
