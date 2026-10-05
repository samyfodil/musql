// TestConflictRollbackInTransaction verifies that ON CONFLICT ROLLBACK
// aborts the entire transaction and reverts all earlier changes.
package compat

import (
	"fmt"
	"testing"
)

func TestConflictRollbackInTransaction(t *testing.T) {
	base := []string{
		`CREATE TABLE t2(a INTEGER UNIQUE ON CONFLICT IGNORE, b INTEGER UNIQUE ON CONFLICT FAIL,
		  c INTEGER UNIQUE ON CONFLICT REPLACE, d INTEGER UNIQUE ON CONFLICT ABORT, e INTEGER UNIQUE ON CONFLICT ROLLBACK)`,
		`CREATE TABLE t3(x)`, `INSERT INTO t3 VALUES(1)`,
		`INSERT INTO t2 VALUES(1,1,1,1,1)`, `INSERT INTO t2 VALUES(2,2,2,2,2)`,
	}
	for _, ins := range []string{
		`INSERT INTO t2 VALUES(3,1,3,3,3)`,
		`INSERT INTO t2 VALUES(3,3,3,1,3)`,
		`INSERT INTO t2 VALUES(3,3,3,3,1)`,
	} {
		stmts := append(append([]string{}, base...), `BEGIN`, `UPDATE t3 SET x=x+1`, ins,
			`SELECT * FROM t2 ORDER BY a`, `COMMIT`, `SELECT x FROM t3`)
		c := run(t, "cgo", stmts)
		m := run(t, "musql", stmts)
		for i := range c {
			if i >= len(m) {
				break
			}
			if fmt.Sprint(c[i]) != fmt.Sprint(m[i]) {
				t.Errorf("[%s] stmt %d (%s) DIVERGES\n  cgo:    %v\n  musql: %v", ins, i, stmts[i], c[i], m[i])
			}
		}
	}
}
