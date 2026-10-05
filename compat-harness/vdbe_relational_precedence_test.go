// Package compat tests comparison operator precedence.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var precQ = []string{
	`SELECT 0 LIKE 0 < 2, (0 LIKE 0) < 2, 0 LIKE (0 < 2)`,
	`SELECT 0 == 0 < 2, (0 == 0) < 2, 0 == (0 < 2)`,
	`SELECT 2 BETWEEN 1 AND 2 < 3`,
	`SELECT 1 < 2 == 1, (1 < 2) == 1`,
	`SELECT 1 = 2 < 3`,
	`SELECT 3 > 2 > 1`,
	`SELECT 1 IS 1 < 2`,
	`SELECT 1 IN (0,1) < 2`,
	`SELECT 5 GLOB '5' < 2`,
	`SELECT 1 <> 2 < 3`,
	`SELECT 2 < 3 AND 1 < 2`,
	`SELECT 1 + 2 < 4`,
	`SELECT 'a' || 'b' < 'ac'`,
	`SELECT 1 < 2 IS 1`,
	`SELECT NOT 1 < 2`,
}

func TestRelationalPrecedenceParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	p, _ := edb.SnapshotPager()
	for _, q := range precQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil || cerr != nil {
			t.Fatalf("[%s] engine=%v cgo=%v", q, eerr, cerr)
		}
		eRows := engineRowsToStrings(ev)
		cols := make([]string, len(eRows[0]))
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
