package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// rowidInLists are IN lists over the rowid: unsorted and repeated values,
// rowids that are missing (every multiple of 7) or negative, both ends of
// int64, and values of every class -- a TEXT or REAL that numeric affinity
// makes an integer, one it does not, NULL, a BLOB.
var rowidInLists = []string{
	`1, 2, 3`,
	`3, 1, 2, 1, 3`,
	`-50, -49, 0, 7, 14, 3000, 9999`,
	`'5', 5.0, 6.5, NULL, X'05', 'abc', '  8  ', '9e0', '10.0'`,
	`9223372036854775807, -9223372036854775808, 2999`,
	`NULL, NULL`,
	`7000, 5000, 1505, -1000, 2000, 2499`,
}

// rowidInLong is a list of 400 values, most of them rowids.
func rowidInLong() string {
	var b strings.Builder
	for i := range 400 {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d", (i*37)%3100-60)
	}
	return b.String()
}

// TestRowidInSeekMatchesScan: "rowid IN (...)" seeks each value
// (OpSeekRowidSetHint), so every form of it must return exactly the rows the
// same query compiled without the seek returns, over several segments, a delta
// beside them, and a session's own rows.
func TestRowidInSeekMatchesScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.musq")
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`}
	for i := -50; i <= 3000; i++ {
		if i%7 != 0 {
			stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, 's%d')`, i, i%13, i))
		}
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	execDB(t, path, `INSERT INTO t VALUES(7000, 1, 'x')`, `INSERT INTO t VALUES(5000, 2, 'y')`)
	execDB(t, path, `VACUUM`)
	clean, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()

	lists := append(append([]string{}, rowidInLists...), rowidInLong())
	forms := []string{
		`SELECT id, s FROM t WHERE id IN (%s)`,
		`SELECT id FROM t WHERE rowid IN (%s) AND k = 3`,
		`SELECT id FROM t WHERE k < 5 AND oid IN (%s) ORDER BY id DESC`,
		`SELECT id FROM t WHERE id IN (%s) LIMIT 2`,
		`SELECT count(*), sum(id) FROM t WHERE id IN (%s)`,
		`SELECT k, count(*) FROM t WHERE id IN (%s) GROUP BY k`,
		`SELECT a.id, b.id FROM t a JOIN t b ON b.id = a.id + 1 WHERE a.id IN (%s)`,
		`SELECT a.id, b.id FROM t a, t b WHERE b.id IN (%[1]s) AND a.id IN (%[1]s) AND a.k = b.k`,
		`SELECT id FROM t WHERE id NOT IN (%s) AND id < 20`,
		`SELECT (SELECT count(*) FROM t WHERE id IN (%s))`,
	}
	params := [][]Value{
		{{Typ: Int, I: 3}, {Typ: Int, I: -49}, {Typ: Int, I: 3}},
		{{Typ: Text, S: []byte("12")}, {Typ: Float, F: 13}, {Typ: Float, F: 13.5}, {Typ: Null}, {Typ: Blob, S: []byte("1")}},
		{},
	}
	check := func(t *testing.T, name string, query func(string, []Value) ([][]Value, error)) {
		t.Helper()
		run := func(q string, args []Value) {
			t.Helper()
			rowidInOffForTest = true
			want, werr := query(q+" -- reference", args)
			rowidInOffForTest = false
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
				if len(args) == 0 {
					continue
				}
				marks := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
				if strings.Contains(f, "%[1]s") {
					run(fmt.Sprintf(f, marks), append(append([]Value{}, args...), args...))
					continue
				}
				run(fmt.Sprintf(f, marks), args)
			}
		}
	}
	check(t, "segments", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := clean.QueryArgs(q, a)
		return rows, err
	})
	execDB(t, path, `INSERT INTO t VALUES(1505, 9, 'delta')`, `DELETE FROM t WHERE id BETWEEN 10 AND 20`, `UPDATE t SET id = -1000 WHERE id = 2000`)
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
	if err := n.Exec(`INSERT INTO t VALUES(2499, 1, 'mine')`); err != nil {
		t.Fatal(err)
	}
	if err := n.Exec(`DELETE FROM t WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	check(t, "session", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := n.Query(q, a)
		return rows, err
	})
}

// TestRowidInSeekWrites: an UPDATE or DELETE whose WHERE holds "rowid IN
// (...)" seeks each value, and must change the rows, in the order, the walk of
// the whole table does: the RETURNING rows and the table after it agree with
// the statement compiled without the seek.
func TestRowidInSeekWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.musq")
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`}
	for i := -50; i <= 3000; i++ {
		if i%7 != 0 {
			stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, 's%d')`, i, i%13, i))
		}
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	execDB(t, path, `INSERT INTO t VALUES(7000, 1, 'x')`, `DELETE FROM t WHERE id BETWEEN 10 AND 20`)

	lists := append(append([]string{}, rowidInLists...), rowidInLong())
	forms := []string{
		`DELETE FROM t WHERE id IN (%s) RETURNING id, s`,
		`DELETE FROM t WHERE rowid IN (%s) AND k > 3 RETURNING id`,
		`UPDATE t SET k = k + 100 WHERE id IN (%s) RETURNING id, k`,
		`UPDATE t SET id = id + 100000 WHERE id IN (%s) RETURNING id`,
		`UPDATE t SET s = s || '!' WHERE k <> 4 AND oid IN (%s) RETURNING rowid, s`,
	}
	run := func(q string, off bool) ([][]Value, [][]Value, error) {
		n, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		rowidInOffForTest = off
		defer func() { rowidInOffForTest = false }()
		_, ret, err := n.ExecReturningArgs(q, nil)
		if err != nil {
			return nil, nil, err
		}
		_, after, err := n.Query(`SELECT id, k, s FROM t ORDER BY id`, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Copied: the rows may read the session's mapping, gone with it.
		return copyRows(ret), copyRows(after), nil
	}
	for _, f := range forms {
		for _, l := range lists {
			q := fmt.Sprintf(f, l)
			wantRet, wantAfter, werr := run(q+" -- reference", true)
			gotRet, gotAfter, gerr := run(q, false)
			if (werr == nil) != (gerr == nil) {
				t.Fatalf("%s: error %v, reference %v", q, gerr, werr)
			}
			if !reflect.DeepEqual(gotRet, wantRet) {
				t.Fatalf("%s: RETURNING\n got %v\nwant %v", q, gotRet, wantRet)
			}
			if !reflect.DeepEqual(gotAfter, wantAfter) {
				t.Fatalf("%s: the table after differs", q)
			}
		}
	}
}

// TestRowidInSeekFires: the seek is in the program where the IN qualifies,
// and not where it does not.
func TestRowidInSeekFires(t *testing.T) {
	p := buildRowidSeekTestDB(t)
	fires := []string{
		"SELECT v FROM t WHERE id IN (1, 2, 3)",
		"SELECT v FROM t WHERE id IN (-100, -1, +2)",
		"SELECT v FROM t WHERE rowid IN (?, ?)",
		"SELECT v FROM t WHERE v > 0 AND id IN (1, 'x', NULL)",
	}
	for _, sql := range fires {
		d, err := DisassembleScan(p, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if !strings.Contains(d, "SeekRowidSetHint") {
			t.Errorf("expected SeekRowidSetHint for %q, got:\n%s", sql, d)
		}
	}
	noFire := []string{
		"SELECT v FROM t WHERE id NOT IN (1, 2)",
		"SELECT v FROM t WHERE id IN (1, v)",
		"SELECT v FROM t WHERE id IN (SELECT 1)",
		"SELECT v FROM t WHERE v IN (1, 2)",
		"SELECT v FROM t WHERE id IN (1, 2) OR v = 3",
		"SELECT v FROM t WHERE id = 1 AND id IN (1, 2)",
	}
	for _, sql := range noFire {
		d, err := DisassembleScan(p, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if strings.Contains(d, "SeekRowidSetHint") {
			t.Errorf("did NOT expect SeekRowidSetHint for %q, got:\n%s", sql, d)
		}
	}
}

// copyRows is rows with every TEXT and BLOB copied.
func copyRows(rows [][]Value) [][]Value {
	out := make([][]Value, len(rows))
	for i, r := range rows {
		out[i] = make([]Value, len(r))
		for j, v := range r {
			out[i][j] = copyValue(v)
		}
	}
	return out
}
