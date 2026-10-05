// This file is the differential gate for the "||" operator's result type: it
// is always TEXT, even when BOTH operands are blobs (pragma.test's t4, whose
// "INSERT INTO t4(b) SELECT b||b||b||b FROM t4" stores TEXT). Only NULL
// short-circuits.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestConcatAlwaysTextParity(t *testing.T) {
	got := map[string]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, _ := sql.Open(drv, dsn)
		db.SetMaxOpenConns(1)
		for _, q := range []string{
			`SELECT typeof(x'0123' || x'4567'), quote(x'0123' || x'4567')`,
			`SELECT typeof(x'0123' || 'ab'), quote(x'0123' || 'ab')`,
			`SELECT typeof('ab' || x'0123')`,
			`SELECT typeof(x'0123' || 5)`,
			`SELECT typeof(x'0123' || NULL)`,
			`SELECT typeof(x'' || x'')`,
			`SELECT quote(x'41' || x'42')`,
		} {
			out := queryString(t, db, q)
			if drv == "sqlite" {
				got[q] = out
				continue
			}
			if got[q] != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", q, got[q], out)
			}
		}
		db.Close()
	}
}
