package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestMatchPrepareTimeVsPerRow verifies when MATCH errors are raised: at
// prepare time for virtual tables or per row for ordinary columns. Over empty
// tables, prepare-time errors appear, per-row errors do not.
func TestMatchPrepareTimeVsPerRow(t *testing.T) {
	cases := []struct {
		name    string
		setup   []string
		q       string
		decline bool
	}{
		{"rtree empty", []string{`CREATE VIRTUAL TABLE rt USING rtree(id,x0,x1)`},
			`SELECT * FROM rt WHERE x0 MATCH 'x'`, false},
		{"rtree with a row", []string{`CREATE VIRTUAL TABLE rt USING rtree(id,x0,x1)`, `INSERT INTO rt VALUES(1,0.0,1.0)`},
			`SELECT * FROM rt WHERE x0 MATCH 'x'`, false},
		{"rtree by table name", []string{`CREATE VIRTUAL TABLE rt USING rtree(id,x0,x1)`},
			`SELECT * FROM rt WHERE rt MATCH 'x'`, false},
		{"rtree, qualified column", []string{`CREATE VIRTUAL TABLE rt USING rtree(id,x0,x1)`},
			`SELECT * FROM rt WHERE rt.x0 MATCH 'x'`, false},
		{"rtree joined to a plain table", []string{`CREATE VIRTUAL TABLE rt USING rtree(id,x0,x1)`, `CREATE TABLE p2(a)`, `INSERT INTO p2 VALUES(1)`},
			`SELECT * FROM p2, rt WHERE x0 MATCH 'x'`, false},
		{"rtree, an ordinary query still works", []string{`CREATE VIRTUAL TABLE rt USING rtree(id,x0,x1)`, `INSERT INTO rt VALUES(1,0.0,1.0),(2,5.0,6.0)`},
			`SELECT id FROM rt WHERE x0 >= 4.0 ORDER BY id`, false},
		{"plain empty answers in both", []string{`CREATE TABLE plain(a,b)`},
			`SELECT * FROM plain WHERE a MATCH 'x'`, false},
		{"plain with a row errors in both", []string{`CREATE TABLE plain(a,b)`, `INSERT INTO plain VALUES(1,'x')`},
			`SELECT * FROM plain WHERE a MATCH 'x'`, false},
		{"plain empty, non-literal pattern", []string{`CREATE TABLE plain(a,b)`},
			`SELECT * FROM plain WHERE a MATCH b`, false},
		{"a select-list MATCH over no rows", []string{`CREATE TABLE plain(a,b)`},
			`SELECT a MATCH 'x' FROM plain`, false},
		// LIMIT 0 short-circuits in C but not in this engine
		{"limit 0 short-circuits in C only", []string{`CREATE TABLE plain(a,b)`, `INSERT INTO plain VALUES(1,'x')`},
			`SELECT * FROM plain WHERE a MATCH 'x' LIMIT 0`, true},
	}
	for ci, tc := range cases {
		ci, tc := ci, tc
		t.Run(fmt.Sprintf("%02d-%s", ci, tc.name), func(t *testing.T) {
			var out [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(drv, p)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range tc.setup {
					if _, err := db.Exec(s); err != nil {
						t.Fatalf("%s: %v", s, err)
					}
				}
				out[i] = renderQuery(db, tc.q)
				db.Close()
			}
			if tc.decline {
				if out[1] != "ERR" {
					t.Errorf("%s: expected this engine to still reject (see the doc comment), got %s", tc.q, out[1])
				}
				return
			}
			if out[0] != out[1] {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", tc.q, out[0], out[1])
			}
		})
	}
}
