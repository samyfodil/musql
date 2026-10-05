// TestIndexedByParity verifies INDEXED BY hint validation: hints must name
// existing indexes on the hinted table, and views cannot use index hints.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var idxByQ = []string{
	`SELECT * FROM t1 INDEXED BY i1 WHERE a = 'one'`,
	`SELECT * FROM t1 INDEXED BY i3 WHERE a = 'one' AND b = 'two'`,
	`SELECT * FROM t1 INDEXED BY i2 WHERE a = 'one'`,
	`SELECT * FROM t2 INDEXED BY i2 WHERE c = 'x'`,
	`SELECT * FROM v1 INDEXED BY i1 WHERE a = 'one'`,
	`SELECT * FROM t1 NOT INDEXED WHERE a = 'one'`,
	`SELECT * FROM t1 WHERE a = 'one'`,
	`DELETE FROM t1 INDEXED BY i3 WHERE a='one'`,
	`UPDATE t1 INDEXED BY i3 SET b='x' WHERE a='one'`,
}

func TestIndexedByParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{`CREATE TABLE t1(a,b)`, `CREATE TABLE t2(c)`, `CREATE INDEX i1 ON t1(a)`, `CREATE INDEX i2 ON t2(c)`, `INSERT INTO t1 VALUES('one','two')`, `CREATE VIEW v1 AS SELECT * FROM t1`} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	p, _ := edb.SnapshotPager()
	for _, q := range idxByQ {
		var eerr, cerr error
		if strings.HasPrefix(q, "SELECT") {
			_, _, eerr = p.QueryArgs(q, nil)
			_, _, cerr = cgoSelect(t, cdb, q, nil)
		} else {
			eerr = edb.Exec(q)
			_, cerr = cdb.Exec(q)
		}
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", q, eerr, cerr)
		}
	}
}
