package engine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTxLoadIntoATableTheTransactionEmptied: a transaction that deletes every
// committed row of a table and then inserts used to arm a direct-path load on
// the emptied store (insert_bulk_direct.go), whose rows only the file rewrite
// writes. The change log described the table through its deletes, so the
// commit appended them alone and the new rows were lost: a second session
// read none of them, and neither did the file once the writer closed.
//
// The inserts now take the log and the commit stays a delta append, leaving
// the segment file as it was. Forced to arm the load anyway, the commit must
// rewrite the file, and the rows must survive that way too.
func TestTxLoadIntoATableTheTransactionEmptied(t *testing.T) {
	type tc struct {
		name   string
		schema []string
		seed   []string
		tx     []string
		tables []string
		want   string // "rowid:values" per row of every table, in rowid order
	}
	cases := []tc{
		{"delete all, same rowid", []string{`CREATE TABLE back(x)`},
			[]string{`INSERT INTO back(_rowid_, x) VALUES(5, 1)`},
			[]string{`DELETE FROM back`, `INSERT INTO back(_rowid_, x) VALUES(5, 2)`},
			[]string{"back"}, "back 5:2"},
		{"delete by rowid, other rowid", []string{`CREATE TABLE back(x)`},
			[]string{`INSERT INTO back(_rowid_, x) VALUES(5, 1)`},
			[]string{`DELETE FROM back WHERE rowid = 5`, `INSERT INTO back(_rowid_, x) VALUES(6, 1)`},
			[]string{"back"}, "back 6:1"},
		{"delete by key, implicit rowid", []string{`CREATE TABLE back(k, x)`},
			[]string{`INSERT INTO back VALUES('a', 1)`},
			[]string{`DELETE FROM back WHERE k = 'a'`, `INSERT INTO back VALUES('a', 2)`},
			[]string{"back"}, "back 1:a,2"},
		{"several rows, integer primary key", []string{`CREATE TABLE back(id INTEGER PRIMARY KEY, x)`},
			[]string{`INSERT INTO back VALUES(1, 1), (2, 2), (3, 3)`},
			[]string{`DELETE FROM back`, `INSERT INTO back VALUES(2, 20)`, `INSERT INTO back VALUES(7, 70)`, `INSERT INTO back VALUES(9, 90)`},
			[]string{"back"}, "back 2:2,20 7:7,70 9:9,90"},
		{"several rows, implicit rowids", []string{`CREATE TABLE back(x)`},
			[]string{`INSERT INTO back VALUES(1), (2), (3)`},
			[]string{`DELETE FROM back WHERE x > 0`, `INSERT INTO back VALUES(4)`, `INSERT INTO back VALUES(5)`},
			[]string{"back"}, "back 1:4 2:5"},
		{"several tables", []string{`CREATE TABLE back(x)`, `CREATE TABLE side(y)`},
			[]string{`INSERT INTO back(_rowid_, x) VALUES(5, 1)`, `INSERT INTO side VALUES('s')`},
			[]string{`DELETE FROM back`, `DELETE FROM side`, `INSERT INTO back(_rowid_, x) VALUES(5, 2)`, `INSERT INTO side VALUES('t')`},
			[]string{"back", "side"}, "back 5:2 | side 1:t"},
		{"inserted, deleted and inserted again in the transaction", []string{`CREATE TABLE back(x)`},
			nil,
			[]string{`INSERT INTO back VALUES(1)`, `DELETE FROM back`, `INSERT INTO back VALUES(2)`},
			[]string{"back"}, "back 1:2"},
	}
	read := func(t *testing.T, n *Session, tables []string) string {
		t.Helper()
		var parts []string
		for _, tbl := range tables {
			_, rows, err := n.Query(`SELECT rowid, * FROM `+tbl+` ORDER BY rowid`, nil)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			b.WriteString(tbl)
			for _, r := range rows {
				var vs []string
				for _, v := range r[1:] {
					switch v.Typ {
					case Int:
						vs = append(vs, fmt.Sprint(v.I))
					case Text:
						vs = append(vs, string(v.S))
					default:
						vs = append(vs, fmt.Sprintf("%v", v))
					}
				}
				fmt.Fprintf(&b, " %d:%s", r[0].I, strings.Join(vs, ","))
			}
			parts = append(parts, b.String())
		}
		return strings.Join(parts, " | ")
	}
	run := func(t *testing.T, c tc, forced bool) {
		defer SetTxLoadAfterWritesForTest(SetTxLoadAfterWritesForTest(forced))
		// A load this small would be drained into the change log at COMMIT
		// (drainSmallLoads); with compaction off it is not, and the commit
		// has to take the rewrite.
		compactionOffForTest = forced
		defer func() { compactionOffForTest = false }()
		path := filepath.Join(t.TempDir(), "x.musq")
		w, err := Create(path)
		if err != nil {
			t.Fatal(err)
		}
		w.DB.SetCommitsEachStatement(true)
		for _, s := range append(append([]string(nil), c.schema...), c.seed...) {
			if err := w.Exec(s); err != nil {
				t.Fatalf("%q: %v", s, err)
			}
			if _, err := w.Commit(); err != nil {
				t.Fatalf("commit %q: %v", s, err)
			}
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range append([]string{`BEGIN`}, c.tx...) {
			if err := w.Exec(s); err != nil {
				t.Fatalf("%q: %v", s, err)
			}
		}
		armed := false
		for _, name := range c.tables {
			armed = armed || w.DB.findTableMeta(name).bulk != nil
		}
		if armed != forced {
			t.Errorf("a load is armed: %v, want %v", armed, forced)
		}
		if err := w.Exec(`COMMIT`); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Commit(); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if rewrote := !bytes.Equal(before, after); rewrote != forced {
			t.Errorf("the commit rewrote the segment file: %v, want %v", rewrote, forced)
		}
		r, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, r, c.tables); got != c.want {
			t.Errorf("a second session read %q, want %q", got, c.want)
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
		if got := read(t, f, c.tables); got != c.want {
			t.Errorf("reopened, the file holds %q, want %q", got, c.want)
		}
	}
	for _, c := range cases {
		for _, forced := range []bool{false, true} {
			name := c.name
			if forced {
				name += ", load forced"
			}
			t.Run(name, func(t *testing.T) { run(t, c, forced) })
		}
	}
}
