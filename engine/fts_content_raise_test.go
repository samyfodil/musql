package engine

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestExternalContentOverARaisingColumn: an external content table whose
// VIRTUAL column raises for a row never answers wrongly: each statement that
// would read that row declines, and a match that answers names the right row.
func TestExternalContentOverARaisingColumn(t *testing.T) {
	for _, mod := range []string{"fts5(g, content='src', content_rowid='id')", "fts4(g, content='src')"} {
		path := filepath.Join(t.TempDir(), "f.musq")
		buildDB(t, path, `CREATE TABLE src(id INTEGER PRIMARY KEY, a TEXT)`, `INSERT INTO src VALUES(1, 'bad'), (2, '"ok"')`)
		execDB(t, path, `ALTER TABLE src ADD COLUMN g AS (json_extract(a, '$')) VIRTUAL`, `CREATE VIRTUAL TABLE f USING `+mod)
		s, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		declines := 0
		check := func(what string, err error) bool {
			t.Helper()
			if err != nil && !errors.Is(err, errVDBEUnsupported) {
				t.Errorf("%s %s: %v, want an answer or a decline", mod, what, err)
			}
			if err != nil {
				declines++
			}
			return err == nil
		}
		check("rebuild", s.Exec(`INSERT INTO f(f) VALUES('rebuild')`))
		if check("index row 2", s.Exec(`INSERT INTO f(rowid, g) VALUES(2, 'ok')`)) {
			_, rows, err := s.Query(`SELECT rowid, g FROM f WHERE f MATCH 'ok'`, nil)
			if check("MATCH 'ok'", err) && (len(rows) != 1 || rows[0][0].I != 2) {
				t.Errorf("%s MATCH 'ok': %v", mod, rows)
			}
		}
		if check("index row 1", s.Exec(`INSERT INTO f(rowid, g) VALUES(1, 'zz')`)) {
			_, _, err := s.Query(`SELECT rowid, g FROM f WHERE f MATCH 'zz'`, nil)
			if err == nil {
				t.Errorf("%s MATCH 'zz' read the raising row and answered", mod)
			}
			check("MATCH 'zz'", err)
		}
		if declines == 0 {
			t.Errorf("%s: nothing declined over the raising row", mod)
		}
		s.Discard()
	}
}
