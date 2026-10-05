// Tests ORDER BY expressions on whole-table aggregates. The expression is
// validated at prepare time even though it never affects the single output row.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// aggOrderByExprSupported are supported ORDER BY expression shapes
var aggOrderByExprSupported = []string{
	`SELECT sum(b) FROM t ORDER BY max(c)`,
	`SELECT sum(b) FROM t ORDER BY abs(a)`,
	`SELECT sum(b) FROM t ORDER BY CASE WHEN a=1 THEN b ELSE c END`,
	`SELECT sum(b) FROM t ORDER BY a IN (1,2)`,
	`SELECT sum(b) FROM t ORDER BY a BETWEEN 1 AND 2`,
	`SELECT sum(b) FROM t ORDER BY c LIKE 'a%'`,
	`SELECT sum(b) FROM t ORDER BY c GLOB 'a*'`,
	`SELECT sum(b) FROM t ORDER BY count(*)`,
	`SELECT sum(b) FROM t ORDER BY count(*)+0`,
	`SELECT sum(b) FROM t ORDER BY max(a,b)`,
	`SELECT sum(b) FROM t ORDER BY -abs(a)`,
	`SELECT sum(b) FROM t ORDER BY group_concat(c,'-')`,
	`SELECT sum(b) FROM t ORDER BY sum(a) COLLATE nocase`,
	`SELECT sum(b) FROM t ORDER BY max(a) LIKE '1%'`,
	`SELECT sum(b) FROM t ORDER BY sum(a) FILTER (WHERE b>1)`,
	// GROUP BY query for control
	`SELECT a, sum(b) FROM t GROUP BY a ORDER BY count(*)+0`,
}

// aggOrderByExprErrors are rejected by C SQLite
var aggOrderByExprErrors = []string{
	`SELECT sum(b) FROM t ORDER BY nosuchfunc(a)`,
	`SELECT sum(b) FROM t ORDER BY abs(a,b)`,
	`SELECT sum(b) FROM t ORDER BY sum(count(a))`,
	`SELECT sum(b) FROM t ORDER BY count(DISTINCT a, b)`,
	`SELECT sum(b) FROM t ORDER BY count(nosuchcol)`,
	`SELECT sum(b) FROM t ORDER BY sum(a) FILTER (WHERE nosuchcol>1)`,
	`SELECT sum(b) FROM t ORDER BY nosuchcol`,
}

// aggOrderByExprDeclined are shapes this engine declines (subquery terms)
var aggOrderByExprDeclined = []string{
	`SELECT sum(b) FROM t ORDER BY (SELECT max(c) FROM t)`,
}

func TestAggOrderByExprParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b,c)`,
		`INSERT INTO t VALUES(1,50,'z'),(1,10,'a'),(2,1,'m'),(2,20,'b'),(3,30,'k')`,
	}
	edb, eerr := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if eerr != nil {
		t.Fatal(eerr)
	}
	defer edb.Close()
	cdb, cerr := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if cerr != nil {
		t.Fatal(cerr)
	}
	defer cdb.Close()
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	p, perr := edb.SnapshotPager()
	if perr != nil {
		t.Fatal(perr)
	}

	for _, q := range aggOrderByExprSupported {
		eCols, eVals, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Errorf("[%s] engine declined a supported shape: %v", q, err)
			continue
		}
		cCols, cRows, err := cgoSelect(t, cdb, q, nil)
		if err != nil {
			t.Errorf("[%s] cgo: %v", q, err)
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v %v\n  cgo:    %v %v",
				q, reason, eCols, engineRowsToStrings(eVals), cCols, cRows)
		}
	}

	for _, q := range aggOrderByExprErrors {
		if _, _, err := p.QueryArgs(q, nil); err == nil {
			t.Errorf("[%s] engine answered a statement C SQLite rejects", q)
		}
		if _, _, err := cgoSelect(t, cdb, q, nil); err == nil {
			t.Errorf("[%s] cgo no longer rejects this -- the pinned oracle behavior changed", q)
		}
	}

	for _, q := range aggOrderByExprDeclined {
		if _, _, err := p.QueryArgs(q, nil); err == nil {
			t.Errorf("[%s] this shape is declined on purpose; if it now compiles, verify it against the oracle and move it to aggOrderByExprSupported", q)
		}
		if _, _, err := cgoSelect(t, cdb, q, nil); err != nil {
			t.Errorf("[%s] cgo now rejects this too -- move it to aggOrderByExprErrors: %v", q, err)
		}
	}
}
