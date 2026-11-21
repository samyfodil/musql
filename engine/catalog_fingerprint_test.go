package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestCatalogFingerprintSeesEveryObject: a catalog change made through the DB
// directly, as the driver makes it, with no Exec of the session's to notice,
// moves the fingerprint, by the object alone too, and the commit writes it:
// the object is there at the next open.
func TestCatalogFingerprintSeesEveryObject(t *testing.T) {
	cases := []struct{ stmt, check string }{
		{`CREATE TABLE t2(a)`, `SELECT count(*) FROM sqlite_schema WHERE name = 't2'`},
		{`CREATE INDEX i ON t(a)`, `SELECT count(*) FROM sqlite_schema WHERE name = 'i'`},
		{`CREATE VIEW v AS SELECT a FROM t`, `SELECT count(*) FROM sqlite_schema WHERE name = 'v'`},
		{`CREATE TRIGGER g AFTER INSERT ON t BEGIN SELECT 1; END`, `SELECT count(*) FROM sqlite_schema WHERE name = 'g'`},
		{`CREATE VIRTUAL TABLE vrow USING fts5vocab(ft, row)`, `SELECT count(*) FROM sqlite_schema WHERE name = 'vrow'`},
		{`PRAGMA user_version = 7`, `PRAGMA user_version`},
		{`PRAGMA application_id = 7`, `PRAGMA application_id`},
	}
	for _, c := range cases {
		t.Run(c.stmt, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fp.musq")
			n, err := Create(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE ft USING fts5(x)`} {
				if err := n.Exec(s); err != nil {
					t.Fatal(err)
				}
			}
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			n, err = OpenWrite(path)
			if err != nil {
				t.Fatal(err)
			}
			if n.catalogMoved() {
				t.Fatal("the catalog moved before any change")
			}
			cookie := n.schemaCookie
			if err := n.DB.Exec(c.stmt); err != nil {
				t.Fatal(err)
			}
			if !n.catalogMoved() {
				t.Error("the fingerprint did not move")
			}
			if !strings.HasPrefix(c.stmt, "PRAGMA") {
				// The object alone moves it, the schema cookie held where it
				// was.
				now := n.schemaCookie
				n.schemaCookie = cookie
				if !n.catalogMoved() {
					t.Error("the fingerprint did not move on the object alone")
				}
				n.schemaCookie = now
			}
			if _, err := n.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			n, err = OpenWrite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer n.Discard()
			_, rows, err := n.Query(c.check, nil)
			if err != nil || len(rows) != 1 || fmt.Sprint(rows[0][0].I) == "0" {
				t.Errorf("after reopen %s = %v, %v", c.check, rows, err)
			}
		})
	}
}
