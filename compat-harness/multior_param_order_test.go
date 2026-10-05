package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// This file tests row order in WHERE_MULTI_OR plans with bound parameters.
// The test compares results between engines when disjuncts contain bound
// parameters.
func TestMultiOrBoundParameterOrder(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b,c)`,
		`CREATE INDEX ia ON t(a)`,
		`CREATE INDEX ib ON t(b)`,
		`INSERT INTO t VALUES(1,9,'r1'),(2,1,'r2'),(3,2,'r3'),(1,3,'r4'),(5,1,'r5')`,
	}
	for _, tc := range []struct {
		name string
		stmt string
		args []any
	}{
		{"or-two-params", `SELECT c FROM t WHERE a=? OR b=?`, []any{1, 1}},
		{"or-param-and-literal", `SELECT c FROM t WHERE a=? OR b=1`, []any{1}},
		{"or-three", `SELECT c FROM t WHERE a=? OR b=? OR c=?`, []any{1, 2, "r5"}},
		{"or-two-literals-control", `SELECT c FROM t WHERE a=1 OR b=1`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, eng := range []struct{ driver, name string }{{"sqlite3", "cgo"}, {"sqlite", "musql"}} {
				dir := t.TempDir()
				db, _ := sql.Open(eng.driver, filepath.Join(dir, eng.name+".db"))
				for _, s := range setup {
					if _, err := db.Exec(s); err != nil {
						t.Fatalf("%s setup: %v", eng.name, err)
					}
				}
				rows, qerr := db.Query(tc.stmt, tc.args...)
				if qerr != nil {
					got[eng.name] = "ERR " + qerr.Error()
					db.Close()
					continue
				}
				out := ""
				for rows.Next() {
					var c any
					rows.Scan(&c)
					out += fmt.Sprintf("%v,", c)
				}
				rows.Close()
				got[eng.name] = out
				db.Close()
			}
			if got["cgo"] != got["musql"] {
				t.Errorf("%s args=%v\n  cgo:    %s\n  musql: %s", tc.stmt, tc.args, got["cgo"], got["musql"])
			}
		})
	}
}
