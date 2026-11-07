package engine

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRowidRangeHintMatchesScan: a rowid bound narrows the scan
// (OpSeekRowidRangeHint), so every form of it must return exactly the rows
// the same query compiled without it returns -- for bounds of every storage class
// and at both ends of int64, over several segments, negative rowids, and a
// delta beside them.
func TestRowidRangeHintMatchesScan(t *testing.T) {
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
	execDB(t, path, `VACUUM`) // a second segment layout
	clean, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()

	bounds := []Value{
		{Typ: Int, I: 0}, {Typ: Int, I: -51}, {Typ: Int, I: -50}, {Typ: Int, I: 1024}, {Typ: Int, I: 2999},
		{Typ: Int, I: 3000}, {Typ: Int, I: 5000}, {Typ: Int, I: 9000},
		{Typ: Int, I: math.MaxInt64}, {Typ: Int, I: math.MinInt64},
		{Typ: Float, F: 1500.5}, {Typ: Float, F: -0.5}, {Typ: Text, S: []byte("1500")}, {Typ: Text, S: []byte("abc")},
		{Typ: Null}, {Typ: Blob, S: []byte{1}},
	}
	forms := []string{
		`SELECT id, s FROM t WHERE id > ?`,
		`SELECT id FROM t WHERE id >= ? LIMIT 5`,
		`SELECT id FROM t WHERE id < ?`,
		`SELECT id FROM t WHERE id <= ? AND k = 3`,
		`SELECT id FROM t WHERE ? < id LIMIT 9`,
		`SELECT id FROM t WHERE ? >= rowid`,
		`SELECT id FROM t WHERE id > ? AND id < 2000`,
		`SELECT id FROM t WHERE id > ? ORDER BY id DESC LIMIT 3`,
		`SELECT count(*), sum(id) FROM (SELECT id FROM t WHERE id >= ?)`,
		// A join's levels each take their own bound.
		`SELECT a.id, b.s FROM t a, t b WHERE a.id < 30 AND b.id > ? AND b.id < 2950 AND b.k = a.k`,
		`SELECT count(*), sum(b.id), group_concat(a.id) FROM t a JOIN t b ON b.k = a.k WHERE a.id <= ? AND a.id > 2900 AND b.id < 25`,
		`SELECT count(*), max(b.id) FROM t a, t b WHERE a.id BETWEEN 0 AND 5 AND b.id >= ? AND b.id <= 2990`,
		`SELECT a.id, b.id FROM t a LEFT JOIN t b ON b.k = a.k AND b.id > 2990 WHERE a.id < 15 AND b.id <= ?`,
		`SELECT count(*) FROM (SELECT k FROM t WHERE id <= ?) a, (SELECT k FROM t WHERE id <= 40) b WHERE a.k = b.k`,
	}
	check := func(t *testing.T, name string, query func(string, []Value) ([][]Value, error)) {
		for _, f := range forms {
			for _, b := range bounds {
				args := []Value{b}
				// Its own text, so the plan cache cannot hand either run the
				// other's program.
				rowidRangeOffForTest = true
				want, werr := query(f+" -- reference", args)
				rowidRangeOffForTest = false
				got, gerr := query(f, args)
				if (werr == nil) != (gerr == nil) {
					t.Fatalf("%s %s %v: error %v, reference %v", name, f, b, gerr, werr)
				}
				if (len(got) > 0 || len(want) > 0) && !reflect.DeepEqual(got, want) {
					t.Fatalf("%s %s %v:\n got %v\nwant %v", name, f, b, got, want)
				}
			}
		}
	}
	// BETWEEN is no bound.
	for i, want := range []int{2, 2, 1} {
		f := forms[9+i]
		if d, err := DisassembleScan(clean, f); err != nil || strings.Count(d, "SeekRowidRangeHint") != want {
			t.Fatalf("%s: want %d bounds (%v):\n%s", f, want, err, d)
		}
	}
	check(t, "segments", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := clean.QueryArgs(q, a)
		return rows, err
	})
	// A delta beside the segments, and a session's own uncommitted rows.
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
	check(t, "session", func(q string, a []Value) ([][]Value, error) {
		_, rows, err := n.Query(q, a)
		return rows, err
	})
}
