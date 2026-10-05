// Compound SELECT result columns must take collation from the first arm
// with an opinion, not always the leftmost.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var compoundCollQ = []string{
	`SELECT a||'' FROM tst UNION ALL SELECT b COLLATE nocase FROM tst ORDER BY 1`,
	`SELECT a FROM tst UNION ALL SELECT b COLLATE nocase FROM tst ORDER BY 1`,
	`SELECT a COLLATE nocase FROM tst UNION ALL SELECT b FROM tst ORDER BY 1`,
	`SELECT a FROM tst UNION ALL SELECT b FROM tst ORDER BY 1`,
	`SELECT a||'' FROM tst UNION SELECT b COLLATE nocase FROM tst ORDER BY 1`,
	`SELECT a||'' FROM tst UNION ALL SELECT b COLLATE nocase FROM tst ORDER BY 1 COLLATE binary`,
	`SELECT a||'' FROM tst UNION SELECT b||'' FROM tst ORDER BY 1`,
	`SELECT a||'' FROM tst UNION ALL SELECT b||'' FROM tst UNION ALL SELECT b COLLATE nocase FROM tst ORDER BY 1`,
	`SELECT n FROM tnum UNION SELECT n FROM tnum ORDER BY 1`,
}

func TestCompoundArmCollationParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{`CREATE TABLE tst(a TEXT, b TEXT)`, `INSERT INTO tst VALUES('A','a'),('B','b'),('C','c')`, `CREATE TABLE tnum(n)`, `INSERT INTO tnum VALUES(3),(1),(2)`} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	p, _ := edb.SnapshotPager()
	for _, q := range compoundCollQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil || cerr != nil {
			t.Fatalf("[%s] engine=%v cgo=%v", q, eerr, cerr)
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch([]string{"c"}, eRows, []string{"c"}, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
