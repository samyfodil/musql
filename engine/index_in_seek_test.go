package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestIndexInSeekMatchesScan: "col IN (list)" seeks each value and answers as
// with no seek, over segments, a delta and a session's rows.
func TestIndexInSeekMatchesScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	stmts := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, g INTEGER, a INTEGER, s TEXT COLLATE NOCASE, x)`,
		`CREATE INDEX ta ON t(a)`,
		`CREATE INDEX tgs ON t(g, s)`,
	}
	words := []string{"a", "B", "c", "D", "e"}
	for i := -40; i <= 2000; i++ {
		x := fmt.Sprintf("%d", i%17)
		if i%5 == 0 {
			x = fmt.Sprintf("'%d'", i%17)
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %d, '%s%d', %s)`, i, i%3, i%97, words[(i+40)%5], i%11, x))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	clean, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()

	lists := []string{
		`1, 2, 3`,
		`5, 1, 5, 2, 1`,
		`-1, 0, 96, 97, 1000`,
		`'1', 2.0, 2.5, NULL, X'01', 'x'`,
		`NULL, NULL`,
		`'a1', 'B2', 'c3 ', 'zz', 'A6'`,
	}
	forms := []string{
		`SELECT id, a FROM t WHERE a IN (%s)`,
		`SELECT id FROM t WHERE g = 1 AND a IN (%s)`,
		`SELECT id FROM t WHERE s IN (%s) AND g = 2`,
		`SELECT count(*), sum(id) FROM t WHERE x IN (%s)`,
		`SELECT id FROM t WHERE a IN (%s) ORDER BY a DESC, id`,
		`SELECT a, count(*) FROM t WHERE a IN (%s) GROUP BY a`,
		`SELECT t1.id, t2.id FROM t t1 JOIN t t2 ON t2.id = t1.id + 1 WHERE t1.a IN (%s)`,
	}
	params := [][]Value{
		{{Typ: Int, I: 3}, {Typ: Int, I: 50}, {Typ: Text, S: []byte("7")}},
		{{Typ: Text, S: []byte("a1")}, {Typ: Text, S: []byte("D3")}, {Typ: Null}, {Typ: Float, F: 4}},
	}
	check := func(t *testing.T, name string, query func(string, []Value) ([][]Value, error)) {
		t.Helper()
		run := func(q string, args []Value) {
			t.Helper()
			indexSeeksOffForTest = true
			want, werr := query(q+" -- reference", args)
			indexSeeksOffForTest = false
			got, gerr := query(q, args)
			if (werr == nil) != (gerr == nil) {
				t.Fatalf("%s %s: error %v, reference %v", name, q, gerr, werr)
			}
			if (len(got) > 0 || len(want) > 0) && !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s:\n got %v\nwant %v", name, q, got, want)
			}
		}
		for _, f := range forms {
			for _, l := range lists {
				run(fmt.Sprintf(f, l), nil)
			}
			for _, args := range params {
				run(fmt.Sprintf(f, strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")), args)
			}
		}
	}
	check(t, "segments", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := clean.QueryArgs(q, a)
		return rows, err
	})
	execDB(t, path, `INSERT INTO t VALUES(5000, 1, 2, 'B2', 3)`, `UPDATE t SET a = 3 WHERE id % 13 = 0`, `DELETE FROM t WHERE id % 7 = 0`)
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
	if err := n.Exec(`UPDATE t SET a = 1, s = 'C3' WHERE id % 11 = 0`); err != nil {
		t.Fatal(err)
	}
	check(t, "session", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := n.Query(q, a)
		return rows, err
	})
}

// TestIndexInSeekWrites: an UPDATE's and a DELETE's "col IN (list)" seek each
// value, and change the rows the walk of the table does, in its order.
func TestIndexInSeekWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.musq")
	stmts := []string{
		`CREATE TABLE c(tbl TEXT, pk BLOB, top INTEGER, PRIMARY KEY(tbl, pk))`,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, s TEXT COLLATE NOCASE)`,
		`CREATE INDEX ta ON t(a)`,
	}
	for i := -30; i <= 900; i++ {
		stmts = append(stmts,
			fmt.Sprintf(`INSERT INTO c VALUES('%c', X'%04x', %d)`, 'a'+rune((i+30)%3), i+30, i),
			fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, '%c%d')`, i, i%41, 'a'+rune((i+30)%4), i%5))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	execDB(t, path, `INSERT INTO t VALUES(-100, 3, 'B1')`, `DELETE FROM t WHERE id % 9 = 0`)
	forms := []string{
		`DELETE FROM t WHERE a IN (3, 1, 2, '4', 5.0, NULL) RETURNING id, a`,
		`UPDATE t SET a = a + 100 WHERE a IN (6, 7, 8) AND id > 0 RETURNING id, a`,
		`UPDATE t SET s = s || '!' WHERE s IN ('a1', 'B2', 'c3 ', 'Z') RETURNING id, s`,
		`DELETE FROM c WHERE tbl = 'b' AND pk IN (X'0001', X'0004', X'0007', X'0100', X'ffff') RETURNING hex(pk), top`,
		`UPDATE c SET top = -top WHERE tbl = 'a' AND pk IN (X'0000', X'0003', 'x', NULL) RETURNING hex(pk), top`,
	}
	run := func(q string, off bool) ([][]Value, [][]Value, error) {
		n, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		indexSeeksOffForTest, writeIndexSeekOffForTest = off, off
		defer func() { indexSeeksOffForTest, writeIndexSeekOffForTest = false, false }()
		_, ret, err := n.ExecReturningArgs(q, nil)
		if err != nil {
			return nil, nil, err
		}
		_, after, err := n.Query(`SELECT id, a, s FROM t ORDER BY id`, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, after2, err := n.Query(`SELECT tbl, hex(pk), top FROM c ORDER BY tbl, pk`, nil)
		if err != nil {
			t.Fatal(err)
		}
		return copyRows(ret), copyRows(append(after, after2...)), nil
	}
	for _, q := range forms {
		wantRet, wantAfter, werr := run(q+" -- reference", true)
		gotRet, gotAfter, gerr := run(q, false)
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%s: error %v, reference %v", q, gerr, werr)
		}
		if !reflect.DeepEqual(gotRet, wantRet) {
			t.Fatalf("%s: RETURNING\n got %v\nwant %v", q, gotRet, wantRet)
		}
		if !reflect.DeepEqual(gotAfter, wantAfter) {
			t.Fatalf("%s: the tables after differ", q)
		}
	}
}

// TestIndexInSeekFires: the seek carries the list where the IN qualifies.
func TestIndexInSeekFires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.musq")
	buildDB(t, path, `CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b)`, `CREATE INDEX ta ON t(a)`,
		`INSERT INTO t VALUES(1, 1, 1), (2, 2, 2)`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, sql := range []string{
		"SELECT b FROM t WHERE a IN (1, 2, 3)",
		"SELECT b FROM t WHERE b > 0 AND a IN (?, ?, -1)",
	} {
		d, err := DisassembleScan(p, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if !strings.Contains(d, "SeekIndexHint") {
			t.Errorf("expected SeekIndexHint for %q, got:\n%s", sql, d)
		}
	}
}
