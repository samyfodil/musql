// This file is the differential gate for an UNDEFINED FUNCTION inside a
// GENERATED COLUMN's expression. SQLite compiles that expression at CREATE
// TABLE time, so "b AS (f2(a+1))" with no such function f2 is
// "no such function: f2" (trustschema1.test), verified directly -- while an
// ordinary column's DEFAULT is not compiled that way and stays accepted.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestGeneratedColumnUnknownFuncParity(t *testing.T) {
	for _, q := range []string{
		`CREATE TABLE g1(a, b AS (f2(a+1)))`,
		`CREATE TABLE g2(a, b AS (abs(a)+1))`,
		`CREATE TABLE g3(a, b AS (a*2))`,
		`CREATE TABLE g4(a, b AS (length(a)))`,
		`CREATE TABLE g5(a, b AS (CASE WHEN nofn(a) THEN 1 END))`,
		`CREATE TABLE g6(a, b DEFAULT (nofn(1)))`,
	} {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", ":memory:")
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
