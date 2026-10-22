package engine

import (
	"path/filepath"
	"reflect"
	"testing"
)

// TestSegSelectWindowsMatchScan holds the windowed pre-filter to the scan it
// shortens: a segment of 10,000 rows is judged a window at a time, so results
// that end inside a window, straddle one, or stop early at a LIMIT must all be
// the rows a plain scan returns, in the same order. Column n is NULL on every
// third row, which the kernel cannot judge, so each window also merges rows it
// passes through unjudged.
func TestSegSelectWindowsMatchScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.musq")
	buildDB(t, path, append([]string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, n INTEGER, s TEXT)`,
	}, segRowWrites(1, 10000)...)...)
	execDB(t, path, `VACUUM`)
	rp, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	type query struct {
		sql  string
		args []Value
	}
	ints := func(xs ...int) []Value {
		out := make([]Value, len(xs))
		for i, x := range xs {
			out[i] = Value{Typ: Int, I: int64(x)}
		}
		return out
	}
	var queries []query
	for _, b := range []int{0, 1000, 1023, 1024, 1025, 3071, 3072, 9990, 10000} {
		queries = append(queries,
			query{`SELECT id, s FROM t WHERE id > ? LIMIT 7`, ints(b)},
			query{`SELECT id FROM t WHERE id >= ? AND id < ?`, ints(b, b+2100)},
			query{`SELECT id, n FROM t WHERE n > ? LIMIT 50`, ints(b * 7)},
			query{`SELECT id FROM t WHERE n <= ? OR k = ?`, ints(b*7, 3)},
			query{`SELECT count(*) FROM (SELECT id FROM t WHERE v > ? LIMIT 3000)`, ints(b * 90)},
		)
	}
	selected := int64(0)
	for _, qa := range queries {
		q := qa.sql
		// Its own text, so the plan cache cannot hand either run the other's
		// program.
		segPeepholesOffForTest = true
		_, want, werr := rp.QueryArgs(q+" -- reference", qa.args)
		segPeepholesOffForTest = false
		SegRowsSelectedForTest()
		_, got, gerr := rp.QueryArgs(q, qa.args)
		selected += SegRowsSelectedForTest()
		if werr != nil || gerr != nil {
			t.Fatalf("%s %v: %v / %v", q, qa.args, werr, gerr)
		}
		if (len(got) > 0 || len(want) > 0) && !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %v: got %d rows, want %d\n got %v\nwant %v", q, qa.args, len(got), len(want), got, want)
		}
	}
	if selected == 0 {
		t.Fatal("no query took the pre-filter, so nothing was tested")
	}
}
