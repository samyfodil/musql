package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestWriteIndexSeekMatchesTheWalk runs the same writes with the column seek on
// and off, on a held session (the row store the driver keeps), and compares
// every statement's change count and the whole table after it.
func TestWriteIndexSeekMatchesTheWalk(t *testing.T) {
	setup := []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, s TEXT, m, r REAL)`,
		`CREATE INDEX t_sec ON t(sec)`,
	}
	for i := 1; i <= 400; i++ {
		sec := fmt.Sprint(i % 37)
		if i%23 == 0 {
			sec = "NULL"
		}
		m := fmt.Sprint(i % 5)
		switch i % 7 {
		case 0:
			m = "'3'"
		case 1:
			m = "3.0"
		}
		setup = append(setup, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %d, 'v%d', %s, %d.5)`, i, sec, i%4, i%9, m, i%6))
	}
	writes := []string{
		`UPDATE t SET k = k + 1 WHERE sec = 5`,
		`UPDATE t SET k = k + 10 WHERE sec = '7'`,    // TEXT key, INTEGER column: affinity makes it 7
		`UPDATE t SET sec = sec + 1 WHERE sec = 8`,   // moves rows between keys
		`UPDATE t SET k = 0 WHERE 9 = sec AND k > 1`, // key on the left, another conjunct
		`DELETE FROM t WHERE sec = 11`,
		`UPDATE t SET k = -1 WHERE m = 3`,      // a column holding TEXT and REAL: refused, walks
		`UPDATE t SET k = -2 WHERE r = 2.5`,    // REAL probe
		`UPDATE t SET k = -3 WHERE sec = NULL`, // matches nothing
		`INSERT INTO t(sec, k, s) VALUES (5, 100, 'new')`,
		`UPDATE t SET k = k * 2 WHERE sec = 5`,         // sees the inserted row
		`ALTER TABLE t ADD COLUMN z INTEGER DEFAULT 4`, // short rows read as 4
		`UPDATE t SET k = 77 WHERE z = 4 AND sec = 12`,
		`UPDATE t SET z = 9 WHERE sec = 13`,
		`UPDATE t SET k = 55 WHERE z = 4`, // short rows and stored 4s
		`UPDATE OR REPLACE t SET id = id + 1000 WHERE sec = 14`,
		`DELETE FROM t WHERE sec = 6 AND id % 2 = 0`,
	}
	dir := t.TempDir()
	seekPath, walkPath := filepath.Join(dir, "seek.musq"), filepath.Join(dir, "walk.musq")
	for _, p := range []string{seekPath, walkPath} {
		buildDB(t, p, setup...)
		execDB(t, p, "VACUUM")
	}
	seek, err := OpenWrite(seekPath)
	if err != nil {
		t.Fatal(err)
	}
	defer seek.Discard()
	walk, err := OpenWrite(walkPath)
	if err != nil {
		t.Fatal(err)
	}
	defer walk.Discard()
	dump := func(n *Session) string {
		_, rows, err := n.Query(`SELECT * FROM t ORDER BY id`, nil)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	served := writeIndexSeeksServed.Load()
	for _, w := range writes {
		gotN, _, gotErr := seek.ExecArgs(w, nil)
		writeIndexSeekOffForTest = true
		wantN, _, wantErr := walk.ExecArgs(w, nil)
		writeIndexSeekOffForTest = false
		if (gotErr == nil) != (wantErr == nil) || gotN != wantN {
			t.Fatalf("%s: seek changed %d (%v), walk %d (%v)", w, gotN, gotErr, wantN, wantErr)
		}
		if g, wt := dump(seek), dump(walk); g != wt {
			t.Fatalf("%s: tables differ\n seek %.300s\n walk %.300s", w, g, wt)
		}
	}
	if writeIndexSeeksServed.Load()-served < 8 {
		t.Fatalf("the column seek served %d writes: the comparison proves nothing", writeIndexSeeksServed.Load()-served)
	}
}

// TestWriteIndexSeekCompositeKeys: the same comparison over a composite TEXT
// and BLOB key and a NOCASE column, whose TEXT probes the indexes refuse.
func TestWriteIndexSeekCompositeKeys(t *testing.T) {
	setup := []string{
		`CREATE TABLE c (tbl TEXT, pk BLOB, col TEXT, v, PRIMARY KEY(tbl, pk, col))`,
		`CREATE TABLE n (a TEXT COLLATE NOCASE, b TEXT, v, PRIMARY KEY(a, b))`,
	}
	for i := 0; i < 900; i++ {
		tbl := "t"
		if i%10 == 0 {
			tbl = "u"
		}
		setup = append(setup, fmt.Sprintf(`INSERT INTO c VALUES ('%s', x'%04x', 'c%d', %d)`, tbl, i/3, i%3, i))
	}
	for i := 0; i < 300; i++ {
		key := "KEY"
		if i%2 == 1 {
			key = "key"
		}
		setup = append(setup, fmt.Sprintf(`INSERT INTO n VALUES ('%s%d', 'b%d', %d)`, key, i%10, i, i))
	}
	txt := func(s string) Value { return Value{Typ: Text, S: []byte(s)} }
	blob := func(b ...byte) Value { return Value{Typ: Blob, S: b} }
	writes := []struct {
		sql  string
		args []Value
	}{
		{`UPDATE c SET v = -v WHERE tbl = 't' AND pk = x'0001' AND col = 'c1'`, nil},
		{`UPDATE c SET v = v + 1000 WHERE tbl = ? AND pk = ? AND col = ?`, []Value{txt("t"), blob(0, 2), txt("c2")}},
		{`UPDATE c SET v = 7 WHERE tbl = ? AND pk = ?`, []Value{txt("t"), blob(0, 4)}},
		{`UPDATE c SET col = 'c9' WHERE pk = ? AND col = ? AND tbl = ?`, []Value{blob(0, 5), txt("c0"), txt("t")}},
		{`UPDATE c SET v = 8 WHERE tbl = ? AND pk = ?`, []Value{txt("t"), txt("\x00\x06")}}, // TEXT probe, BLOB column: nothing
		{`UPDATE c SET v = 9 WHERE tbl = ? AND pk = ?`, []Value{{Typ: Int, I: 1}, blob(0, 7)}},
		{`DELETE FROM c WHERE tbl = ? AND pk = ? AND col = ?`, []Value{txt("t"), blob(0, 8), txt("c1")}},
		{`INSERT INTO c VALUES ('t', x'0009', 'c7', 5)`, nil},
		{`UPDATE c SET v = 10 WHERE tbl = 't' AND pk = x'0009'`, nil}, // sees the inserted row
		{`UPDATE OR REPLACE c SET col = 'c1' WHERE tbl = ? AND pk = ? AND col = ?`, []Value{txt("t"), blob(0, 10), txt("c2")}},
		{`UPDATE c SET v = 11 WHERE tbl = 'u' AND col = 'c0'`, nil},
		{`UPDATE n SET v = -1 WHERE a = 'KEY3' AND b = 'b13'`, nil}, // NOCASE: the 'key3' rows too
		{`UPDATE n SET v = -2 WHERE a = ? AND b = ?`, []Value{txt("key4"), txt("b4")}},
		{`DELETE FROM n WHERE a = 'Key5' AND b = 'b5'`, nil},
		{`UPDATE n SET b = 'zz' WHERE b = 'b6'`, nil},
	}
	dir := t.TempDir()
	seekPath, walkPath := filepath.Join(dir, "seek.musq"), filepath.Join(dir, "walk.musq")
	for _, p := range []string{seekPath, walkPath} {
		buildDB(t, p, setup...)
		execDB(t, p, "VACUUM")
	}
	seek, err := OpenWrite(seekPath)
	if err != nil {
		t.Fatal(err)
	}
	defer seek.Discard()
	walk, err := OpenWrite(walkPath)
	if err != nil {
		t.Fatal(err)
	}
	defer walk.Discard()
	dump := func(n *Session) string {
		var out string
		for _, q := range []string{`SELECT * FROM c ORDER BY rowid`, `SELECT * FROM n ORDER BY rowid`} {
			_, rows, err := n.Query(q, nil)
			if err != nil {
				t.Fatal(err)
			}
			out += fmt.Sprint(typedRows(rows))
		}
		return out
	}
	served := writeIndexSeeksServed.Load()
	for _, w := range writes {
		gotN, _, gotErr := seek.ExecArgs(w.sql, w.args)
		writeIndexSeekOffForTest = true
		wantN, _, wantErr := walk.ExecArgs(w.sql, w.args)
		writeIndexSeekOffForTest = false
		if (gotErr == nil) != (wantErr == nil) || gotN != wantN {
			t.Fatalf("%s %v: seek changed %d (%v), walk %d (%v)", w.sql, w.args, gotN, gotErr, wantN, wantErr)
		}
		if g, wt := dump(seek), dump(walk); g != wt {
			t.Fatalf("%s %v: tables differ\n seek %.300s\n walk %.300s", w.sql, w.args, g, wt)
		}
	}
	if writeIndexSeeksServed.Load()-served < 8 {
		t.Fatalf("the column seek served %d writes: the comparison proves nothing", writeIndexSeeksServed.Load()-served)
	}
}
