// ANALYZE scoped forms and stat cleanup on DROP.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestAnalyzeScopeAndDropParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a,b,c)`, `CREATE INDEX t1i1 ON t1(a)`, `CREATE INDEX t1i2 ON t1(b)`,
		`INSERT INTO t1 VALUES(1,2,3),(4,5,6)`,
		`CREATE TABLE t2(x,y)`, `CREATE INDEX t2i1 ON t2(x)`,
		`INSERT INTO t2 VALUES(1,2),(3,4)`,
	}
	steps := []string{
		`ANALYZE t1i1`,
		`ANALYZE t1`,
		`ANALYZE`,
		`ANALYZE main`,
		`ANALYZE main.t1`,
		`DROP INDEX t1i2`,
		`DROP TABLE t2`,
	}
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range setup {
		edb.Exec(s)
		cdb.Exec(s)
	}
	q := `SELECT idx, stat FROM sqlite_stat1 ORDER BY idx`
	for _, s := range steps {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		p, _ := edb.SnapshotPager()
		_, ev, _ := p.QueryArgs(q, nil)
		_, cr, _ := cgoSelect(t, cdb, q, nil)
		if (eerr == nil) != (cerr == nil) {
			t.Fatalf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eerr, cerr)
		}
		eRows := engineRowsToStrings(ev)
		cols := []string{"idx", "stat"}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("after %q, sqlite_stat1 DIVERGES: %s\n  engine: %v\n  cgo:    %v", s, reason, eRows, cr)
		}
	}
}
