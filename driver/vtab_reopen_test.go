package driver

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestVirtualTableRowsSurviveAReopen verifies that virtual table writes
// persist across Close/reopen.
func TestVirtualTableRowsSurviveAReopen(t *testing.T) {
	for _, tc := range []struct{ name, create, insert, read, want string }{
		{"fts4", `CREATE VIRTUAL TABLE v USING fts4(c)`,
			`INSERT INTO v(c) VALUES('apple'),('banana')`,
			`SELECT c FROM v WHERE v MATCH 'apple'`, "[apple] "},
		{"fts3", `CREATE VIRTUAL TABLE v USING fts3(c)`,
			`INSERT INTO v(c) VALUES('apple'),('banana')`,
			`SELECT c FROM v WHERE v MATCH 'apple'`, "[apple] "},
		{"rtree", `CREATE VIRTUAL TABLE v USING rtree(id, x0, x1)`,
			`INSERT INTO v VALUES(1, 0.0, 1.0),(2, 5.0, 6.0)`,
			`SELECT id FROM v WHERE x0 > 4`, "[2] "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.musq")
			db, err := sql.Open(DriverName, path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			for _, s := range []string{tc.create, tc.insert} {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			if got := rowsAsText(t, db, tc.read); got != tc.want {
				t.Fatalf("before the reopen: got %s, want %s", got, tc.want)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db2, err := sql.Open(DriverName, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db2.Close()
			db2.SetMaxOpenConns(1)
			if got := rowsAsText(t, db2, tc.read); got != tc.want {
				t.Errorf("after the reopen: got %s, want %s -- the rows did not reach the file", got, tc.want)
			}
		})
	}
}
