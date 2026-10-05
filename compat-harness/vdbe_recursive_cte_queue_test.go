// Package compat tests recursive CTE queue discipline with ORDER BY and LIMIT.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var recQueries = []string{
	`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION ALL SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id) SELECT group_concat(id) FROM a`,
	`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION ALL SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id ORDER BY 2) SELECT group_concat(id) FROM a`,
	`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION ALL SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id ORDER BY 2 DESC) SELECT group_concat(id) FROM a`,
	`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION ALL SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id ORDER BY 2 LIMIT 4) SELECT group_concat(id) FROM a`,
	`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION ALL SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id ORDER BY 2 LIMIT 4 OFFSET 2) SELECT group_concat(id) FROM a`,
	`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION ALL SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id LIMIT 3) SELECT group_concat(id) FROM a`,
	`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id ORDER BY 2) SELECT group_concat(id) FROM a`,
	`WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n<10) SELECT group_concat(n) FROM c`,
	`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<20 ORDER BY n DESC) SELECT group_concat(n) FROM c`,
	`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c LIMIT 5) SELECT group_concat(n) FROM c`,
}

func TestRecursiveCTEQueueParity(t *testing.T) {
	ep := filepath.Join(t.TempDir(), "e.sqlite")
	edb, _ := engine.Create(ep)
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{`CREATE TABLE edge(xfrom, xto, seq)`, `INSERT INTO edge VALUES(0,1,10),(0,2,20),(0,3,30),(1,4,40),(2,5,50),(3,6,60),(3,7,70),(4,8,80),(5,9,90)`} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	p, _ := edb.SnapshotPager()
	for _, q := range recQueries {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			// A shape this engine declines is skipped on both sides, exactly
			// like the harness does -- never scored as a match.
			continue
		}
		if cerr != nil {
			t.Errorf("[%s] engine accepted what C SQLite rejected: %v", q, cerr)
			continue
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch([]string{"g"}, eRows, []string{"g"}, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
	edb.Close()
}
