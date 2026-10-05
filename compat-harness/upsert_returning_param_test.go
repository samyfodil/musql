package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestUpsertReturningBoundParameter verifies that bound parameters work in
// INSERT ... ON CONFLICT ... RETURNING statements.
func TestUpsertReturningBoundParameter(t *testing.T) {
	const setup = `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`
	const seed = `INSERT INTO t VALUES(1,'one')`
	for _, tc := range []struct {
		name string
		stmt string
		args []any
	}{
		{"update arm, params in VALUES, SET and RETURNING",
			`INSERT INTO t VALUES(?1,'ins') ON CONFLICT(a) DO UPDATE SET b=?2 RETURNING a, b, ?3`,
			[]any{1, "setval", "tag"}},
		{"insert arm, params in VALUES, SET and RETURNING",
			`INSERT INTO t VALUES(?1,'ins') ON CONFLICT(a) DO UPDATE SET b=?2 RETURNING a, b, ?3`,
			[]any{2, "setval", "tag"}},
		{"param inside a RETURNING expression",
			`INSERT INTO t VALUES(1,'ins') ON CONFLICT(a) DO UPDATE SET b='u' RETURNING a + ?, b || ?`,
			[]any{10, "!"}},
		{"param in the DO UPDATE WHERE, false",
			`INSERT INTO t VALUES(1,'ins') ON CONFLICT(a) DO UPDATE SET b='u' WHERE a>? RETURNING a, b`,
			[]any{99}},
		{"param in the DO UPDATE WHERE, true",
			`INSERT INTO t VALUES(1,'ins') ON CONFLICT(a) DO UPDATE SET b='u' WHERE a>? RETURNING a, b`,
			[]any{0}},
		{"null param", `INSERT INTO t VALUES(1,'ins') ON CONFLICT(a) DO UPDATE SET b=? RETURNING a, b`,
			[]any{nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, eng := range []struct{ driver, name string }{{"sqlite3", "cgo"}, {"sqlite", "musql"}} {
				dir := t.TempDir()
				db, err := sql.Open(eng.driver, filepath.Join(dir, eng.name+".db"))
				if err != nil {
					t.Fatalf("%s open: %v", eng.name, err)
				}
				for _, s := range []string{setup, seed} {
					if _, err := db.Exec(s); err != nil {
						db.Close()
						t.Fatalf("%s setup %q: %v", eng.name, s, err)
					}
				}
				rows, qerr := db.Query(tc.stmt, tc.args...)
				if qerr != nil {
					got[eng.name] = "ERR"
					db.Close()
					continue
				}
				cols, _ := rows.Columns()
				var lines []string
				for rows.Next() {
					vals := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range vals {
						ptrs[i] = &vals[i]
					}
					if serr := rows.Scan(ptrs...); serr != nil {
						lines = append(lines, "SCANERR")
						break
					}
					cells := make([]string, len(vals))
					for i, v := range vals {
						switch x := v.(type) {
						case nil:
							cells[i] = "NULL"
						case []byte:
							cells[i] = string(x)
						default:
							cells[i] = fmt.Sprintf("%v", x)
						}
					}
					lines = append(lines, strings.Join(cells, "|"))
				}
				if rerr := rows.Err(); rerr != nil {
					lines = append(lines, "ROWSERR")
				}
				rows.Close()
				// The stored row too: a RETURNING list that reads the wrong
				// registers and a WRITE that stores the wrong values are
				// different bugs, and this case has to be able to tell them
				// apart.
				after, aerr := db.Query(`SELECT a,b FROM t ORDER BY a`)
				if aerr == nil {
					for after.Next() {
						var a any
						var b any
						if after.Scan(&a, &b) == nil {
							lines = append(lines, fmt.Sprintf("row %v,%v", a, b))
						}
					}
					after.Close()
				}
				got[eng.name] = strings.Join(lines, " ; ")
				db.Close()
			}
			if got["cgo"] != got["musql"] {
				t.Errorf("%q %v DIVERGES\n  cgo:    %s\n  musql: %s", tc.stmt, tc.args, got["cgo"], got["musql"])
			}
		})
	}
}
