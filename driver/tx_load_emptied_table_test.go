package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestTxLoadIntoATableTheTransactionEmptied: one transaction deletes every
// committed row of a table and then inserts, which used to arm a direct-path
// load on the emptied store. The commit appended only the deletes: a second
// handle on the file read the table empty while the writer was open, and a
// fresh handle still did after it closed. Each case runs as it now does, the
// inserts in the change log, and with the load forced, which the commit must
// write by rewriting the file.
func TestTxLoadIntoATableTheTransactionEmptied(t *testing.T) {
	cases := []struct {
		name   string
		schema []string
		seed   []string
		tx     []string
		tables []string
		want   string
	}{
		{"delete all, same explicit rowid", []string{`CREATE TABLE back(x)`},
			[]string{`INSERT INTO back(_rowid_, x) VALUES(5, 1)`},
			[]string{`DELETE FROM back`, `INSERT INTO back(_rowid_, x) VALUES(5, 2)`},
			[]string{"back"}, "back 5:2"},
		{"delete by rowid, another explicit rowid", []string{`CREATE TABLE back(x)`},
			[]string{`INSERT INTO back(_rowid_, x) VALUES(5, 1)`},
			[]string{`DELETE FROM back WHERE rowid = 5`, `INSERT INTO back(_rowid_, x) VALUES(6, 1)`},
			[]string{"back"}, "back 6:1"},
		{"delete by key, implicit rowid", []string{`CREATE TABLE back(k TEXT, x)`},
			[]string{`INSERT INTO back VALUES('a', 1)`},
			[]string{`DELETE FROM back WHERE k = 'a'`, `INSERT INTO back VALUES('a', 2)`},
			[]string{"back"}, "back 1:a,2"},
		{"several rows by INTEGER PRIMARY KEY", []string{`CREATE TABLE back(id INTEGER PRIMARY KEY, x)`},
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
	}
	read := func(t *testing.T, db *sql.DB, tables []string) string {
		t.Helper()
		var parts []string
		for _, tbl := range tables {
			rows, err := db.Query(`SELECT rowid, * FROM ` + tbl + ` ORDER BY rowid`)
			if err != nil {
				t.Fatal(err)
			}
			cols, _ := rows.Columns()
			var b strings.Builder
			b.WriteString(tbl)
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				var vs []string
				for _, v := range vals[1:] {
					if bs, ok := v.([]byte); ok {
						v = string(bs)
					}
					vs = append(vs, fmt.Sprint(v))
				}
				fmt.Fprintf(&b, " %v:%s", vals[0], strings.Join(vs, ","))
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			parts = append(parts, b.String())
		}
		return strings.Join(parts, " | ")
	}
	open := func(t *testing.T, path string) *sql.DB {
		t.Helper()
		db, err := sql.Open(DriverName, path)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	for _, c := range cases {
		for _, forced := range []bool{false, true} {
			name := c.name
			if forced {
				name += ", load forced"
			}
			t.Run(name, func(t *testing.T) {
				defer engine.SetTxLoadAfterWritesForTest(engine.SetTxLoadAfterWritesForTest(forced))
				path := filepath.Join(t.TempDir(), "x.musq")
				w := open(t, path)
				defer w.Close()
				for _, s := range append(append([]string(nil), c.schema...), c.seed...) {
					if _, err := w.Exec(s); err != nil {
						t.Fatalf("%q: %v", s, err)
					}
				}
				tx, err := w.Begin()
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range c.tx {
					if _, err := tx.Exec(s); err != nil {
						t.Fatalf("%q: %v", s, err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				r := open(t, path)
				if got := read(t, r, c.tables); got != c.want {
					t.Errorf("a second handle read %q while the writer was open, want %q", got, c.want)
				}
				r.Close()
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				f := open(t, path)
				defer f.Close()
				if got := read(t, f, c.tables); got != c.want {
					t.Errorf("reopened, the file holds %q, want %q", got, c.want)
				}
			})
		}
	}
}
