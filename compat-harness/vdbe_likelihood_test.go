// This file tests likelihood()'s probability argument validation.
// The function accepts floating-point literals in [0.0, 1.0].
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestLikelihoodRangeParity(t *testing.T) {
	for _, q := range []string{
		`SELECT likelihood(1, 100)`,
		`SELECT likelihood(1, 0.5)`,
		`SELECT likelihood(1, 0.0)`,
		`SELECT likelihood(1, 0)`,
		`SELECT likelihood(1, 1)`,
		`SELECT likelihood(1, 1.0)`,
		`SELECT likelihood(1, -0.5)`,
		`SELECT likelihood(1, 2)`,
		`SELECT likelihood(1, 'x')`,
		`SELECT likelihood(5, 0.25)`,
		`SELECT likely(5)`,
		`SELECT unlikely(5)`,
	} {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", ":memory:")
		p, _ := edb.SnapshotPager()
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		e := engineRowsToStrings(ev)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", q, eerr, cerr)
		} else if cerr != nil {
			got := strings.TrimPrefix(strings.TrimPrefix(eerr.Error(), "vdbe: semantic error: "), "engine: ")
			if got != cerr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", q, got, cerr.Error())
			}
		} else if len(e) != len(cr) || (len(e) > 0 && e[0][0] != cr[0][0]) {
			t.Errorf("[%s] DIVERGES\n  engine: %v\n  cgo:    %v", q, e, cr)
		}
		edb.Close()
		cdb.Close()
	}
}
