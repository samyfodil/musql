package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestVtabReturningBoundParameter verifies that virtual table INSERT RETURNING
// expressions correctly handle bound parameters.
func TestVtabReturningBoundParameter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		stmt  string
		args  []any
	}{
		{"bare param", `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r VALUES(1,2,3) RETURNING id, ?`, []any{42}},
		{"param in CASE", `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r VALUES(2,2,3) RETURNING CASE WHEN ? THEN 'yes' ELSE 'no' END`, []any{1}},
		{"param in arithmetic", `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r VALUES(3,2,3) RETURNING id + ?`, []any{10}},
		{"param concatenated", `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r VALUES(4,2,3) RETURNING id || ?`, []any{"Z"}},
		{"two params", `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r VALUES(5,2,3) RETURNING ?, ?`, []any{"a", "b"}},
		{"null param", `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r VALUES(6,2,3) RETURNING id, ?`, []any{nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, eng := range []struct{ driver, name string }{{"sqlite3", "cgo"}, {"sqlite", "musql"}} {
				dir := t.TempDir()
				db, err := sql.Open(eng.driver, filepath.Join(dir, eng.name+".db"))
				if err != nil {
					t.Fatalf("%s open: %v", eng.name, err)
				}
				if _, err := db.Exec(tc.setup); err != nil {
					db.Close()
					t.Fatalf("%s setup: %v", eng.name, err)
				}
				rows, qerr := db.Query(tc.stmt, tc.args...)
				if qerr != nil {
					got[eng.name] = "ERR: " + qerr.Error()
					db.Close()
					continue
				}
				cols, _ := rows.Columns()
				out := ""
				for rows.Next() {
					cells := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range cells {
						ptrs[i] = &cells[i]
					}
					if err := rows.Scan(ptrs...); err != nil {
						t.Fatalf("%s scan: %v", eng.name, err)
					}
					out += fmt.Sprintf("%v;", cells)
				}
				if err := rows.Err(); err != nil {
					out = "ERR: " + err.Error()
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
