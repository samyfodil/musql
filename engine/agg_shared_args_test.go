package engine_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestAggIdenticalInputsEvaluateOnce: aggregates whose FILTER, argument and
// separator are spelled alike evaluate them once per row, as C merges
// identical calls into one AggInfo slot, and a different FILTER or argument
// keeps its own.
func TestAggIdenticalInputsEvaluateOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(g INTEGER, a INTEGER, b TEXT)`,
		`INSERT INTO t VALUES(1,1,'2'),(1,2,'3'),(1,3,NULL),(2,4,'1.5'),(2,1,'10')`,
	} {
		mustExec(t, db, s)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	const q = `SELECT g, sum(a*b), avg(a*b), sum(a*b) FILTER (WHERE a>1), count(a*b) FILTER (WHERE a>1),
		total(a*b) FILTER (WHERE a>1), sum(a*b+0) FROM t GROUP BY g HAVING sum(a*b) > 0 ORDER BY sum(a*b) DESC`
	_, rows, err := p.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, r := range rows {
		for _, v := range r {
			got += fmt.Sprintf("%v:%v ", v.Typ, v)
		}
		got += "\n"
	}
	want := ""
	i := func(n int64) engine.Value { return engine.Value{Typ: engine.Int, I: n} }
	f := func(x float64) engine.Value { return engine.Value{Typ: engine.Float, F: x} }
	for _, r := range [][]engine.Value{
		{i(2), f(16), f(8), f(6), i(1), f(6), f(16)},
		{i(1), i(8), f(4), i(6), i(1), f(6), i(8)},
	} {
		for _, v := range r {
			want += fmt.Sprintf("%v:%v ", v.Typ, v)
		}
		want += "\n"
	}
	if got != want {
		t.Fatalf("got\n%swant\n%s", got, want)
	}

	_, ops, err := p.Query(`EXPLAIN ` + q)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range ops {
		if string(r[1].S) == "Multiply" {
			n++
		}
	}
	// a*b unfiltered, a*b under the FILTER, and the one inside a*b+0.
	if n != 3 {
		t.Fatalf("%d Multiply opcodes, want 3", n)
	}
}
