// CREATE VIEW must reject parameters and references to other databases.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var viewValidQ = []string{
	`CREATE VIEW v12 AS SELECT a FROM t1 WHERE b=?`,
	`CREATE VIEW v12b AS SELECT a FROM t1 WHERE b=?1`,
	`CREATE VIEW v12c AS SELECT a FROM t1 WHERE b=:n`,
	`CREATE VIEW v13 AS SELECT y FROM two.t2`,
	`CREATE VIEW v14 AS SELECT y FROM main.t1`,
	`CREATE VIEW v15 AS SELECT a FROM t1 WHERE b=5`,
	`CREATE VIEW v16 AS SELECT a FROM t1 WHERE b IN (SELECT a FROM t1 WHERE a=?)`,
}

func TestCreateViewValidationParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{`CREATE TABLE t1(a,b)`} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	for _, q := range viewValidQ {
		eerr := edb.Exec(q)
		_, cerr := cdb.Exec(q)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", q, eerr, cerr)
			continue
		}
		if cerr != nil {
			if got := strings.TrimPrefix(eerr.Error(), "engine: "); got != cerr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", q, got, cerr.Error())
			}
		}
	}
}
