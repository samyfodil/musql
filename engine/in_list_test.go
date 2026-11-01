package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// inListLists cover every class and shape the set keys.
var inListLists = []string{
	`1, 2, 3`,
	`3, 1, 2, 1, 3`,
	`1, '2', 3.0, NULL`,
	`-1, -2, +3, - -4, 4.5, '6'`,
	`'a', 'B', 'c ', 'd  ', 'e'`,
	`'A', 'b', 'C', 'x' || char(0) || 'y', 'Z'`,
	`X'01', X'02', 'x', 1`,
	`NULL, NULL, NULL`,
	`'1', '2.0', '3e0', ' 4', '5x'`,
	`9223372036854775807, -9223372036854775808, 9.223372036854775807e18, 0.5`,
}

func inListLong() string {
	var b strings.Builder
	for i := range 150 {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d", i*7-30)
	}
	return b.String()
}

// TestInListMatchesChain: the set answers as the comparison chain for every
// affinity, collation, list and NOT IN, through the program JIT over segments
// and the VDBE over the log.
func TestInListMatchesChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.musq")
	stmts := []string{`CREATE TABLE t(i INTEGER, r REAL, s TEXT, n TEXT COLLATE NOCASE, rt TEXT COLLATE RTRIM, b BLOB, x)`}
	vals := []string{`1`, `2`, `3`, `-1`, `-4`, `4.5`, `6`, `'6'`, `'a'`, `'A'`, `'c'`, `'c '`, `'d'`, `'x' || char(0) || 'z'`,
		`X'01'`, `X'02'`, `NULL`, `2.0`, `'2'`, `' 4'`, `9223372036854775807`, `-9223372036854775808`, `0.5`, `'b'`, `'B '`}
	for k := range 600 {
		v := vals[k%len(vals)]
		w := vals[(k*7+3)%len(vals)]
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %s, %s, %s, %s, %s, %s)`, k%40-10, v, w, v, w, v, w))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	clean, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()

	lists := append(append([]string{}, inListLists...), inListLong())
	cols := []string{"i", "r", "s", "n", "rt", "b", "x", "i + 1", "s COLLATE NOCASE", "CAST(x AS TEXT)"}
	forms := []string{
		`SELECT rowid FROM t WHERE %s IN (%s)`,
		`SELECT rowid FROM t WHERE %s NOT IN (%s)`,
		`SELECT rowid, %s IN (%s) FROM t`,
		`SELECT count(*) FROM t WHERE %s IN (%s)`,
		`SELECT sum(i) FROM t WHERE %s NOT IN (%s)`,
	}
	params := [][]Value{
		{{Typ: Int, I: 3}, {Typ: Int, I: -1}, {Typ: Int, I: 6}},
		{{Typ: Text, S: []byte("2")}, {Typ: Float, F: 4.5}, {Typ: Null}, {Typ: Blob, S: []byte{1}}},
		{{Typ: Text, S: []byte("A")}, {Typ: Text, S: []byte("c")}, {Typ: Text, S: []byte("b ")}},
		{{Typ: Int, I: 1}, {Typ: Int, I: 2}, {Typ: Int, I: 3}, {Typ: Int, I: 40}},
	}
	check := func(t *testing.T, name string, query func(string, []Value) ([][]Value, error)) {
		t.Helper()
		run := func(q string, args []Value) {
			t.Helper()
			inListOffForTest = true
			want, werr := query(q+" -- reference", args)
			inListOffForTest = false
			got, gerr := query(q, args)
			if (werr == nil) != (gerr == nil) {
				t.Fatalf("%s %s %v: error %v, reference %v", name, q, args, gerr, werr)
			}
			if (len(got) > 0 || len(want) > 0) && !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s %v:\n got %v\nwant %v", name, q, args, got, want)
			}
		}
		for _, f := range forms {
			for _, c := range cols {
				for _, l := range lists {
					run(fmt.Sprintf(f, c, l), nil)
				}
				for _, args := range params {
					marks := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
					run(fmt.Sprintf(f, c, marks), args)
					run(fmt.Sprintf(f, c, "1, "+marks+", 'a'"), args)
				}
			}
		}
	}
	check(t, "segments", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := clean.QueryArgs(q, a)
		return rows, err
	})
	execDB(t, path, `INSERT INTO t VALUES(5, 'A', 'b ', 'C', 'c   ', X'01', 3.0)`, `DELETE FROM t WHERE rowid % 9 = 0`)
	dirty, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dirty.Close()
	check(t, "delta", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := dirty.QueryArgs(q, a)
		return rows, err
	})
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if err := n.Exec(`UPDATE t SET s = upper(s) WHERE rowid % 5 = 1`); err != nil {
		t.Fatal(err)
	}
	check(t, "session", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := n.Query(q, a)
		return rows, err
	})
}

// TestInListWrites: an UPDATE's and a DELETE's WHERE probe the set too.
func TestInListWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.musq")
	stmts := []string{`CREATE TABLE t(i INTEGER, s TEXT COLLATE NOCASE)`}
	for k := range 400 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, '%c')`, k%30, 'a'+rune(k%26)))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	for _, q := range []string{
		`UPDATE t SET i = i + 100 WHERE i IN (1, 2, 3, '4', 5.0) RETURNING rowid, i`,
		`DELETE FROM t WHERE s IN ('A', 'b', 'C ', 'd') RETURNING rowid, s`,
		`DELETE FROM t WHERE i NOT IN (` + inListLong() + `) RETURNING rowid`,
	} {
		var res [2][][]Value
		for k, off := range []bool{true, false} {
			n, err := OpenWrite(path)
			if err != nil {
				t.Fatal(err)
			}
			inListOffForTest = off
			sfx := ""
			if off {
				sfx = " -- reference"
			}
			_, rows, err := n.ExecReturningArgs(q+sfx, nil)
			inListOffForTest = false
			if err != nil {
				t.Fatal(err)
			}
			_, after, err := n.Query(`SELECT rowid, i, s FROM t ORDER BY rowid`, nil)
			if err != nil {
				t.Fatal(err)
			}
			res[k] = copyRows(append(rows, after...))
			n.Discard()
		}
		if !reflect.DeepEqual(res[0], res[1]) {
			t.Fatalf("%s: the set and the chain disagree", q)
		}
	}
}
