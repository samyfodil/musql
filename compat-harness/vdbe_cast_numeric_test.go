// CAST(<text> AS NUMERIC) must fold REALs to INTEGERs when exactly representable.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var castQ = []string{
	`SELECT typeof(CAST('-5.0' AS NUMERIC)), CAST('-5.0' AS NUMERIC)||''`,
	`SELECT typeof(CAST('-5e+0' AS NUMERIC)), CAST('-5e+0' AS NUMERIC)||''`,
	`SELECT typeof(CAST('5.0' AS NUMERIC)), CAST('5.0' AS NUMERIC)||''`,
	`SELECT typeof(CAST('5.5' AS NUMERIC)), CAST('5.5' AS NUMERIC)||''`,
	`SELECT typeof(CAST('9223372036854775807.0' AS NUMERIC))`,
	`SELECT typeof(CAST('1e300' AS NUMERIC))`,
	`SELECT typeof(CAST(-5.0 AS NUMERIC)), CAST(-5.0 AS NUMERIC)||''`,
	`SELECT typeof(CAST('  -5.0  ' AS NUMERIC))`,
	`SELECT typeof(CAST('-5.25e+2' AS NUMERIC)), CAST('-5.25e+2' AS NUMERIC)||''`,
}

func TestCastNumericFoldParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	p, _ := edb.SnapshotPager()
	for _, q := range castQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, _ := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			t.Fatalf("[%s] %v", q, eerr)
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
