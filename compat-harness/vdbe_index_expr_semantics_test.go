// This file pins that a CREATE INDEX's indexed EXPRESSION and a partial
// index's WHERE are semantically validated even though this engine records
// such an index as decorative and never materializes it -- C SQLite
// compiles them, so an invalid call inside one is rejected at CREATE time
// (altertab3.test). See vdbe_likelihood_test.go for the underlying rule.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestIndexExprSemanticsParity(t *testing.T) {
	for _, q := range []string{
		`CREATE INDEX i2 ON t2((LIKELIHOOD(c0, 100) IN ()))`,
		`CREATE INDEX i3 ON t2(likelihood(c0, 0.5))`,
		`CREATE INDEX i4 ON t2(likelihood(c0, 0.0))`,
		`CREATE INDEX i5 ON t2(c0+1)`,
		`CREATE INDEX i6 ON t2(c0) WHERE likelihood(c0, 100)`,
	} {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", ":memory:")
		edb.Exec(`CREATE TABLE t2(c0)`)
		cdb.Exec(`CREATE TABLE t2(c0)`)
		eerr := edb.Exec(q)
		_, cerr := cdb.Exec(q)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", q, eerr, cerr)
		} else if cerr != nil {
			if got := strings.TrimPrefix(eerr.Error(), "engine: "); got != cerr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", q, got, cerr.Error())
			}
		}
		edb.Close()
		cdb.Close()
	}
}
