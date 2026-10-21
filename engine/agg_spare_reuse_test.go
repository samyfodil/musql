package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// TestAggSpareReuseKeepsAnswers: a pooled machine rebuilds the accumulator set
// its last execution left (reuseAggAccs), and machines pass between programs,
// so aggregates of every shape run back to back must keep the answers they
// gave on a cold start.
func TestAggSpareReuseKeepsAnswers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.musq")
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, g INTEGER, v INTEGER, s TEXT)`}
	for i := 1; i <= 300; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %d, 's%d')`, i, i%7, (i*37)%101, i%11))
	}
	buildDB(t, path, stmts...)
	rp, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	queries := []string{
		`SELECT count(*) FROM t WHERE v > ?`,
		`SELECT sum(v), count(DISTINCT s), max(v) FROM t WHERE g = ?`,
		`SELECT group_concat(s, '-') FROM t WHERE id < ? AND g = 3`,
		`SELECT min(v), id, s FROM t WHERE g <> ?`,
		`SELECT g, count(*), sum(v) FROM t WHERE v > ? GROUP BY g HAVING count(*) > 2`,
		`SELECT avg(v), total(v) FROM t WHERE id > ?`,
		`SELECT count(*) FROM t WHERE v > ? HAVING max(v) > 50`,
	}
	args := func(k int) []Value { return []Value{{Typ: Int, I: int64(k)}} }
	want := map[string][][]Value{}
	for _, q := range queries {
		for k := range 6 {
			// A reference compiled under its own text, so it runs on a cold
			// set before the loop below reuses any.
			_, rows, err := rp.QueryArgs(q+fmt.Sprintf(" -- ref %d", k), args(k*20))
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			want[fmt.Sprint(q, k)] = rows
		}
	}
	for round := range 5 {
		for i := range queries {
			q := queries[(i+round)%len(queries)]
			for k := range 6 {
				_, got, err := rp.QueryArgs(q, args(k*20))
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				if w := want[fmt.Sprint(q, k)]; (len(got) > 0 || len(w) > 0) && !reflect.DeepEqual(got, w) {
					t.Fatalf("round %d %s [%d]:\n got %v\nwant %v", round, q, k*20, got, w)
				}
			}
		}
	}
}
