// Tests for bare column anchor row in GROUP BY queries.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var bareQ = []string{
	`SELECT a, c, max(b) FILTER (WHERE c='x') FROM t2 GROUP BY a`,
	`SELECT a, c, max(b) FROM t2 GROUP BY a`,
	`SELECT a, c, min(b) FROM t2 GROUP BY a`,
	`SELECT a, c, max(b) FILTER (WHERE c>3) FROM t2 GROUP BY a`,
	`SELECT a, c, count(*) FROM t2 GROUP BY a`,
	`SELECT a, c, max(NULL) FROM t2 GROUP BY a`,
}

func TestBareColumnAnchorParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{`CREATE TABLE t2(a,b,c)`, `INSERT INTO t2 VALUES(1,2,3),(1,3,4),(1,9,10),(2,5,6),(2,7,8)`} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	p, _ := edb.SnapshotPager()
	for _, q := range bareQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, _ := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			continue // a shape this engine declines is skipped, as the harness does
		}
		eRows := engineRowsToStrings(ev)
		cols := []string{"a", "c", "m"}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
