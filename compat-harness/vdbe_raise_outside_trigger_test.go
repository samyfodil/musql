// RAISE() outside a trigger should match C SQLite's acceptance/rejection rules.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestRaiseOutsideTriggerParity(t *testing.T) {
	for _, q := range []string{
		`CREATE TABLE v0(c1 INT, c2 AS (RAISE(IGNORE)))`,
		`CREATE TABLE v1(c1 INT, c2 AS (RAISE(ABORT,'x')))`,
		`CREATE TABLE v3(c1 INT DEFAULT (RAISE(IGNORE)))`,
		`CREATE INDEX ii ON v4(RAISE(IGNORE))`,
	} {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", ":memory:")
		edb.Exec(`CREATE TABLE v4(c1 INT)`)
		cdb.Exec(`CREATE TABLE v4(c1 INT)`)
		eerr := edb.Exec(q)
		_, cerr := cdb.Exec(q)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", q, eErrOrNil(eerr), cerr)
		} else if cerr != nil {
			if got := strings.TrimPrefix(eerr.Error(), "engine: "); got != cerr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", q, got, cerr.Error())
			}
		}
		edb.Close()
		cdb.Close()
	}
}

// eErrOrNil keeps the error message readable when the engine accepted.
func eErrOrNil(err error) any {
	if err == nil {
		return "<accepted>"
	}
	return err
}
