package engine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadIntoAnEmptyTableCommitsBySize: a load into an empty table commits as
// a delta batch when the delta can take it, else by a rewrite; both read back.
func TestLoadIntoAnEmptyTableCommitsBySize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		load    []string
		rows    int
		rewrite bool
	}{
		{"one row in a transaction", []string{`BEGIN`, `INSERT INTO q VALUES (1, 'x')`, `COMMIT`}, 1, false},
		{"a few rows in a transaction", []string{`BEGIN`, `INSERT INTO q VALUES (1, 'x')`, `INSERT INTO q VALUES (2, 'y')`, `INSERT INTO q VALUES (3, 'z')`, `COMMIT`}, 3, false},
		{"INSERT ... SELECT", []string{`INSERT INTO q WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 5) SELECT i, 'v' || i FROM c`}, 5, false},
		{"more than the delta takes", []string{`BEGIN`,
			`INSERT INTO q WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 40000) SELECT i, printf('%050d', i) FROM c`,
			`COMMIT`}, 40000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.musq")
			w, err := Create(path)
			if err != nil {
				t.Fatal(err)
			}
			w.DB.SetCommitsEachStatement(true)
			run := func(s string, commit bool) {
				t.Helper()
				if err := w.Exec(s); err != nil {
					t.Fatalf("%q: %v", s, err)
				}
				if commit {
					if _, err := w.Commit(); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, s := range []string{`CREATE TABLE big(id INTEGER PRIMARY KEY, v TEXT)`,
				`INSERT INTO big WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 100) SELECT i, 'v' || i FROM c`,
				`CREATE TABLE q(id INTEGER PRIMARY KEY, v TEXT)`} {
				run(s, true)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.load {
				run(s, false)
			}
			if w.DB.findTableMeta("q").bulk == nil {
				t.Fatal("no load was armed, so this tests nothing")
			}
			if _, err := w.Commit(); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if rewrote := !bytes.Equal(before, after); rewrote != tc.rewrite {
				t.Errorf("the commit rewrote the segment file: %v, want %v", rewrote, tc.rewrite)
			}
			count := func(n *Session) string {
				t.Helper()
				_, rows, err := n.Query(`SELECT count(*), coalesce(sum(id), 0), min(v) FROM q`, nil)
				if err != nil {
					t.Fatal(err)
				}
				var vs []string
				for _, v := range rows[0] {
					if v.Typ == Text {
						vs = append(vs, string(v.S))
						continue
					}
					vs = append(vs, fmt.Sprint(v.I))
				}
				return strings.Join(vs, "|")
			}
			want := count(w)
			if !strings.HasPrefix(want, fmt.Sprint(tc.rows, "|")) {
				t.Fatalf("the writer reads %q, want %d rows", want, tc.rows)
			}
			r, err := OpenWrite(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := count(r); got != want {
				t.Errorf("a second session read %q, want %q", got, want)
			}
			r.Discard()
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			f, err := OpenWrite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Discard()
			if got := count(f); got != want {
				t.Errorf("reopened, the file holds %q, want %q", got, want)
			}
		})
	}
}
