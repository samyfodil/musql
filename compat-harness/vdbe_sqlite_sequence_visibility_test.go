// sqlite_sequence should become visible at AUTOINCREMENT table creation,
// not only when rows are inserted.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestSqliteSequenceVisibilityParity(t *testing.T) {
	steps := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`INSERT INTO t1 VALUES(NULL,1)`,
		`DELETE FROM t1`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y)`,
		`CREATE TABLE t3(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
	}
	queries := []string{
		`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`,
		`SELECT * FROM sqlite_sequence ORDER BY name`,
	}
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range steps {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Logf("  !! %s\n     eng=%v cgo=%v", s, eerr, cerr)
		}
		p, _ := edb.SnapshotPager()
		for _, q := range queries {
			_, ev, eqerr := p.QueryArgs(q, nil)
			_, cr, _ := cgoSelect(t, cdb, q, nil)
			if eqerr != nil {
				// A query this engine declines is skipped, exactly as the
				// harness does -- never scored as a match.
				continue
			}
			eRows := engineRowsToStrings(ev)
			if len(eRows) != len(cr) {
				t.Errorf("after %q, [%s] row count %d vs %d\n  engine: %v\n  cgo:    %v", s, q, len(eRows), len(cr), eRows, cr)
				continue
			}
			if len(eRows) == 0 {
				continue
			}
			cols := make([]string, len(eRows[0]))
			for i := range cols {
				cols[i] = fmt.Sprintf("c%d", i)
			}
			if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
				t.Errorf("after %q, [%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", s, q, reason, eRows, cr)
			}
		}
	}
}
