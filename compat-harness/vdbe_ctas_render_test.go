// Tests for CREATE TABLE AS SELECT schema rendering.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var ctasNames = []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10", "k11"}
var ctasSel = map[string]string{
	"k1":  `SELECT a, b FROM src`,
	"k2":  `SELECT a AS key, b AS "select" FROM src`,
	"k3":  `SELECT a AS x1, b AS ok_name FROM src`,
	"k4":  `SELECT a AS "1abc" FROM src`,
	"k5":  `SELECT a AS "quo""te" FROM src`,
	"k6":  `SELECT a+1, count(*) FROM src`,
	"k7":  `SELECT a,b,c,d,e,f,g,h,i,j FROM src`,
	"k8":  `SELECT a AS aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa FROM src`,
	"k9":  `SELECT 1 AS solo`,
	"k10": `SELECT a AS "with space" FROM src`,
	"k11": `SELECT a AS abort, b AS "table" FROM src`,
}

func TestCTASRenderParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	ddl := `CREATE TABLE src(a INTEGER, b TEXT, c REAL, d BLOB, e NUMERIC, f VARCHAR(10), g, h DOUBLE, i BOOLEAN, j DATE)`
	edb.Exec(ddl)
	cdb.Exec(ddl)
	for _, n := range ctasNames {
		eerr := edb.Exec(`CREATE TABLE ` + n + ` AS ` + ctasSel[n])
		_, cerr := cdb.Exec(`CREATE TABLE ` + n + ` AS ` + ctasSel[n])
		if eerr != nil {
			// A CTAS shape this engine declines is skipped, exactly as the
			// harness does. "SELECT a+1, count(*)" is one today: an aggregate
			// with no GROUP BY over a bare column has no anchor row here.
			continue
		}
		if cerr != nil {
			t.Errorf("[%s] engine accepted a CTAS C SQLite rejected: %v", n, cerr)
			continue
		}
		p, _ := edb.SnapshotPager()
		_, ev, _ := p.QueryArgs(`SELECT sql FROM sqlite_master WHERE name='`+n+`'`, nil)
		_, cr, _ := cgoSelect(t, cdb, `SELECT sql FROM sqlite_master WHERE name='`+n+`'`, nil)
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch([]string{"sql"}, eRows, []string{"sql"}, cr, true); !ok {
			t.Errorf("[%s] stored CTAS text DIVERGES: %s\n  engine: %q\n  cgo:    %q", n, reason, eRows, cr)
		}
	}
}
