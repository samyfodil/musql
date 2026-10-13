package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// A point read through SQL on a table whose segments sit beside a delta: the
// segment's copy of a row answers only when the delta neither puts nor kills that
// rowid. The read used to check the WHOLE table for a delta (sorting it, once per
// lookup) and decline; it now checks the one rowid, so each case is pinned: an
// untouched row, an updated one, a deleted one, one inserted past the segments,
// and rowids nothing holds -- through a fresh reader and through the writing
// session itself.
func TestPointReadBesideTheDelta(t *testing.T) {
	compactionOffForTest = true
	defer func() { compactionOffForTest = false }()
	path := filepath.Join(t.TempDir(), "p.musq")
	sess, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if err := sess.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if _, err := sess.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, b TEXT)`)
	exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 300)
		INSERT INTO t SELECT i, 'v' || i FROM c`)
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CompactSegmentFile(path); err != nil {
		t.Fatal(err)
	}
	sess, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	exec(`UPDATE t SET b = 'u' || id WHERE id IN (7, 150, 300)`)
	exec(`DELETE FROM t WHERE id IN (1, 64, 299)`)
	exec(`INSERT INTO t VALUES (301, 'n301'), (5000, 'n5000')`)

	want := func(id int64) (string, bool) {
		switch {
		case id == 1 || id == 64 || id == 299:
			return "", false
		case id == 7 || id == 150 || id == 300:
			return fmt.Sprintf("u%d", id), true
		case id == 301 || id == 5000:
			return fmt.Sprintf("n%d", id), true
		case id >= 2 && id <= 298:
			return fmt.Sprintf("v%d", id), true
		}
		return "", false
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.segs == nil || p.segs.cleanFor(rootOf(t, p, "t")) {
		t.Fatal("the fixture must leave t's segments beside a delta")
	}
	readers := map[string]func(int64) ([][]Value, error){
		"reader": func(id int64) ([][]Value, error) {
			_, rows, err := p.QueryArgs(`SELECT b FROM t WHERE id = ?`, []Value{{Typ: Int, I: id}})
			return rows, err
		},
		"session": func(id int64) ([][]Value, error) {
			_, rows, err := sess.Query(`SELECT b FROM t WHERE id = ?`, []Value{{Typ: Int, I: id}})
			return rows, err
		},
	}
	for name, read := range readers {
		for _, id := range []int64{-1, 0, 1, 2, 7, 63, 64, 65, 150, 298, 299, 300, 301, 302, 4999, 5000, 5001} {
			rows, err := read(id)
			if err != nil {
				t.Fatalf("%s id %d: %v", name, id, err)
			}
			w, ok := want(id)
			if !ok {
				if len(rows) != 0 {
					t.Errorf("%s id %d: got %v, want no row", name, id, rows)
				}
				continue
			}
			if len(rows) != 1 || string(rows[0][0].S) != w {
				t.Errorf("%s id %d: got %v, want %q", name, id, rows, w)
			}
		}
	}
}

func rootOf(t *testing.T, p *ReadOnlyPager, name string) uint32 {
	t.Helper()
	schema, err := p.Schema()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range schema {
		if r.Type == "table" && r.Name == name {
			return r.RootPage
		}
	}
	t.Fatalf("no table %s", name)
	return 0
}
