// Trigger bodies must not reference other databases, and qualified DROP
// statements must respect schema qualifiers.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestTriggerAndDropQualifierParity(t *testing.T) {
	for _, q := range []string{
		`CREATE TRIGGER r5 AFTER INSERT ON t5 BEGIN SELECT * FROM orig.t1; END`,
		`CREATE TRIGGER r6 AFTER INSERT ON t5 BEGIN SELECT * FROM main.t5; END`,
		`DROP VIEW temp.v1`,
		`DROP VIEW v1`,
		`DROP VIEW main.v1`,
		`DROP INDEX temp.i1`,
		`DROP TRIGGER temp.tr1`,
	} {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", ":memory:")
		for _, s := range []string{`CREATE TABLE t5(a)`, `CREATE VIEW v1 AS SELECT 1`, `CREATE INDEX i1 ON t5(a)`, `CREATE TRIGGER tr1 AFTER INSERT ON t5 BEGIN SELECT 1; END`} {
			edb.Exec(s)
			cdb.Exec(s)
		}
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
		edb.Close()
		cdb.Close()
	}
}
